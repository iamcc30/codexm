//! Synthetic diagnostic of the pinned public native writer coordinator.
//! This deliberately retains native stale-lock cleanup. It is not a repair API.
use std::io::{BufRead, Write};
use std::path::{Component, Path, PathBuf};
use std::sync::{Arc, mpsc};
use std::time::Duration;

use codex_protocol::ThreadId;
use codex_rollout::WriterLockCoordinator;
use serde::Deserialize;
use serde_json::json;

const MARKER: &str = "codexm synthetic native lock diagnostic\n";
const PIN: &str = "6ff1e1ca3b30fc8b914720681fcf5fe62e95f856";

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct Request {
    synthetic_test_root: PathBuf,
    codex_home: PathBuf,
    owner_ids: Vec<String>,
    #[serde(default = "default_hold_ms")]
    max_hold_ms: u64,
}

fn default_hold_ms() -> u64 { 60_000 }
fn err(value: impl std::fmt::Display) -> String { value.to_string() }

fn checked(path: &Path) -> Result<PathBuf, String> {
    if !path.is_absolute() { return Err("absolute paths required".into()); }
    let mut current = PathBuf::new();
    for component in path.components() {
        if matches!(component, Component::ParentDir | Component::CurDir) {
            return Err("dot path components forbidden".into());
        }
        current.push(component.as_os_str());
        if std::fs::symlink_metadata(&current).map_err(err)?.file_type().is_symlink() {
            return Err("symlink path component forbidden".into());
        }
    }
    std::fs::canonicalize(path).map_err(err)
}

fn regular_single_link(path: &Path) -> Result<(), String> {
    let metadata = std::fs::symlink_metadata(path).map_err(err)?;
    if !metadata.file_type().is_file() { return Err("regular files required".into()); }
    #[cfg(unix)]
    {
        use std::os::unix::fs::MetadataExt;
        if metadata.nlink() != 1 { return Err("hardlinked files forbidden".into()); }
    }
    Ok(())
}

fn bounded_line(input: &mut impl BufRead, max: usize) -> Result<String, String> {
    let mut bytes = Vec::new();
    loop {
        let available = input.fill_buf().map_err(err)?;
        if available.is_empty() { return Err("newline-terminated input required".into()); }
        let length = available.iter().position(|value| *value == b'\n')
            .map_or(available.len(), |index| index + 1);
        if bytes.len() + length > max { return Err("input line exceeds diagnostic limit".into()); }
        let done = available[length - 1] == b'\n';
        bytes.extend_from_slice(&available[..length]);
        input.consume(length);
        if done { return String::from_utf8(bytes).map_err(err); }
    }
}

fn emit(value: serde_json::Value) -> Result<(), String> {
    let mut output = std::io::stdout().lock();
    writeln!(output, "{value}").map_err(err)?;
    output.flush().map_err(err)
}

