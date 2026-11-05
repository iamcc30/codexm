//! Diagnostic only: publish canonical staged rows into an existing synthetic DB.
//! No live-profile exclusion, source verification, or filesystem+DB atomicity claim.
use std::collections::HashSet;
use std::io::Write;
use std::path::{Component, Path, PathBuf};
use std::time::Duration;

use serde::Deserialize;
use sqlx::Connection;
use sqlx::SqliteConnection;
use sqlx::sqlite::{SqliteConnectOptions, SqlitePoolOptions, SqliteSynchronous};

static MIGRATOR: sqlx::migrate::Migrator = sqlx_macros::migrate!("../state/thread_history_migrations");
// Only compile-time identifiers enter SQL. All request data uses bind parameters.
struct TableQueries {
    name: &'static str,
    any_main: &'static str,
    invalid_main: &'static str,
    invalid_staged: &'static str,
    insert: &'static str,
    forward_prefix: &'static str,
    reverse_prefix: &'static str,
}

macro_rules! table_queries {
    ($table:literal, $columns:literal) => {
        TableQueries {
            name: $table,
            any_main: concat!("SELECT EXISTS(SELECT 1 FROM main.", $table, " WHERE thread_id=?)"),
            invalid_main: concat!("SELECT EXISTS(SELECT 1 FROM main.", $table, " WHERE thread_id=? AND (rollout_ordinal<0 OR rollout_ordinal>=?))"),
            invalid_staged: concat!("SELECT EXISTS(SELECT 1 FROM staged.", $table, " WHERE thread_id=? AND (rollout_ordinal<0 OR rollout_ordinal>=?))"),
            insert: concat!("INSERT INTO main.", $table, " (", $columns, ") SELECT ", $columns, " FROM staged.", $table, " WHERE thread_id=?"),
            forward_prefix: concat!("SELECT EXISTS(SELECT ", $columns, " FROM main.", $table, " WHERE thread_id=? AND rollout_ordinal<? EXCEPT SELECT ", $columns, " FROM staged.", $table, " WHERE thread_id=?)"),
            reverse_prefix: concat!("SELECT EXISTS(SELECT ", $columns, " FROM staged.", $table, " WHERE thread_id=? EXCEPT SELECT ", $columns, " FROM main.", $table, " WHERE thread_id=? AND rollout_ordinal<?)"),
        }
    };
}

const TABLES: [TableQueries; 3] = [
    table_queries!("thread_turns", "thread_id,turn_id,rollout_ordinal,status,error_json,started_at,completed_at,duration_ms,first_user_item_id,final_agent_item_id,rollout_byte_offset,rollout_end_ordinal,rollout_end_byte_offset,root_turn_id"),
    table_queries!("thread_items", "thread_id,turn_id,item_id,rollout_ordinal,created_at_ms,item_json,item_type,updated_at_ordinal,started_at_ms,completed_at_ms"),
    table_queries!("thread_realtime_items", "thread_id,item_id,rollout_ordinal,created_at_ms,item_type,item_json"),
];

#[derive(Clone, Copy, Debug)]
enum Database { Main, Staged }

impl Database {
    fn schema_sql(self) -> &'static str {
        match self {
            Self::Main => "SELECT type,name,tbl_name,sql FROM main.sqlite_schema ORDER BY type,name",
            Self::Staged => "SELECT type,name,tbl_name,sql FROM staged.sqlite_schema ORDER BY type,name",
        }
    }

    fn migrations_sql(self) -> &'static str {
        match self {
            Self::Main => "SELECT version,description,success,checksum FROM main._sqlx_migrations ORDER BY version",
            Self::Staged => "SELECT version,description,success,checksum FROM staged._sqlx_migrations ORDER BY version",
        }
    }

    fn checkpoint_sql(self) -> &'static str {
        match self {
            Self::Main => "SELECT next_rollout_byte_offset,next_rollout_ordinal FROM main.thread_history_projection_state WHERE thread_id=?",
            Self::Staged => "SELECT next_rollout_byte_offset,next_rollout_ordinal FROM staged.thread_history_projection_state WHERE thread_id=?",
        }
    }
}
const MARKER: &str = "codexm synthetic projection publication\n";
type SchemaRow = (String, String, String, Option<String>);
type MigrationRow = (i64, String, bool, Vec<u8>);

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    synthetic_test_root: PathBuf,
    staging_db: PathBuf,
    target_db: PathBuf,
    rollouts: Vec<Rollout>,
    #[serde(default = "default_timeout")]
    busy_timeout_ms: u64,
    #[serde(default)]
    fail_before_commit: bool,
    #[serde(default)]
    pause_before_commit_ms: u64,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Rollout {
    rollout_id: String,
    end_byte_offset: i64,
    end_ordinal_exclusive: i64,
}

