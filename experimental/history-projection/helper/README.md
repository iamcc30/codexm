# Experimental native history projector

This is a local feasibility prototype, **not a production repair or import command**.
Only synthetic copied sessions may be used. No real profile data, auth, sessions,
MCP, app-server, model, fork/resume operations, or live destination writes are part
of the helper. A successful build/run does not establish production readiness.

The separate `publish_projection` diagnostic example now tests publication into
an **existing synthetic** history database. Its contract, marker requirement,
transaction/schema guards, failpoints and unresolved production boundaries are in
`PUBLISHER_DESIGN.md`. It does not change the original `projection_only` API.
Build it by adding `--example publish_projection` to the Cargo command below.
No publisher runtime/compile success is claimed until its independent CI passes.

## Pin and build

Use a disposable checkout of `openai/codex` at
`6ff1e1ca3b30fc8b914720681fcf5fe62e95f856` (`rust-v0.166.0`).

```
python helper/build_patch.py /absolute/disposable/codex --apply
cd /absolute/disposable/codex/codex-rs
cargo build --locked -p codex-thread-store \
  --features projection-only-experiment --example projection_only
target/debug/examples/projection_only < /absolute/synthetic-manifest.json
```

The source toolchain is Rust 1.95.0. Build dependencies include the upstream
workspace and its patched git dependencies, bundled SQLite, and zstd. This helper
adds optional sha2/tempfile dependencies already pinned in the upstream lockfile;
the patch also updates the thread-store lockfile dependency list. The release tag
sets workspace.package.version to 0.166.0 but leaves all 170 source-less workspace
lock entries at 0.0.0. The builder deterministically reconciles those local package
versions to their pinned manifests. Every tracked Cargo manifest is read from HEAD
and compared with its working copy; the exact 170 local package names must match
the lock. No registry/git dependency versions, sources or checksums are changed,
and no unlocked Cargo resolver is run. `--emit /path/patch.diff` optionally emits a reviewable diff.
Without `--apply`, the builder only validates sources (and optionally emits diff).
It requires the exact HEAD plus exact Git blob hashes for seven baseline files;
any mismatched anchor, existing helper file, or changed source fails closed.

## Manifest contract

```
{
  "source_root": "/absolute/synthetic-inputs",
  "output_dir": "/absolute/new-staging-output",
  "rollouts": [{
    "rollout_id": "physical-rollout-uuid",
    "path": "/absolute/synthetic-inputs/rollout-...jsonl",
    "end_byte_offset": 1234,
    "end_ordinal_exclusive": 8,
    "prefix_sha256": "lowercase-sha256-of-exact-included-prefix"
  }]
}
```

All source files must be read-only regular plain `.jsonl` files under source_root.
Reject symlinks in all path components, path escapes, unknown manifest fields,
duplicate physical IDs, filenames inconsistent with physical ID or logical
session owner, and compressed files. Source/output trees must be disjoint.
Output must not exist; its existing parent must have no symlinks.

The caller resolves lineage from synthetic metadata and supplies each explicit
prefix. For ancestors, both cutoffs must be the selected descendant's history_base
bookmark, never the ancestor's full tail. For the selected tip, use a complete
newline-terminated prefix. The helper does **not** infer lineage or completeness
of the supplied manifest. It validates each individual prefix only.

The helper reads exactly end_byte_offset bytes, checks SHA-256, and strictly
parses every record using the pinned upstream decoder. It rejects blanks, partial
lines, unknown/malformed variants, absent/regressed/gapped ordinals, malformed
timestamps, legacy format, or inconsistent final ordinal. Validation is
intentionally stricter than Codex's tolerant recovery projector. All prefixes are
validated before any SQLite open. Inputs are bounded to 64 MiB each, 128 MiB total,
1024 entries; stdin manifest is limited to 1 MiB.

After validation it creates private read-only prefix snapshots in a temporary
directory under output_dir. Only these snapshots are projected, so later original
source changes cannot affect projection and excluded tails are never read. The
temporary files are removed before a successful result. A failure can leave the
new output directory or partial staged DB; never reuse it or treat it as complete.

Stdout success is a JSON object with `status: "projected"`, `experimental: true`,
the exact upstream commit, `history_db`, explicit rollout hash/bound reports, and
`publication_performed: false`. A WAL TRUNCATE checkpoint and pool close finish
before success. Stderr plus nonzero exit denotes failure. Only the staging history
database is initialized; no StateRuntime is constructed. Final success output
should contain the history DB only, subject to the pinned SQLite layer's ordinary
bookkeeping; the acceptance harness must verify the actual file inventory.

## Why this shape

`LocalThreadStore::new(config, None)` only allocates state and writer-coordinator
objects; it does not acquire filesystem writer locks. The feature-only entrypoint
sets its dedicated history pool explicitly. Three existing state-DB guards permit
that pool only when this feature is compiled. Default builds preserve their
original state-DB gate. No public constructor grants general caller access to a
state-free writable store.

All materialization, projection, item conversion, SQL application, and checkpoints
remain upstream code. No event or SQL semantics are reimplemented. Physical
rollout IDs key projection rows; logical metadata IDs identify source owners.
This distinction matters after revert. Real Codex writer locks use the logical
thread ID, **not** the physical projection ID. This prototype does not acquire
either lock because it only accepts detached synthetic copies. It is not suitable
for concurrent live input, hostile filesystem mutation, or production publication.

StateRuntime initialization was deliberately avoided: upstream opens/migrates
state, logs, goals, memories, and queue databases, creates backfill state, and
starts reclamation/log maintenance. The dedicated history opener still performs
history-specific SQLite setup and migrations in the newly created staging dir.

The stable materializer has no explicit ancestor end-offset parameter. Copying
only the authenticated prefix makes its EOF the requested boundary, without
requiring a later upstream change such as #58959. That PR handles ancestor truncation/recovery in fork/revert, not a standalone
UI projection import API; this prototype does not depend on it.

## Verification status and required acceptance

Local checks: Python syntax, all seven source Git blob hashes, patch anchors,
generated Cargo TOML/lockfile parsing, expected dependency/feature declarations,
and rejection of a wrong upstream HEAD. All 171 tracked Cargo manifests were
retrieved at the pinned commit and checked against their Git tree blob hashes.
Tests verify exactly 170 expected local-version updates and unchanged external
package blocks. The original projector then compiled and passed the isolated
native proof in [CI run 41946567278](https://github.com/iamcc30/codexm/actions/runs/41946567278)
at commit a94a093ef252db0328d9e48bb6f5c56cf30c068f: all 13 native flow gates,
both complete paginated payload comparisons, and 12 rejection cases passed.
The new existing-database publisher is separate and still requires its first
Rust build and runtime CI. Neither experiment is production integration.

Required CI/synthetic checks: compile with --locked; fork plus revert with exact
physical/current mapping; bounded ancestor with forbidden tail marker; complete
native paginated traversal and full-item payload parity; no duplicate pages;
source byte/hash/metadata inventory unchanged; only staging history DB output;
idempotent independent rebuilds; malformed/unknown/partial/ordinal/hash/owner/
symlink/duplicate/legacy/compressed/writable-input/existing-output rejection.
Run rejection cases before DB initialization and verify no output_dir exists.

Production work remains separate: compressed prefixes, trusted live snapshots,
cross-process writer coordination, selected-rollout authority, resource scaling,
version negotiation, schema compatibility, atomic bounded import/refresh, error
recovery, compatibility with normal Codex appends, and broad protocol regression
coverage. Do not merge a production solution on the strength of this prototype.
