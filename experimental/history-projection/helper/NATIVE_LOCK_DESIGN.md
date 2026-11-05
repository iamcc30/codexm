# Synthetic native writer-lock boundary diagnostic

This separate experiment measures the public writer-lock coordinator in exact
upstream Codex 0.166.0 commit
`6ff1e1ca3b30fc8b914720681fcf5fe62e95f856`. It does not integrate with codexm,
publish projections, authorize access to real sessions, or establish production
repair safety. In particular, a successful run **must** report
`native_global_exclusion: false` and the measured native fork bypass.

## Minimal implementation and build

Use the existing optional `projection-only-experiment` feature. Add an example
named `native_lock_probe`, with these file mappings:

- `native_lock_probe_example.rs` to `codex-rs/thread-store/examples/native_lock_probe.rs`
- `native_lock_probe.rs` to `codex-rs/thread-store/examples/native_lock_probe_support/mod.rs`

The example imports its support module directly. No thread-store library export,
new dependency, `WriterLockCoordinator` change, hand-written `flock`, or harness
unlink operation is required. The support module calls the public upstream
`WriterLockCoordinator::new` and `Arc<...>.acquire` directly, preserving native
coordination and guard-drop behavior.

## Executable protocol

Input is a bounded newline-terminated JSON object followed later by a command:

```json
{"synthetic_test_root":"/tmp/codexm-native-acceptance-...","codex_home":"/tmp/codexm-native-acceptance-.../native-lock-probe/copy","owner_ids":["canonical-logical-thread-uuid"],"max_hold_ms":120000}
```

The root must contain a regular single-link file named
`CODEXM_SYNTHETIC_NATIVE_LOCK_TEST` with exact contents
`codexm synthetic native lock diagnostic\n`. The root name must begin with
`codexm-native-acceptance-`. `HOME`, current working directory, and requested
`CODEX_HOME` must be within that synthetic root; the environment's `CODEX_HOME`
must equal the request. Paths must be absolute, existing, and free of symlink
components. Existing entries in the native lock directory must be regular
single-link files. These checks do not defend against an adversary concurrently
replacing the filesystem, and a marker never grants production authority.

The helper rejects unknown fields, noncanonical UUIDs, duplicates, more than
eight owners, an input line exceeding 16 KiB, and hold durations outside
1000..120000 ms. The default is 60000 ms. Owners are sorted before acquisition.
It does not open a rollout or any SQLite database, load session configuration,
start a model session, execute hooks, or initialize MCP.

After acquiring all guards, stdout emits and flushes one JSON line:

```json
{"status":"acquired","owner_ids":["..."],"diagnostic_only":true,"native_global_exclusion":false,"native_stale_cleanup_enabled":true,"upstream_commit":"6ff1e1ca3b30fc8b914720681fcf5fe62e95f856","max_hold_ms":120000}
```

The guard remains held until the literal `release\n`, input error/EOF, or the
bounded hold deadline. Explicit release emits `status: released` and exits 0.
Expiry emits `status: expired` and exits 4. Other protocol/preflight failures
write stderr and exit 1. If acquisition returns native `WouldBlock`, all earlier
guards are dropped, stdout emits `status: blocked`, the contended `owner_id`,
`native_error_kind: WouldBlock`, and `released_partial_count`, and the helper
exits 3. An arbitrary I/O error is never classified as writer contention.

The lifetime bound starts after all guards are acquired. Upstream coordination
acquisition and guard drop can each wait up to 30 seconds; this diagnostic does
not change those native waits. The Python owner also imposes process deadlines
and releases or kills **its own** helper in `finally` cleanup. It does not kill
live native writers to obtain a lock or manually remove their lock files.

## Harness integration

Before the original `second-resume` flow, while the original source writer is
closed, call:

```python
run_native_lock_checks(binary, codex, app_server_class, source_home,
                       child_id, child_name, root, env, cwd, artifacts, checks)
```

Pass the saved unmodified native `AppServer` class, not the probe subclass. The
function creates its own marker and distinct copies below `native-lock-probe`.
It copies only the synthetic sessions directory and the verified localhost-only
mock-provider config. It never copies a native state/history DB, WAL, auth file,
MCP config, or hooks. `HOME` stays synthetic and the environment is allowlisted.
Every native session uses the real pinned app-server with the existing mock
localhost provider. All native server context managers close their processes.