/// Return 0 after explicit release, 3 for native WouldBlock, 4 after hold expiry.
/// Other errors are reported by the example with exit 1. No SQLite or rollout is opened.
pub fn run<R: BufRead + Send + 'static>(mut input: R) -> Result<i32, String> {
    let request: Request = serde_json::from_str(&bounded_line(&mut input, 16 * 1024)?).map_err(err)?;
    if request.owner_ids.is_empty() || request.owner_ids.len() > 8 {
        return Err("1..=8 explicit logical owner ids required".into());
    }
    if !(1000..=120_000).contains(&request.max_hold_ms) {
        return Err("max_hold_ms must be 1000..=120000".into());
    }
    let root = checked(&request.synthetic_test_root)?;
    if !root.is_dir() || !root.file_name().and_then(|v| v.to_str())
        .is_some_and(|v| v.starts_with("codexm-native-acceptance-")) {
        return Err("native acceptance synthetic root required".into());
    }
    let marker = checked(&root.join("CODEXM_SYNTHETIC_NATIVE_LOCK_TEST"))?;
    regular_single_link(&marker)?;
    if std::fs::metadata(&marker).map_err(err)?.len() != MARKER.len() as u64
        || std::fs::read_to_string(marker).map_err(err)? != MARKER {
        return Err("explicit synthetic native lock marker required".into());
    }
    let home = checked(&request.codex_home)?;
    if home == root || !home.starts_with(&root) || !home.is_dir() {
        return Err("Codex home must be a directory strictly under synthetic root".into());
    }
    let env_home = checked(&PathBuf::from(std::env::var_os("HOME").ok_or("HOME required")?))?;
    if env_home == root || !env_home.starts_with(&root) || !env_home.is_dir() {
        return Err("HOME must be isolated under synthetic root".into());
    }
    let env_codex_home = checked(&PathBuf::from(std::env::var_os("CODEX_HOME")
        .ok_or("CODEX_HOME required")?))?;
    if env_codex_home != home { return Err("CODEX_HOME must match request".into()); }
    let cwd = checked(&std::env::current_dir().map_err(err)?)?;
    if !cwd.starts_with(&root) { return Err("working directory must be synthetic".into()); }
    let lock_dir = home.join("thread-writer-locks");
    match std::fs::symlink_metadata(&lock_dir) {
        Ok(_) => {
            let directory = checked(&lock_dir)?;
            if !directory.is_dir() { return Err("native lock directory required".into()); }
            for entry in std::fs::read_dir(directory).map_err(err)? {
                regular_single_link(&entry.map_err(err)?.path())?;
            }
        }
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
        Err(error) => return Err(err(error)),
    }
    let mut owners = request.owner_ids;
    for owner in &owners {
        let id = ThreadId::from_string(owner).map_err(err)?;
        if id.to_string() != *owner { return Err("canonical lowercase UUIDs required".into()); }
    }
    owners.sort();
    if owners.windows(2).any(|pair| pair[0] == pair[1]) {
        return Err("duplicate logical owner id".into());
    }
    // No hand-written flock, file deletion or special production acquire path.
    // The public native coordinator's first acquisition may remove stale files.
    let coordinator = Arc::new(WriterLockCoordinator::new(&home));
    let mut guards = Vec::new();
    for owner in &owners {
        match coordinator.acquire(ThreadId::from_string(owner).map_err(err)?) {
            Ok(guard) => guards.push(guard),
            Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {
                let partial_count = guards.len();
                drop(guards);
                emit(json!({"status":"blocked", "owner_id":owner,
                    "native_error_kind":"WouldBlock", "released_partial_count":partial_count,
                    "diagnostic_only":true, "native_global_exclusion":false,
                    "native_stale_cleanup_enabled":true, "upstream_commit":PIN}))?;
                return Ok(3);
            }
            Err(error) => return Err(err(error)),
        }
    }
    emit(json!({"status":"acquired", "owner_ids":owners,
        "diagnostic_only":true, "native_global_exclusion":false,
        "native_stale_cleanup_enabled":true, "upstream_commit":PIN,
        "max_hold_ms":request.max_hold_ms}))?;
    let (sender, receiver) = mpsc::channel();
    std::thread::spawn(move || { let _ = sender.send(bounded_line(&mut input, 32)); });
    let release = receiver.recv_timeout(Duration::from_millis(request.max_hold_ms));
    // Release on every outcome. A bounded reader thread can remain blocked on stdin
    // until this diagnostic process exits; it never owns a native guard.
    drop(guards);
    match release {
        Ok(Ok(command)) if command == "release\n" => {
            emit(json!({"status":"released", "owner_ids":owners,
                "diagnostic_only":true, "native_global_exclusion":false}))?;
            Ok(0)
        }
        Err(mpsc::RecvTimeoutError::Timeout) => {
            emit(json!({"status":"expired", "owner_ids":owners,
                "diagnostic_only":true, "native_global_exclusion":false}))?;
            Ok(4)
        }
        Ok(Ok(_)) => Err("only literal release followed by newline is accepted".into()),
        Ok(Err(error)) => Err(error),
        Err(error) => Err(err(error)),
    }
}
