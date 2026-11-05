# Diagnostic existing-database publication proof

This is a separate experiment. It does not enable production integration, read
real sessions, change the passing projector API, or establish live writer
exclusion. Build the additional `publish_projection` example under the existing
`projection-only-experiment` feature.

## API

The executable reads at most 1 MiB of JSON from stdin:

```
{
  "synthetic_test_root": "/absolute/synthetic-test",
  "staging_db": "/absolute/synthetic-test/stage/thread_history_1.sqlite",
  "target_db": "/absolute/synthetic-test/target/thread_history_1.sqlite",
  "rollouts": [{
    "rollout_id": "physical-rollout-uuid",
    "end_byte_offset": 1234,
    "end_ordinal_exclusive": 8
  }],
  "busy_timeout_ms": 250,
  "fail_before_commit": false,
  "pause_before_commit_ms": 0
}
```

Both DBs must already exist under the explicit test root and be distinct files.
Staging must be a closed private snapshot with no WAL, SHM or journal sidecar and
must stay immutable for the call. It is attached using mode=ro&immutable=1 to avoid
read-side creation of WAL/SHM. Never use immutable mode for the target.
The root must contain a regular marker file `CODEXM_SYNTHETIC_PROJECTION_TEST`
whose complete contents are `codexm synthetic projection publication\n`.
Reject symlink components, hardlinked databases/SQLite sidecars on Unix,
noncanonical/duplicate UUIDs, invalid bounds and unknown JSON fields. Busy/pause
limits are 0..5000 ms. The tests are intended for Linux synthetic CI.

The harness must generate staging using the passing native projector and verify
source-prefix hashes separately. This publisher does not read JSONL or claim its
own source authentication. Supplying a marker does not grant production authority.

Success stdout:

```
{
  "status": "published",
  "diagnostic_only": true,
  "inserted_rollouts": ["..."],
  "retained_rollouts": ["..."],
  "source_hashes_verified": false,
  "native_writer_exclusion": false,
  "filesystem_database_atomicity": false,
  "upstream_commit": "6ff1e1ca3b30fc8b914720681fcf5fe62e95f856"
}
```

After all transaction writes, stderr emits `CODEXM_PUBLISH_BEFORE_COMMIT`, then
flushes, optionally sleeps, optionally fails, and otherwise commits. The harness
can kill the process during the pause to verify rollback. All test controls are
only in this experimental entrypoint. Failure prints stderr and returns nonzero.
Commit/close I/O failure or missing stdout must not be assumed to mean no commit;
inspect the database before retrying. A repeated successful request is a no-op.

## Transaction and schema invariants

1. Initialize a reference database **in memory only** with the pinned upstream
   thread-history SQLx migrator. Never run a migrator against target or staging.
2. Read-only preflight compares both DBs to the reference: complete sqlite_schema
   rows (tables, indices, triggers, SQL), plus migration version, description,
   success and checksum. Ignore installation timestamp/execution duration only.
   Reject missing, extra, newer or checksum-mismatched migrations and schema drift.
3. Open target with create_if_missing(false), bounded busy timeout and synchronous
   FULL, without requesting a journal-mode change. Attach staging via mode=ro&immutable=1.
   BEGIN IMMEDIATE serializes all target writes; repeat schema checks inside it.
4. For every selected physical ID require exact staging checkpoint bounds and
   staging row creation ordinals inside those bounds. If target has no checkpoint,
   require no rows for that ID in all three data tables, then insert all explicit
   columns and the checkpoint. No REPLACE, DELETE, UPDATE, schema change or migration.
5. Existing exact checkpoints are accepted only when every prefix row matches in
   both directions. Existing checkpoints strictly ahead in both bytes and ordinals
   are accepted using the same comparison, leaving all rows/checkpoints unchanged.
   Reject target rows outside its own checkpoint. In both DBs also require item
   update ordinal >= creation ordinal and strictly below that DB's checkpoint.
   This conservatively rejects older mixed-writer zero-update defaults when they
   precede creation; it does not invent or repair missing update metadata.
   Reject partial/orphan partitions.
6. Compare all columns of thread_turns, thread_items and thread_realtime_items.
   Destination scope is creation rollout_ordinal < requested cutoff. Never filter
   by updated_at_ordinal, which could conceal changes to an older visible item.
   A later old-item payload update or later completion/summary mutation causes
   rejection. Completed-turn later tails with unchanged prefix remain intact.
7. One transaction covers all selected IDs and checkpoints. A late mismatch or
   test injection rolls back earlier inserts. Unrelated IDs and migration/schema
   records are untouched. Staging is never a writable transaction participant.
8. Close the connection after commit. Do not replace the database file, force a
   WAL truncation/checkpoint, or copy WAL files into an existing target. Normal
   SQLite journal/WAL side effects are expected; preservation refers to logical
   unselected records, not byte-identical target database files.

## Why native locks do not complete production safety

The public `codex_rollout::WriterLockCoordinator::new(codex_home)` constructor
only allocates state. Its `Arc<...>.acquire(logical_thread_id)` method uses the same
cross-process lock namespace as native resume, revert, archive and delete without
starting any session, MCP, config hooks or StateRuntime. Projection rows instead
use physical rollout IDs; these differ after revert.

However, first acquire also removes unrelated stale lock files. A minimally
invasive production helper needs a pinned no-cleanup acquire path, or a dedicated
publication guard that does not scan/remove other lock files. Acquiring multiple
logical owners must use stable ordering and fail/release on contention.

