#!/usr/bin/env python3
"""Verify a pinned disposable Codex checkout, then emit/apply the experimental patch."""
import argparse
import difflib
import hashlib
import json
import pathlib
import subprocess

PIN = "6ff1e1ca3b30fc8b914720681fcf5fe62e95f856"
HERE = pathlib.Path(__file__).resolve().parent
FEATURE = "projection-only-experiment"


def replace_once(text, old, new):
    if text.count(old) != 1:
        raise ValueError(f"Expected exactly one patch anchor: {old!r}")
    return text.replace(old, new, 1)


def build(root):
    head = subprocess.check_output(["git", "-C", str(root), "rev-parse", "HEAD"], text=True).strip()
    if head != PIN:
        raise ValueError(f"Wrong upstream HEAD: {head}; expected {PIN}")
    pins = json.loads((HERE / "upstream-blobs.json").read_text())
    originals = {}
    for name, expected in pins.items():
        path = root / name
        if path.is_symlink():
            raise ValueError(f"Symlink source rejected: {name}")
        raw = path.read_bytes()
        actual = hashlib.sha1(b"blob " + str(len(raw)).encode() + b"\0" + raw).hexdigest()
        if actual != expected:
            raise ValueError(f"Upstream blob mismatch: {name}: {actual}")
        originals[name] = raw.decode()
    changed = dict(originals)
    mod = "codex-rs/thread-store/src/local/mod.rs"
    changed[mod] = replace_once(changed[mod], "mod archive_thread;", '''#[cfg(feature = "projection-only-experiment")]
mod projection_only;
#[cfg(feature = "projection-only-experiment")]
pub use projection_only::run_projection_only;

mod archive_thread;''')
    changed[mod] = replace_once(changed[mod], "    async fn thread_history_db(&self) -> ThreadStoreResult<&sqlx::SqlitePool> {\n        if self.state_db.is_none() {", '''    // Normal builds retain the original state-DB gate. The experimental entrypoint
    // explicitly supplies a staging-only pool without creating StateRuntime.
    fn has_history_projection_database(&self) -> bool {
        self.state_db.is_some()
            || (cfg!(feature = "projection-only-experiment")
                && self.thread_history_db.get().is_some())
    }

    async fn thread_history_db(&self) -> ThreadStoreResult<&sqlx::SqlitePool> {
        if !self.has_history_projection_database() {''')
    for suffix in ["thread_history.rs", "thread_history_materialization.rs"]:
        name = "codex-rs/thread-store/src/local/" + suffix
        changed[name] = replace_once(changed[name], "    if store.state_db.is_none() {", "    if !store.has_history_projection_database() {")
    lib = "codex-rs/thread-store/src/lib.rs"
    changed[lib] = replace_once(changed[lib], "pub use local::LocalThreadStore;", '''#[cfg(feature = "projection-only-experiment")]
pub use local::run_projection_only;
pub use local::LocalThreadStore;''')
    cargo = "codex-rs/thread-store/Cargo.toml"
    changed[cargo] = replace_once(changed[cargo], "[dependencies]\n", '''[features]
projection-only-experiment = ["dep:sha2", "dep:tempfile"]

[[example]]
name = "projection_only"
path = "examples/projection_only.rs"
required-features = ["projection-only-experiment"]

[dependencies]
sha2 = { workspace = true, optional = true }
tempfile = { workspace = true, optional = true }
''')
    lock = "codex-rs/Cargo.lock"
    start = changed[lock].index('name = "codex-thread-store"\n')
    end = changed[lock].index('\n[[package]]', start)
    section = changed[lock][start:end]
    patched = replace_once(section, ' "serde_json",\n', ' "serde_json",\n "sha2 0.10.9",\n')
    changed[lock] = changed[lock][:start] + patched + changed[lock][end:]
    for target, source in [
        ("codex-rs/thread-store/src/local/projection_only.rs", "projection_only.rs"),
        ("codex-rs/thread-store/examples/projection_only.rs", "projection_only_example.rs"),
    ]:
        if (root / target).exists() or (root / target).is_symlink():
            raise ValueError(f"Refusing to overwrite new helper path: {target}")
        originals[target] = ""
        changed[target] = (HERE / source).read_text()
    return originals, changed


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("checkout", type=pathlib.Path, help="disposable upstream Codex checkout")
    parser.add_argument("--apply", action="store_true", help="apply after all pins/anchors validate")
    parser.add_argument("--emit", type=pathlib.Path, help="optional unified diff output file")
    args = parser.parse_args()
    root = args.checkout.resolve(strict=True)
    originals, changed = build(root)
    diff = "".join("".join(difflib.unified_diff(
        originals[name].splitlines(keepends=True), text.splitlines(keepends=True),
        fromfile="a/" + name if originals[name] else "/dev/null", tofile="b/" + name,
    )) for name, text in changed.items())
    if args.emit:
        args.emit.write_text(diff)
    if args.apply:
        for name, text in changed.items():
            target = root / name
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_text(text)
    print(json.dumps({"upstream_commit": PIN, "verified": True, "applied": args.apply,
                      "changed_files": list(changed), "patch_bytes": len(diff.encode())}))


if __name__ == "__main__":
    main()
