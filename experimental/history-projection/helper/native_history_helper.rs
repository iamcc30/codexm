//! Pinned offline maintenance protocol. The caller owns the cooperative startup gate.
//! Native logical locks do not exclude an unmanaged fork's ancestor projection.
use std::collections::HashSet;
use std::io::{BufRead, Read, Write};
use std::os::unix::fs::{MetadataExt, PermissionsExt};
use std::path::{Component, Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use codex_protocol::ThreadId;
use codex_rollout::{WriterLockCoordinator, WriterLockGuard};
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use sqlx::sqlite::{SqliteConnectOptions, SqlitePoolOptions, SqliteSynchronous};
use sqlx::{Connection, SqliteConnection};

use super::publish_projection::{Advance, PublicationMode, Rollout, native_publication};

static STATE_MIGRATOR: sqlx::migrate::Migrator = sqlx_macros::migrate!("../state/migrations");
const ARTIFACT: &str = "native-artifact.json";
const MAX_REQUEST: u64 = 1024 * 1024;
const PIN: &str = "6ff1e1ca3b30fc8b914720681fcf5fe62e95f856";
type Schema = Vec<(String, String, String, Option<String>)>;
type Migrations = Vec<(i64, String, bool, Vec<u8>)>;

#[derive(Deserialize)]
#[serde(tag = "action", rename_all = "snake_case", deny_unknown_fields)]
enum Command {
    Acquire {
        profile_home: PathBuf,
        owner_ids: Vec<String>,
    },
    Inspect {
        thread_ids: Vec<String>,
    },
    Prepare {
        profile_home: PathBuf,
        work_dir: PathBuf,
        rollouts: Vec<Input>,
        tips: Vec<Tip>,
    },
    Publish {
        profile_home: PathBuf,
        work_dir: PathBuf,
        artifact_sha256: String,
    },
    Verify {
        profile_home: PathBuf,
        work_dir: PathBuf,
        artifact_sha256: String,
    },
    Release {},
}

#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Input {
    rollout_id: String,
    session_id: String,
    path: String,
    end_byte_offset: u64,
    end_ordinal_exclusive: u64,
    prefix_sha256: String,
}

#[derive(Clone, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Tip {
    thread_id: String,
    expected_path: String,
    new_path: String,
}