fn default_timeout() -> u64 { 250 }
fn err(value: impl std::fmt::Display) -> String { value.to_string() }

fn checked(path: &Path) -> Result<PathBuf, String> {
    if !path.is_absolute() { return Err("absolute paths required".into()); }
    let mut current = PathBuf::new();
    for part in path.components() {
        if matches!(part, Component::ParentDir | Component::CurDir) { return Err("dot components forbidden".into()); }
        current.push(part.as_os_str());
        if std::fs::symlink_metadata(&current).map_err(err)?.file_type().is_symlink() {
            return Err("symlinks forbidden".into());
        }
    }
    std::fs::canonicalize(path).map_err(err)
}

fn input_db(root: &Path, path: &Path) -> Result<PathBuf, String> {
    let path = checked(path)?;
    if !path.starts_with(root) || !path.is_file() { return Err("DB must be an existing file inside synthetic root".into()); }
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;
        if std::fs::metadata(&path).map_err(err)?.nlink() != 1 { return Err("hardlinked DB forbidden".into()); }
    }
    for suffix in ["-wal", "-shm", "-journal"] {
        let mut sidecar = path.as_os_str().to_os_string();
        sidecar.push(suffix);
        match std::fs::symlink_metadata(&sidecar) {
            Ok(meta) if meta.file_type().is_symlink() || !meta.is_file() => return Err("unsafe SQLite sidecar".into()),
            Ok(meta) => {
                #[cfg(unix)]
                {
                    use std::os::unix::fs::MetadataExt;
                    if meta.nlink() != 1 { return Err("hardlinked SQLite sidecar forbidden".into()); }
                }
            },
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {},
            Err(e) => return Err(err(e)),
        }
    }
    Ok(path)
}

fn readonly_uri(path: &Path) -> Result<String, String> {
    let mut uri = String::from("file:");
    for byte in path.to_str().ok_or("non-UTF8 path unsupported")?.as_bytes() {
        if byte.is_ascii_alphanumeric() || b"/._-".contains(byte) { uri.push(char::from(*byte)); }
        else { uri.push_str(&format!("%{byte:02X}")); }
    }
    // Caller supplies a closed private staging snapshot, with no sidecars below.
    // Never apply immutable mode to the target, which needs normal crash recovery.
    uri.push_str("?mode=ro&immutable=1");
    Ok(uri)
}

async fn connect(target: &Path, staging: &Path, readonly: bool, timeout: u64) -> Result<SqliteConnection, String> {
    let options = SqliteConnectOptions::new().filename(target).create_if_missing(false)
        .read_only(readonly).busy_timeout(Duration::from_millis(timeout))
        .synchronous(SqliteSynchronous::Full);
    let mut connection = SqliteConnection::connect_with(&options).await.map_err(err)?;
    // Only main is writable. ATTACH is parameterized and explicitly read-only.
    sqlx::query("ATTACH DATABASE ? AS staged").bind(readonly_uri(staging)?)
        .execute(&mut connection).await.map_err(err)?;
    Ok(connection)
}

async fn schema(connection: &mut SqliteConnection, db: Database) -> Result<Vec<SchemaRow>, String> {
    sqlx::query_as(db.schema_sql()).fetch_all(connection).await.map_err(err)
}

async fn migrations(connection: &mut SqliteConnection, db: Database) -> Result<Vec<MigrationRow>, String> {
    sqlx::query_as(db.migrations_sql()).fetch_all(connection).await.map_err(err)
}

async fn check_schema(connection: &mut SqliteConnection, expected_schema: &[SchemaRow], expected_migrations: &[MigrationRow]) -> Result<(), String> {
    for db in [Database::Main, Database::Staged] {
        if schema(connection, db).await? != expected_schema || migrations(connection, db).await? != expected_migrations {
            return Err(format!("{db:?} schema/migration mismatch against pinned native reference"));
        }
    }
    Ok(())
}