More importantly, paginated_fork::prepare can materialize ancestors/source under
in-process LiveWriterLocks without acquiring the corresponding cross-process
writer ownership. Therefore owner locks alone do not exclude every native
projector. SQLite serialization and native checkpoint CAS prevent overwritten
rows, but a concurrent native fork can fail after observing a stale checkpoint.
This experiment does not establish operational compatibility with live clients.

Native open_thread_history_db also is not a version guard: runtime migrators set
ignore_missing=true to accept newer migrations. The new diagnostic instead uses
an exact in-memory reference before opening an existing target writable.

## Production stages still required

1. Pass synthetic existing-DB tests: unrelated sentinels, missing partition insert,
   exact no-op rerun, compatible ahead tail, changed old item/turn rejection,
   orphan/incomplete partition, wrong bounds, drift in both staging and target,
   unknown/checksum-failed migration, busy timeout, late-batch rollback and process
   death before commit. Re-run the original 13-flow native proof unchanged.
2. Add explicit, source-bound destination manifest validation, immutable bounded
   snapshot handling, selected-rollout metadata checks and cross-process ownership
   experiments. Rehash destination prefixes under the applicable owner locks.
3. Prove sync publication order across JSONL, thread metadata and history. A single
   SQLite transaction cannot atomically publish separate filesystem files plus
   state/history databases. Design a recoverable protocol before product wiring.
4. Handle newer binaries/schema, compressed input, upgrades/packaging and supported
   busy cases explicitly. Never truncate ahead projections or assume smaller
   checkpoint replacement is safe. Unsupported mid-turn/changed-prefix cases must
   remain conflicts until a source-supported historical-view solution is tested.

## Sources

All paths refer to upstream commit 6ff1e1ca3b30fc8b914720681fcf5fe62e95f856:

- codex-rs/rollout/src/writer_lock.rs: native ownership, global coordination,
  stale-lock cleanup and guard lifecycle.
- codex-rs/thread-store/src/local/live_writer.rs and revert_thread.rs: logical
  owner locks versus physical rollout materialization keys.
- codex-rs/thread-store/src/local/paginated_fork.rs: projection path without full
  cross-process projector exclusion.
- codex-rs/thread-store/src/local/thread_history/segment_paging.rs: visible rows
  scoped by creation rollout_ordinal, not update ordinal.
- codex-rs/thread-store/src/local/thread_history.rs: all row fields, immutable
  terminal turns, item updates, transaction/checkpoint CAS.
- codex-rs/state/src/migrations.rs: ignore_missing runtime behavior.
- codex-rs/state/thread_history_migrations/0001 through 0008: exact schema,
  including realtime cleanup trigger on checkpoint deletion.

The publisher compiled and passed [CI run 41949899004](https://github.com/iamcc30/codexm/actions/runs/41949899004)
at experiment commit f2e5742: all 24 publisher checks and all 13 native flow gates
passed. The local executor still lacks Rust; Python checks here are supplemental
SQL-level checks, not a replacement for that CI evidence. Native-lock and recovery
experiments remain separate. This result does not establish production readiness,
complete native-writer exclusion, or crash-atomic filesystem-plus-database sync.

## Experimental same-physical-rollout advancement (not yet native-CI verified)

A behind target is still rejected by default. Optional `advance_source_root`
inside the synthetic root and per-rollout `advance` enable a stronger proof:

- `source_path`: read-only canonical full newer JSONL snapshot under that root.
- `destination_path`: canonical existing synthetic rollout with the same filename.
- `base_end_byte_offset`, `base_end_ordinal_exclusive`: exact old checkpoint.
- `base_prefix_sha256`: SHA-256 of both source and destination old prefix.
- `prefix_sha256`: SHA-256 of the newer source prefix at the rollout's existing
  top-level `end_byte_offset` / `end_ordinal_exclusive` bounds.

Before opening the target writable, the helper invokes the unchanged native
projector for both old and newer prefixes into disposable private DBs. The
original strict decoder, bounds, hashes, filename/owner and ordinal checks apply.
Inside BEGIN IMMEDIATE, the supplied stage must equal the rebuilt newer
partition in every column and checkpoint. A behind target must exactly equal the
rebuilt old partition in every column and checkpoint, without prefix filtering.
Only then are that physical ID's three row sets replaced and checkpoint advanced
in the same transaction. Unrelated partitions and compatible ahead partitions
retain their previous behavior. Success adds `advanced_rollouts`; retry after a
committed advancement uses the unchanged retention path.

This permits native changes to prior item payloads and turn status during a real
continuation without accepting unexplained target drift. It does not trust an
old checkpoint alone. A malformed/unknown record, source/destination hash
mismatch, staged payload mismatch, missing/extra/modified target row, or partial
checkpoint fails closed. Destination state must be either exactly the old prefix (matching its hash),
or at least the full newer bound (matching its full-prefix hash). Intermediate
lengths and divergent appends are rejected; compatible longer tails are never
rewritten. Canonical physical identity and the source/destination filename must
match. This destination check repeats within the DB
transaction, but this is not a filesystem lock or concurrent-writer guarantee.
Caller-supplied hashes bind this synthetic experiment, not production authority.

Local `python -m unittest discover -s helper/tests -v` exercises seven SQL-only
cases using the exact migration schema and SQL fragments extracted from the Rust
source: equality, changed/missing/extra old rows, changed staging rows, unrelated
row preservation and transaction rollback. These are not Rust compilation or
native acceptance. The previously passing CI predates this extension.