Explicit path parameters refer only to copied rollout files. Native resume/fork
reconstruct metadata in that same new home, so no state database can retain an
absolute rollout path into the original fixture. The final report proves the
original source profile's file hashes, modes, sizes, mtime, inode and device
inventory stayed unchanged. The original 13 native-flow gates continue separately.

## Expected checks and classifications

Ordinary diagnostic boundary checks must pass:

1. Reject duplicate owners, unknown fields, unbounded holds, mismatched homes,
   and an incorrect marker before creating any lock state.
2. A real live native resume makes helper acquisition return native `WouldBlock`.
3. Contention on the second sorted owner releases the first owner's guard,
   permitting another helper to acquire it immediately.
4. A held logical guard causes a separate native resume RPC to fail with that
   owner's exact `already has an active writer` error, not an unrelated failure.
   Source rollout bytes remain unchanged during the failed resume.
5. Native resume succeeds after explicit guard release and owns that lock again.
6. The diagnostic hold expires and releases its lock; abrupt helper exit also
   permits native reacquisition without any lock-file deletion by the harness.
7. The original acceptance fixture inventory remains unchanged.

Three checks deliberately reproduce limitations. They are classified as
`known_limitation_observed`, not production acceptance successes:

- After revert, holding the **physical rollout UUID** permits native resume of
  the distinct **logical owner UUID**. A second logical-owner contender still
  sees `WouldBlock`, proving the native writer really acquired its logical lock.
- The first public native acquire removes an unrelated stale UUID lock file.
  A non-lock sentinel remains. This is intentional measurement of upstream
  behavior, not an implementation choice to clean production locks.
- An ordinary native fork with `lastTurnId` rebuilds a missing ancestor
  projection while **all logical owners in that source lineage** remain held.

The fork test first bootstraps a dedicated copy by native fork through a visible
completed ancestor turn, closes that native process, and records the ancestor's
complete rows across turns, items, realtime items, and projection checkpoint.
It then deletes **only that synthetic ancestor partition**, holds all source
logical guards, and starts a second native app-server. It requires the partition
to remain absent after initialization, so startup work cannot be misattributed to
fork. The fork must recreate the exact checkpoint and all rows, with nonempty
turns/items, while the helper stays alive. A second helper must still get native
`WouldBlock` for every held owner after projection. Every preexisting copied
source JSONL must remain unchanged; the new native fork may create its own files.

If a fork merely succeeds without the expected ancestor writes, the test fails.
If the fork is excluded, produces different rows, rewrites source bytes, or an
owner guard is no longer held, the test fails for reassessment. A changed native
behavior is not silently reclassified as compatible.

`native-lock-summary.json` records success/failure, exact case classifications,
observed errors, reconstructed row counts and these top-level booleans:

- `native_global_exclusion: false` (always)
- `stale_lock_cleanup_observed`
- `fork_ancestor_projection_bypass_observed`
- `original_fixture_unchanged`

Individual helper stdout/stderr and native RPC logs are saved with
`native-lock-` prefixes. None of this test's success classifications satisfy a
production concurrent-writer safety gate.

## Source-supported interpretation

Pinned source:

- `codex-rs/rollout/src/writer_lock.rs`, blob
  `31068707a2731303ec48a0cfcf154923d8e75eeb`: `.coordination.lock` protects native
  lock creation/removal; first acquire scans and removes stale UUID lock files.
- `codex-rs/thread-store/src/local/live_writer.rs`: resume acquires logical owner
  cross-process ownership before installing a recorder.
- `codex-rs/thread-store/src/local/paginated_fork.rs`, blob
  `09a3eede89a1a760c50bb15fcc9639860eb376b7`: non-latest fork materializes ancestor
  projections under in-process `LiveWriterLocks`, without corresponding native
  cross-process writer ownership. Physical rollout IDs are projection keys.
- `codex-rs/app-server-protocol/src/protocol/v2/thread.rs`: `path` and
  `lastTurnId` are supported native fork parameters.

A narrower no-cleanup acquire API could avoid the unrelated stale-file side
effect, but cannot close the fork projection gap. Therefore this diagnostic
does not patch native locking or propose owner guards as a complete live repair
protocol. Filesystem/database publication ordering, concurrent checkpoint races,
other projector paths, version compatibility and production recovery remain
separate unproven stages.

## Validation status

Python syntax and isolated harness utility checks can run in this executor. Rust
compilation and all real native boundary assertions require CI because the
executor has neither Rust nor the pinned native binaries. Do not describe those
runtime observations as established until the exact-commit CI run completes.