async fn checkpoint(connection: &mut SqliteConnection, db: Database, id: &str) -> Result<Option<(i64, i64)>, String> {
    sqlx::query_as(db.checkpoint_sql()).bind(id).fetch_optional(connection).await.map_err(err)
}

async fn any_rows(connection: &mut SqliteConnection, table: &TableQueries, id: &str) -> Result<bool, String> {
    sqlx::query_scalar(table.any_main).bind(id).fetch_one(connection).await.map_err(err)
}

async fn same_prefix(connection: &mut SqliteConnection, table: &TableQueries, rollout: &Rollout) -> Result<bool, String> {
    let forward: bool = sqlx::query_scalar(table.forward_prefix)
        .bind(&rollout.rollout_id).bind(rollout.end_ordinal_exclusive).bind(&rollout.rollout_id)
        .fetch_one(&mut *connection).await.map_err(err)?;
    let reverse: bool = sqlx::query_scalar(table.reverse_prefix)
        .bind(&rollout.rollout_id).bind(&rollout.rollout_id).bind(rollout.end_ordinal_exclusive)
        .fetch_one(connection).await.map_err(err)?;
    Ok(!forward && !reverse)
}

/// Diagnostic publication of synthetic data only. Preserves the original projector API.
pub async fn run_publish_projection(json: &str) -> Result<String, String> {
    let request: Request = serde_json::from_str(json).map_err(err)?;
    if request.busy_timeout_ms > 5000 || request.pause_before_commit_ms > 5000 || request.rollouts.is_empty() || request.rollouts.len() > 1024 {
        return Err("invalid diagnostic limits".into());
    }
    let root = checked(&request.synthetic_test_root)?;
    if !root.is_dir() { return Err("synthetic root must be a directory".into()); }
    let marker = checked(&root.join("CODEXM_SYNTHETIC_PROJECTION_TEST"))?;
    if std::fs::metadata(&marker).map_err(err)?.len() != MARKER.len() as u64 || std::fs::read_to_string(marker).map_err(err)? != MARKER {
        return Err("explicit synthetic test marker required".into());
    }
    let target = input_db(&root, &request.target_db)?;
    let staging = input_db(&root, &request.staging_db)?;
    if target == staging { return Err("staging and target must differ".into()); }
    for suffix in ["-wal", "-shm", "-journal"] {
        let mut sidecar = staging.as_os_str().to_os_string();
        sidecar.push(suffix);
        match std::fs::symlink_metadata(&sidecar) {
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {},
            _ => return Err("staging must be a closed private snapshot without SQLite sidecars".into()),
        }
    }
    let mut ids = HashSet::new();
    for rollout in &request.rollouts {
        let id = codex_protocol::ThreadId::from_string(&rollout.rollout_id).map_err(err)?;
        if id.to_string() != rollout.rollout_id || !ids.insert(id) || rollout.end_byte_offset <= 0 || rollout.end_ordinal_exclusive <= 0 {
            return Err("duplicate/noncanonical rollout ID or invalid bounds".into());
        }
    }
    // Reference only: migration runs in memory, never on the target or staging DB.
    let reference = SqlitePoolOptions::new().max_connections(1).connect("sqlite::memory:").await.map_err(err)?;
    MIGRATOR.run(&reference).await.map_err(err)?;
    let (expected_schema, expected_migrations) = {
        let mut connection = reference.acquire().await.map_err(err)?;
        (schema(&mut connection, Database::Main).await?, migrations(&mut connection, Database::Main).await?)
    };
    reference.close().await;
    // Read-only preflight ensures unsupported databases are never opened writable.
    let mut preflight = connect(&target, &staging, true, request.busy_timeout_ms).await?;
    let preflight_result = check_schema(&mut preflight, &expected_schema, &expected_migrations).await;
    preflight.close().await.map_err(err)?;
    preflight_result?;

    let mut connection = connect(&target, &staging, false, request.busy_timeout_ms).await?;
    sqlx::query("BEGIN IMMEDIATE").execute(&mut connection).await.map_err(err)?;
    let result: Result<(Vec<String>, Vec<String>), String> = async {
        // Repeat under the write transaction so a preflight race fails closed.
        check_schema(&mut connection, &expected_schema, &expected_migrations).await?;
        let mut inserted = Vec::new();
        let mut retained = Vec::new();
        for rollout in &request.rollouts {
            let id = &rollout.rollout_id;
            let bound = (rollout.end_byte_offset, rollout.end_ordinal_exclusive);
            if checkpoint(&mut connection, Database::Staged, id).await? != Some(bound) {
                return Err(format!("staging checkpoint mismatch for {id}"));
            }
            for table in &TABLES {
                let invalid: bool = sqlx::query_scalar(table.invalid_staged)
                    .bind(id).bind(rollout.end_ordinal_exclusive).fetch_one(&mut connection).await.map_err(err)?;
                if invalid { return Err(format!("staging rows exceed prefix for {id}")); }
            }
            let invalid_updates: bool = sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM staged.thread_items WHERE thread_id=? AND (updated_at_ordinal<rollout_ordinal OR updated_at_ordinal>=?))")
                .bind(id).bind(rollout.end_ordinal_exclusive).fetch_one(&mut connection).await.map_err(err)?;
            if invalid_updates { return Err(format!("staging item updates exceed prefix or precede creation for {id}")); }
            match checkpoint(&mut connection, Database::Main, id).await? {
                None => {
                    for table in &TABLES {
                        if any_rows(&mut connection, table, id).await? { return Err(format!("orphan destination rows for {id}")); }
                    }
                    for table in &TABLES {
                        sqlx::query(table.insert)
                            .bind(id).execute(&mut connection).await.map_err(err)?;
                    }
                    sqlx::query("INSERT INTO main.thread_history_projection_state(thread_id,next_rollout_byte_offset,next_rollout_ordinal) SELECT thread_id,next_rollout_byte_offset,next_rollout_ordinal FROM staged.thread_history_projection_state WHERE thread_id=?")
                        .bind(id).execute(&mut connection).await.map_err(err)?;
                    inserted.push(id.clone());
                }
                Some(existing) => {
                    if existing != bound && !(existing.0 > bound.0 && existing.1 > bound.1) { return Err(format!("incompatible destination checkpoint for {id}")); }
                    for table in &TABLES {
                        let invalid: bool = sqlx::query_scalar(table.invalid_main)
                            .bind(id).bind(existing.1).fetch_one(&mut connection).await.map_err(err)?;
                        if invalid { return Err(format!("destination rows exceed its checkpoint for {id}")); }
                        if !same_prefix(&mut connection, table, rollout).await? { return Err(format!("incompatible destination prefix in {} for {id}", table.name)); }
                    }
                    let invalid_updates: bool = sqlx::query_scalar("SELECT EXISTS(SELECT 1 FROM main.thread_items WHERE thread_id=? AND (updated_at_ordinal<rollout_ordinal OR updated_at_ordinal>=?))")
                        .bind(id).bind(existing.1).fetch_one(&mut connection).await.map_err(err)?;
                    if invalid_updates { return Err(format!("destination item updates exceed its checkpoint or precede creation for {id}")); }
                    retained.push(id.clone());
                }
            }
        }
        eprintln!("CODEXM_PUBLISH_BEFORE_COMMIT");
        std::io::stderr().flush().map_err(err)?;
        tokio::time::sleep(Duration::from_millis(request.pause_before_commit_ms)).await;
        if request.fail_before_commit { return Err("injected failure before commit".into()); }
        Ok((inserted, retained))
    }.await;
    match result {
        Ok((inserted, retained)) => {
            if let Err(error) = sqlx::query("COMMIT").execute(&mut connection).await {
                let _ = sqlx::query("ROLLBACK").execute(&mut connection).await;
                let _ = connection.close().await;
                return Err(format!("commit failed; verify destination before retry: {error}"));
            }
            connection.close().await.map_err(err)?;
            serde_json::to_string(&serde_json::json!({
                "status":"published", "diagnostic_only":true,
                "inserted_rollouts":inserted, "retained_rollouts":retained,
                "source_hashes_verified":false, "native_writer_exclusion":false,
                "filesystem_database_atomicity":false,
                "upstream_commit":"6ff1e1ca3b30fc8b914720681fcf5fe62e95f856"
            })).map_err(err)
        }
        Err(error) => {
            let rollback = sqlx::query("ROLLBACK").execute(&mut connection).await;
            let _ = connection.close().await;
            rollback.map_err(|failure| format!("{error}; rollback error: {failure}"))?;
            Err(error)
        }
    }
}