#[derive(Clone, Copy, Serialize, Deserialize, PartialEq, Eq)]
#[serde(deny_unknown_fields)]
struct Identity {
    device: u64,
    inode: u64,
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Base {
    end_byte_offset: i64,
    end_ordinal_exclusive: i64,
    prefix_sha256: String,
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct BoundInput {
    input: Input,
    base: Option<Base>,
}

#[derive(Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct Artifact {
    version: u32,
    upstream_commit: String,
    profile: Identity,
    state: Identity,
    history: Identity,
    stage_sha256: String,
    rollouts: Vec<BoundInput>,
    tips: Vec<Tip>,
}

struct Session {
    profile: PathBuf,
    owners: HashSet<String>,
    _guards: Vec<WriterLockGuard>,
}

fn err(value: impl std::fmt::Display) -> String {
    value.to_string()
}
fn digest(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn test_pause(stage: &str) {
    #[cfg(debug_assertions)]
    if std::env::var("CODEXM_NATIVE_HISTORY_TEST_PAUSE_AT")
        .ok()
        .as_deref()
        == Some(stage)
    {
        eprintln!("CODEXM_NATIVE_HISTORY_TEST_PAUSED:{stage}");
        let _ = std::io::stderr().flush();
        std::thread::sleep(Duration::from_secs(120));
    }
    let _ = stage;
}
fn canonical_id(value: &str) -> Result<ThreadId, String> {
    let id = ThreadId::from_string(value).map_err(err)?;
    if id.to_string() != value {
        return Err("canonical lowercase UUID required".into());
    }
    Ok(id)
}
fn identity(path: &Path) -> Result<Identity, String> {
    let stat = std::fs::metadata(path).map_err(err)?;
    Ok(Identity {
        device: stat.dev(),
        inode: stat.ino(),
    })
}
fn checked(path: &Path, missing: bool) -> Result<PathBuf, String> {
    if !path.is_absolute()
        || path
            .components()
            .any(|part| matches!(part, Component::CurDir | Component::ParentDir))
    {
        return Err("canonical absolute paths required".into());
    }
    let uid = std::fs::metadata("/proc/self").map_err(err)?.uid();
    let mut current = PathBuf::new();
    for part in path.components() {
        current.push(part.as_os_str());
        let stat = match std::fs::symlink_metadata(&current) {
            Ok(stat) => stat,
            Err(error) if missing && error.kind() == std::io::ErrorKind::NotFound => {
                return Ok(path.to_path_buf());
            }
            Err(error) => return Err(err(error)),
        };
        if stat.file_type().is_symlink()
            || (!stat.is_dir() && (current != path || !stat.is_file() || stat.nlink() != 1))
        {
            return Err("symlink, hardlink or nonregular maintenance path".into());
        }
        if current == path && (stat.uid() != uid || stat.mode() & 0o022 != 0) {
            return Err("maintenance target must be user-owned and not writable by others".into());
        }
    }
    Ok(path.to_path_buf())
}
fn private_work(path: &Path, profile: &Path) -> Result<PathBuf, String> {
    let path = checked(path, false)?;
    if !path.is_dir()
        || path.starts_with(profile)
        || profile.starts_with(&path)
        || std::fs::metadata(&path).map_err(err)?.mode() & 0o077 != 0
    {
        return Err("private work directory must be disjoint from profile".into());
    }
    Ok(path)
}
fn relative(path: &str) -> Result<(), String> {
    if path.is_empty()
        || path.starts_with('/')
        || path
            .split('/')
            .any(|part| part.is_empty() || part == "." || part == "..")
        || path.contains('\\')
    {
        return Err("canonical profile-relative path required".into());
    }
    Ok(())
}
fn rollout_path(profile: &Path, path: &str, owner: &str, missing: bool) -> Result<PathBuf, String> {
    relative(path)?;
    if !path.starts_with("sessions/") || !path.ends_with(".jsonl") {
        return Err("only active plain rollout paths are supported".into());
    }
    let path = checked(&profile.join(path), missing)?;
    if codex_rollout::thread_id_from_rollout_path(&path) != Some(canonical_id(owner)?) {
        return Err("rollout filename logical owner mismatch".into());
    }
    Ok(path)
}
fn database(profile: &Path, name: &str) -> Result<PathBuf, String> {
    let path = checked(&profile.join(name), false)?;
    if !path.is_file() {
        return Err("existing native database required".into());
    }
    for suffix in ["-wal", "-shm", "-journal"] {
        let sidecar = checked(&profile.join(format!("{name}{suffix}")), true)?;
        if let Ok(stat) = std::fs::metadata(sidecar) {
            if !stat.is_file() {
                return Err("nonregular SQLite sidecar".into());
            }
        }
    }
    Ok(path)
}
async fn require_wal(db: &mut SqliteConnection) -> Result<(), String> {
    // Read-only recovery preflight cannot repair a hot rollback journal.
    let mode: String = sqlx::query_scalar("PRAGMA journal_mode")
        .fetch_one(db)
        .await
        .map_err(err)?;
    if !mode.eq_ignore_ascii_case("wal") {
        return Err("native history maintenance requires existing WAL journal mode".into());
    }
    Ok(())
}
async fn connection(path: &Path, readonly: bool) -> Result<SqliteConnection, String> {
    let mut db = SqliteConnection::connect_with(
        &SqliteConnectOptions::new()
            .filename(path)
            .create_if_missing(false)
            .read_only(readonly)
            .busy_timeout(Duration::from_millis(250))
            .synchronous(SqliteSynchronous::Full),
    )
    .await
    .map_err(err)?;
    if let Err(error) = require_wal(&mut db).await {
        let _ = db.close().await;
        return Err(error);
    }
    Ok(db)
}
async fn state_schema(db: &mut SqliteConnection) -> Result<(Schema, Migrations), String> {
    let schema =
        sqlx::query_as("SELECT type,name,tbl_name,sql FROM sqlite_schema ORDER BY type,name")
            .fetch_all(&mut *db)
            .await
            .map_err(err)?;
    let migrations = sqlx::query_as(
        "SELECT version,description,success,checksum FROM _sqlx_migrations ORDER BY version",
    )
    .fetch_all(db)
    .await
    .map_err(err)?;
    Ok((schema, migrations))
}
async fn check_state(db: &mut SqliteConnection) -> Result<(), String> {
    let reference = SqlitePoolOptions::new()
        .max_connections(1)
        .connect("sqlite::memory:")
        .await
        .map_err(err)?;
    STATE_MIGRATOR.run(&reference).await.map_err(err)?;
    let expected = {
        let mut connection = reference.acquire().await.map_err(err)?;
        state_schema(&mut connection).await?
    };
    reference.close().await;
    if state_schema(db).await? != expected {
        return Err("native state schema/migrations differ from exact pin".into());
    }
    Ok(())
}
async fn selected(db: &mut SqliteConnection, owner: &str) -> Result<String, String> {
    let row: Option<(String, String, bool, Option<i64>)> = sqlx::query_as(
        "SELECT rollout_path,history_mode,archived,archived_at FROM threads WHERE id=?",
    )
    .bind(owner)
    .fetch_optional(db)
    .await
    .map_err(err)?;
    let (path, mode, archived, archived_at) =
        row.ok_or("existing native thread metadata required")?;
    if mode != "paginated" || archived || archived_at.is_some() {
        return Err("only existing active paginated native threads are supported".into());
    }
    Ok(path)
}
fn own(session: &Session, owner: &str) -> Result<(), String> {
    canonical_id(owner)?;
    if !session.owners.contains(owner) {
        return Err("logical owner lock is not held".into());
    }
    Ok(())
}
fn validate_inputs(
    session: &Session,
    work: &Path,
    inputs: &[Input],
    tips: &[Tip],
) -> Result<(), String> {
    if inputs.is_empty() || inputs.len() > 1024 || tips.is_empty() || tips.len() > 1024 {
        return Err("bounded nonempty native operation required".into());
    }
    let mut ids = HashSet::new();
    let mut total = 0u64;
    for input in inputs {
        own(session, &input.session_id)?;
        let id = canonical_id(&input.rollout_id)?;
        if !ids.insert(id) {
            return Err("duplicate physical rollout".into());
        }
        let destination = rollout_path(&session.profile, &input.path, &input.session_id, true)?;
        if codex_rollout::rollout_id_from_path(&destination) != Some(id) {
            return Err("physical rollout mismatch".into());
        }
        let source = checked(
            &work
                .join("inputs")
                .join(destination.file_name().ok_or("missing filename")?),
            false,
        )?;
        let stat = std::fs::metadata(&source).map_err(err)?;
        if stat.mode() & 0o222 != 0
            || input.end_byte_offset == 0
            || input.end_byte_offset > 64 << 20
            || stat.len() != input.end_byte_offset
            || input.end_ordinal_exclusive == 0
            || input.end_ordinal_exclusive >= 1 << 63
        {
            return Err("immutable source prefix bounds are invalid".into());
        }
        total += input.end_byte_offset;
        if total > 128 << 20 {
            return Err("native prefix closure exceeds limit".into());
        }
        if digest(&std::fs::read(source).map_err(err)?) != input.prefix_sha256 {
            return Err("private source prefix changed".into());
        }
    }
    let mut owners = HashSet::new();
    for tip in tips {
        own(session, &tip.thread_id)?;
        if !owners.insert(&tip.thread_id) {
            return Err("duplicate selected logical tip".into());
        }
        rollout_path(&session.profile, &tip.expected_path, &tip.thread_id, false)?;
        if !inputs
            .iter()
            .any(|input| input.session_id == tip.thread_id && input.path == tip.expected_path)
        {
            return Err("selected old tip is outside the authenticated ancestry".into());
        }
        let path = rollout_path(&session.profile, &tip.new_path, &tip.thread_id, true)?;
        if !inputs.iter().any(|input| {
            input.session_id == tip.thread_id
                && input.path == tip.new_path
                && codex_rollout::rollout_id_from_path(&path)
                    == canonical_id(&input.rollout_id).ok()
        }) {
            return Err("selected tip omitted from authenticated closure".into());
        }
    }
    Ok(())
}
fn publication(work: &Path, profile: &Path, inputs: &[BoundInput]) -> Result<Vec<Rollout>, String> {
    inputs
        .iter()
        .map(|bound| {
            let input = &bound.input;
            let destination = profile.join(&input.path);
            let source = work
                .join("inputs")
                .join(destination.file_name().ok_or("missing source filename")?);
            Ok(Rollout {
                rollout_id: input.rollout_id.clone(),
                end_byte_offset: input.end_byte_offset as i64,
                end_ordinal_exclusive: input.end_ordinal_exclusive as i64,
                advance: bound.base.as_ref().map(|base| Advance {
                    source_path: source,
                    destination_path: destination,
                    base_end_byte_offset: base.end_byte_offset,
                    base_end_ordinal_exclusive: base.end_ordinal_exclusive,
                    base_prefix_sha256: base.prefix_sha256.clone(),
                    prefix_sha256: input.prefix_sha256.clone(),
                }),
            })
        })
        .collect()
}
async fn cas_tip(db: &mut SqliteConnection, profile: &Path, tip: &Tip) -> Result<(), String> {
    let actual = selected(db, &tip.thread_id).await?;
    let old = profile
        .join(&tip.expected_path)
        .to_string_lossy()
        .to_string();
    let new = profile.join(&tip.new_path).to_string_lossy().to_string();
    if actual == new {
        return Ok(());
    }
    if actual != old {
        return Err("native selected-tip changed before commit".into());
    }
    let update = sqlx::query("UPDATE threads SET rollout_path=? WHERE id=? AND rollout_path=? AND history_mode='paginated' AND archived=0 AND archived_at IS NULL")
        .bind(&new).bind(&tip.thread_id).bind(&old).execute(db).await.map_err(err)?;
    if update.rows_affected() != 1 {
        return Err("native selected-tip compare-and-set failed".into());
    }
    Ok(())
}

async fn check_tips(session: &Session, tips: &[Tip], desired: bool) -> Result<(), String> {
    let mut db = connection(&database(&session.profile, "state_5.sqlite")?, true).await?;
    check_state(&mut db).await?;
    for tip in tips {
        let actual = selected(&mut db, &tip.thread_id).await?;
        let expected = session
            .profile
            .join(&tip.expected_path)
            .to_string_lossy()
            .to_string();
        let new = session
            .profile
            .join(&tip.new_path)
            .to_string_lossy()
            .to_string();
        if actual != new && (desired || actual != expected) {
            return Err("native selected-tip preimage conflict".into());
        }
    }
    db.close().await.map_err(err)
}
async fn prepare(
    session: &Session,
    work: &Path,
    inputs: Vec<Input>,
    tips: Vec<Tip>,
) -> Result<Value, String> {
    validate_inputs(session, work, &inputs, &tips)?;
    check_tips(session, &tips, false).await?;
    let history = database(&session.profile, "thread_history_1.sqlite")?;
    let mut db = connection(&history, true).await?;
    let mut bound = Vec::new();
    for input in &inputs {
        let existing: Option<(i64, i64)> = sqlx::query_as("SELECT next_rollout_byte_offset,next_rollout_ordinal FROM thread_history_projection_state WHERE thread_id=?")
            .bind(&input.rollout_id).fetch_optional(&mut db).await.map_err(err)?;
        let base = match existing {
            Some((bytes, ordinal))
                if bytes > 0
                    && ordinal > 0
                    && bytes < input.end_byte_offset as i64
                    && ordinal < input.end_ordinal_exclusive as i64 =>
            {
                let source = work.join("inputs").join(
                    Path::new(&input.path)
                        .file_name()
                        .ok_or("missing filename")?,
                );
                let bytes_all = std::fs::read(source).map_err(err)?;
                Some(Base {
                    end_byte_offset: bytes,
                    end_ordinal_exclusive: ordinal,
                    prefix_sha256: digest(&bytes_all[..bytes as usize]),
                })
            }
            _ => None,
        };
        bound.push(BoundInput {
            input: input.clone(),
            base,
        });
    }
    db.close().await.map_err(err)?;
    let stage = work.join("native-stage");
    let manifest = json!({"source_root":work.join("inputs"),"output_dir":stage,"rollouts":inputs.iter().map(|input| {
        json!({"rollout_id":input.rollout_id,"path":work.join("inputs").join(Path::new(&input.path).file_name().unwrap()),
            "end_byte_offset":input.end_byte_offset,"end_ordinal_exclusive":input.end_ordinal_exclusive,"prefix_sha256":input.prefix_sha256})
    }).collect::<Vec<_>>()});
    super::run_projection_only(&manifest.to_string()).await?;
    let stage_db = checked(&stage.join("thread_history_1.sqlite"), false)?;
    native_publication(
        history.clone(),
        stage_db.clone(),
        work.join("inputs"),
        publication(work, &session.profile, &bound)?,
        &session.profile,
        work,
        PublicationMode::Prepare,
    )
    .await?;
    std::fs::set_permissions(&stage_db, std::fs::Permissions::from_mode(0o400)).map_err(err)?;
    let artifact = Artifact {
        version: 1,
        upstream_commit: PIN.into(),
        profile: identity(&session.profile)?,
        state: identity(&database(&session.profile, "state_5.sqlite")?)?,
        history: identity(&history)?,
        stage_sha256: digest(&std::fs::read(stage_db).map_err(err)?),
        rollouts: bound,
        tips,
    };
    let bytes = serde_json::to_vec(&artifact).map_err(err)?;
    let mut file = std::fs::OpenOptions::new()
        .write(true)
        .create_new(true)
        .open(work.join(ARTIFACT))
        .map_err(err)?;
    file.write_all(&bytes).map_err(err)?;
    file.sync_all().map_err(err)?;
    std::fs::set_permissions(work.join(ARTIFACT), std::fs::Permissions::from_mode(0o400))
        .map_err(err)?;
    Ok(json!({"artifact_path":ARTIFACT,"artifact_sha256":digest(&bytes)}))
}
fn load(session: &Session, work: &Path, hash: &str) -> Result<Artifact, String> {
    let path = checked(&work.join(ARTIFACT), false)?;
    let bytes = std::fs::read(path).map_err(err)?;
    if bytes.len() > MAX_REQUEST as usize || digest(&bytes) != hash {
        return Err("native artifact authentication mismatch".into());
    }
    let artifact: Artifact = serde_json::from_slice(&bytes).map_err(err)?;
    if artifact.version != 1
        || artifact.upstream_commit != PIN
        || identity(&session.profile)? != artifact.profile
        || identity(&database(&session.profile, "state_5.sqlite")?)? != artifact.state
        || identity(&database(&session.profile, "thread_history_1.sqlite")?)? != artifact.history
    {
        return Err("native artifact version/root/database identity changed".into());
    }
    validate_inputs(
        session,
        work,
        &artifact
            .rollouts
            .iter()
            .map(|bound| bound.input.clone())
            .collect::<Vec<_>>(),
        &artifact.tips,
    )?;
    let stage = checked(&work.join("native-stage/thread_history_1.sqlite"), false)?;
    for suffix in ["-wal", "-shm", "-journal"] {
        if std::fs::symlink_metadata(
            work.join(format!("native-stage/thread_history_1.sqlite{suffix}")),
        )
        .is_ok()
        {
            return Err("private stage has SQLite sidecars".into());
        }
    }
    if digest(&std::fs::read(stage).map_err(err)?) != artifact.stage_sha256 {
        return Err("private stage changed".into());
    }
    Ok(artifact)
}
async fn apply(session: &Session, work: &Path, hash: &str, publish: bool) -> Result<Value, String> {
    let artifact = load(session, work, hash)?;
    let history = connection(
        &database(&session.profile, "thread_history_1.sqlite")?,
        true,
    )
    .await?;
    history.close().await.map_err(err)?;
    for bound in &artifact.rollouts {
        let input = &bound.input;
        let path = rollout_path(&session.profile, &input.path, &input.session_id, false)?;
        let mut bytes = Vec::new();
        std::fs::File::open(path)
            .map_err(err)?
            .take(input.end_byte_offset)
            .read_to_end(&mut bytes)
            .map_err(err)?;
        if bytes.len() as u64 != input.end_byte_offset || digest(&bytes) != input.prefix_sha256 {
            return Err("published destination prefix differs from frozen native input".into());
        }
    }
    check_tips(session, &artifact.tips, !publish).await?;
    native_publication(
        database(&session.profile, "thread_history_1.sqlite")?,
        work.join("native-stage/thread_history_1.sqlite"),
        work.join("inputs"),
        publication(work, &session.profile, &artifact.rollouts)?,
        &session.profile,
        work,
        if publish {
            PublicationMode::Publish
        } else {
            PublicationMode::Verify
        },
    )
    .await?;
    if publish {
        // History is verified before the stable logical ID can select the new file.
        native_publication(
            database(&session.profile, "thread_history_1.sqlite")?,
            work.join("native-stage/thread_history_1.sqlite"),
            work.join("inputs"),
            publication(work, &session.profile, &artifact.rollouts)?,
            &session.profile,
            work,
            PublicationMode::Verify,
        )
        .await?;
        test_pause("history_committed");
        let mut db = connection(&database(&session.profile, "state_5.sqlite")?, false).await?;
        sqlx::query("BEGIN IMMEDIATE")
            .execute(&mut db)
            .await
            .map_err(err)?;
        let result: Result<(), String> = async {
            check_state(&mut db).await?;
            for tip in &artifact.tips {
                cas_tip(&mut db, &session.profile, tip).await?;
            }
            Ok(())
        }
        .await;
        match result {
            Ok(()) => {
                test_pause("state_before_commit");
                sqlx::query("COMMIT").execute(&mut db).await.map_err(err)?;
            }
            Err(error) => {
                let _ = sqlx::query("ROLLBACK").execute(&mut db).await;
                return Err(error);
            }
        }
        db.close().await.map_err(err)?;
        test_pause("state_after_commit");
    }
    check_tips(session, &artifact.tips, true).await?;
    Ok(json!({"status":"verified","selected_tips":artifact.tips.len(),"upstream_commit":PIN}))
}

pub async fn run_native_history_helper() -> Result<(), String> {
    if !cfg!(target_os = "linux") {
        return Err("native history maintenance is Linux-only".into());
    }
    let stdin = std::io::stdin();
    let mut input = stdin.lock();
    let mut session: Option<Session> = None;
    loop {
        let mut line = Vec::new();
        let count = input
            .by_ref()
            .take(MAX_REQUEST + 1)
            .read_until(b'\n', &mut line)
            .map_err(err)?;
        if count == 0 {
            return Ok(());
        }
        if count as u64 > MAX_REQUEST || !line.ends_with(b"\n") {
            return Err("bounded newline-terminated helper command required".into());
        }
        let command: Result<Command, _> = serde_json::from_slice(&line);
        let mut released = false;
        let result: Result<Value, String> = async {
            match command.map_err(err)? {
                Command::Acquire {
                    profile_home,
                    mut owner_ids,
                } => {
                    if session.is_some() {
                        return Err("native owner session already acquired".into());
                    }
                    let profile = checked(&profile_home, false)?;
                    if !profile.is_dir() || owner_ids.is_empty() || owner_ids.len() > 1024 {
                        return Err("profile and bounded owner closure required".into());
                    }
                    owner_ids.sort();
                    if owner_ids.windows(2).any(|pair| pair[0] == pair[1]) {
                        return Err("duplicate logical owner".into());
                    }
                    checked(&profile.join("thread-writer-locks"), true)?;
                    if let Ok(entries) = std::fs::read_dir(profile.join("thread-writer-locks")) {
                        for entry in entries {
                            checked(&entry.map_err(err)?.path(), false)?;
                        }
                    }
                    let coordinator =
                        Arc::new(WriterLockCoordinator::new_without_stale_cleanup(&profile));
                    let mut guards = Vec::new();
                    for owner in &owner_ids {
                        guards.push(coordinator.acquire(canonical_id(owner)?).map_err(err)?);
                    }
                    session = Some(Session {
                        profile,
                        owners: owner_ids.into_iter().collect(),
                        _guards: guards,
                    });
                    Ok(json!({"status":"acquired","native_global_exclusion":false}))
                }
                Command::Inspect { thread_ids } => {
                    let current = session.as_ref().ok_or("acquire native owners first")?;
                    if thread_ids.is_empty() || thread_ids.len() > 1024 {
                        return Err("bounded thread inspection required".into());
                    }
                    let mut db =
                        connection(&database(&current.profile, "state_5.sqlite")?, true).await?;
                    check_state(&mut db).await?;
                    let mut tips = Vec::new();
                    for thread_id in thread_ids {
                        own(current, &thread_id)?;
                        let path = PathBuf::from(selected(&mut db, &thread_id).await?);
                        let relative = path
                            .strip_prefix(&current.profile)
                            .map_err(err)?
                            .to_str()
                            .ok_or("non-UTF8 native path")?
                            .to_string();
                        rollout_path(&current.profile, &relative, &thread_id, false)?;
                        tips.push(json!({"thread_id":thread_id,"expected_path":relative}));
                    }
                    db.close().await.map_err(err)?;
                    Ok(json!({"tips":tips}))
                }
                Command::Prepare {
                    profile_home,
                    work_dir,
                    rollouts,
                    tips,
                } => {
                    let current = session.as_ref().ok_or("acquire native owners first")?;
                    if profile_home != current.profile {
                        return Err("profile changed during maintenance".into());
                    }
                    let work = private_work(&work_dir, &current.profile)?;
                    prepare(current, &work, rollouts, tips).await
                }
                Command::Publish {
                    profile_home,
                    work_dir,
                    artifact_sha256,
                } => {
                    let current = session.as_ref().ok_or("acquire native owners first")?;
                    if profile_home != current.profile {
                        return Err("profile changed during maintenance".into());
                    }
                    let work = private_work(&work_dir, &current.profile)?;
                    apply(current, &work, &artifact_sha256, true).await
                }
                Command::Verify {
                    profile_home,
                    work_dir,
                    artifact_sha256,
                } => {
                    let current = session.as_ref().ok_or("acquire native owners first")?;
                    if profile_home != current.profile {
                        return Err("profile changed during maintenance".into());
                    }
                    let work = private_work(&work_dir, &current.profile)?;
                    apply(current, &work, &artifact_sha256, false).await
                }
                Command::Release {} => {
                    session = None;
                    released = true;
                    Ok(json!({"status":"released"}))
                }
            }
        }
        .await;
        let reply = match result {
            Ok(result) => json!({"ok":true,"result":result}),
            Err(error) => json!({"ok":false,"error":error}),
        };
        println!("{reply}");
        std::io::stdout().flush().map_err(err)?;
        if released {
            return Ok(());
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    const OWNER: &str = "00000000-0000-4000-8000-000000000001";

    async fn state_fixture() -> SqliteConnection {
        let mut db = SqliteConnection::connect("sqlite::memory:").await.unwrap();
        STATE_MIGRATOR.run(&mut db).await.unwrap();
        for id in [OWNER, "unrelated"] {
            sqlx::query("INSERT INTO threads(id,rollout_path,created_at,updated_at,source,model_provider,cwd,title,sandbox_policy,approval_mode,history_mode,name,tokens_used) VALUES(?,?,1,2,'cli','provider','/project','title','read-only','never','paginated','name',37)")
                .bind(id).bind("/profile/sessions/old.jsonl").execute(&mut db).await.unwrap();
        }
        db
    }

    fn tip() -> Tip {
        Tip {
            thread_id: OWNER.into(),
            expected_path: "sessions/old.jsonl".into(),
            new_path: "sessions/new.jsonl".into(),
        }
    }

    #[tokio::test]
    async fn native_targets_require_existing_wal_without_changing_mode() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("native.sqlite");
        let mut db = SqliteConnection::connect_with(
            &SqliteConnectOptions::new()
                .filename(&path)
                .create_if_missing(true)
                .journal_mode(sqlx::sqlite::SqliteJournalMode::Delete),
        )
        .await
        .unwrap();
        sqlx::query("CREATE TABLE sentinel(value TEXT)")
            .execute(&mut db)
            .await
            .unwrap();
        assert!(require_wal(&mut db).await.is_err());
        db.close().await.unwrap();
        assert!(connection(&path, true).await.is_err());
        let mut db = SqliteConnection::connect_with(
            &SqliteConnectOptions::new()
                .filename(&path)
                .journal_mode(sqlx::sqlite::SqliteJournalMode::Wal),
        )
        .await
        .unwrap();
        require_wal(&mut db).await.unwrap();
        db.close().await.unwrap();
        let mut db = connection(&path, true).await.unwrap();
        let mode: String = sqlx::query_scalar("PRAGMA journal_mode")
            .fetch_one(&mut db)
            .await
            .unwrap();
        assert_eq!(mode, "wal");
        db.close().await.unwrap();
    }

    #[tokio::test]
    async fn tip_cas_preserves_metadata_unrelated_rows_and_is_idempotent() {
        let mut db = state_fixture().await;
        let before: (String, String, String, i64, i64, i64) = sqlx::query_as(
            "SELECT title,name,cwd,tokens_used,created_at,updated_at FROM threads WHERE id=?",
        )
        .bind(OWNER)
        .fetch_one(&mut db)
        .await
        .unwrap();
        cas_tip(&mut db, Path::new("/profile"), &tip())
            .await
            .unwrap();
        cas_tip(&mut db, Path::new("/profile"), &tip())
            .await
            .unwrap();
        assert_eq!(
            selected(&mut db, OWNER).await.unwrap(),
            "/profile/sessions/new.jsonl"
        );
        assert_eq!(
            selected(&mut db, "unrelated").await.unwrap(),
            "/profile/sessions/old.jsonl"
        );
        let after: (String, String, String, i64, i64, i64) = sqlx::query_as(
            "SELECT title,name,cwd,tokens_used,created_at,updated_at FROM threads WHERE id=?",
        )
        .bind(OWNER)
        .fetch_one(&mut db)
        .await
        .unwrap();
        assert_eq!(before, after);
    }

    #[tokio::test]
    async fn changed_tip_and_unsupported_thread_states_are_preserved() {
        let mut db = state_fixture().await;
        sqlx::query("UPDATE threads SET rollout_path='/independent' WHERE id=?")
            .bind(OWNER)
            .execute(&mut db)
            .await
            .unwrap();
        assert!(
            cas_tip(&mut db, Path::new("/profile"), &tip())
                .await
                .is_err()
        );
        for query in [
            "UPDATE threads SET archived=1 WHERE id=?",
            "UPDATE threads SET archived=0,history_mode='legacy' WHERE id=?",
        ] {
            sqlx::query(query)
                .bind(OWNER)
                .execute(&mut db)
                .await
                .unwrap();
            assert!(
                cas_tip(&mut db, Path::new("/profile"), &tip())
                    .await
                    .is_err()
            );
        }
        let path: String = sqlx::query_scalar("SELECT rollout_path FROM threads WHERE id=?")
            .bind(OWNER)
            .fetch_one(&mut db)
            .await
            .unwrap();
        assert_eq!(path, "/independent");
    }

    #[tokio::test]
    async fn late_tip_conflict_rolls_back_earlier_tip_updates() {
        let mut db = state_fixture().await;
        sqlx::query("BEGIN IMMEDIATE")
            .execute(&mut db)
            .await
            .unwrap();
        cas_tip(&mut db, Path::new("/profile"), &tip())
            .await
            .unwrap();
        let conflict = Tip {
            thread_id: "unrelated".into(),
            expected_path: "sessions/independent.jsonl".into(),
            new_path: "sessions/replacement.jsonl".into(),
        };
        assert!(
            cas_tip(&mut db, Path::new("/profile"), &conflict)
                .await
                .is_err()
        );
        sqlx::query("ROLLBACK").execute(&mut db).await.unwrap();
        assert_eq!(
            selected(&mut db, OWNER).await.unwrap(),
            "/profile/sessions/old.jsonl"
        );
        assert_eq!(
            selected(&mut db, "unrelated").await.unwrap(),
            "/profile/sessions/old.jsonl"
        );
    }

    #[tokio::test]
    async fn exact_state_guard_rejects_schema_and_migration_drift() {
        let mut db = state_fixture().await;
        check_state(&mut db).await.unwrap();
        sqlx::query("UPDATE _sqlx_migrations SET checksum=x'00' WHERE version=1")
            .execute(&mut db)
            .await
            .unwrap();
        assert!(check_state(&mut db).await.is_err());
        let mut db = state_fixture().await;
        sqlx::query("ALTER TABLE threads ADD COLUMN unexpected TEXT")
            .execute(&mut db)
            .await
            .unwrap();
        assert!(check_state(&mut db).await.is_err());
    }

    #[test]
    fn protocol_rejects_unknown_fields_and_noncanonical_paths() {
        assert!(
            serde_json::from_str::<Command>(r#"{"action":"release","unexpected":true}"#).is_err()
        );
        for path in [
            "../file",
            "/absolute",
            "sessions//file",
            "sessions/./file",
            "sessions/../file",
            "sessions\\file",
        ] {
            assert!(relative(path).is_err(), "{path}");
        }
        assert!(canonical_id("00000000-0000-4000-8000-00000000000A").is_err());
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn native_ownership_contends_without_unrelated_stale_cleanup() {
        let home = tempfile::tempdir().unwrap();
        let locks = home.path().join("thread-writer-locks");
        std::fs::create_dir(&locks).unwrap();
        let stale = locks.join("00000000-0000-4000-8000-000000000099.lock");
        std::fs::write(&stale, b"unrelated").unwrap();
        let first = Arc::new(WriterLockCoordinator::new_without_stale_cleanup(
            home.path(),
        ));
        let guard = first.acquire(canonical_id(OWNER).unwrap()).unwrap();
        assert_eq!(std::fs::read(&stale).unwrap(), b"unrelated");
        let second = Arc::new(WriterLockCoordinator::new_without_stale_cleanup(
            home.path(),
        ));
        assert_eq!(
            second
                .acquire(canonical_id(OWNER).unwrap())
                .err()
                .unwrap()
                .kind(),
            std::io::ErrorKind::WouldBlock
        );
        drop(guard);
        second.acquire(canonical_id(OWNER).unwrap()).unwrap();
        assert!(stale.exists());
    }
}
