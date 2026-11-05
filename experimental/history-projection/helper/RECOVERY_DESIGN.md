# Diagnostic quiescent-profile recovery proof

This is an isolated experiment, not a production recovery implementation. It does
not authorize a repair or merge, relax the existing 13-flow native acceptance or
24 publisher checks, edit native profiles, acquire native writer locks, or publish
anything outside disposable caller-created synthetic fixtures.

The executable test adapter is `harness/recovery_checks.py`. It exercises the
existing pinned Rust `publish_projection` example without modifying that API.
Local `--self-test` uses a deliberately small Python SQLite transaction model;
passing it is **not** evidence of Rust, SQLx, native schema, or native UI behavior.
Native CI must run the same recovery suite with the real built publisher.

## Explicit limits

- Direct native Codex ignores our journal and cooperative gate. Native fork and
  projector writers are not excluded. These tests require a quiescent synthetic
  profile and codexm-controlled startup, not arbitrary native concurrency.
- No transaction spans JSONL files, history SQLite, and metadata. There is **no
  cross-resource crash atomicity**. Intermediate files/history may be visible
  before metadata; a cooperating launcher stays blocked until verified completion.
- `synthetic-registry.json` is a test metadata/registry stand-in. This does not
  exercise native state.sqlite metadata, real codexm registry format, or their
  transactions. Their real adapters, schemas, and failure cases remain unproven.
- Publication creates missing JSONL files only. An existing file is retained only
  if its entire previously bound bytes are unchanged and its bounded prefix
  matches. Recovery of existing-file append, replacement, update, truncation, or
  competing native writes remains unproven. The opaque retained-tail test proves
  byte preservation, not validity of that tail as a native rollout.
- The harness survives outside each killed adapter and supplies the original
  trusted plan again. It does **not** prove trusted-plan authority/bootstrap after
  loss of that caller or a whole-machine restart. A real durable plan-loading and
  authority design is still required. Never bootstrap authority from arbitrary
  journal paths, hashes, or a self-written completion marker.
- Faults are actual process SIGKILLs, including partially written temporary files.
  They are not power cuts, disk corruption, fsync failure, ENOSPC, network-filesystem
  behavior, or a formal proof of persistence ordering under every filesystem.
  The implementation uses file/directory fsync and Linux renameat2, but those
  assumptions require separate validation on supported deployment platforms.

## Test hook and isolation

```
run_recovery_checks(
    publisher_binary, source_db, manifest, root, env, artifacts, checks
)
```

This matches the existing `probe.prepare_projection` fixture arguments. The
caller must have created a root named `codexm-native-acceptance-*` and a regular
`CODEXM_SYNTHETIC_PROJECTION_TEST` marker containing exactly
`codexm synthetic projection publication\n`. All mutable protocol targets are
under `root/recovery-probe/<case>/`; the caller receives `recovery-cases.json` in
its existing artifacts directory. Input snapshots are copied only from closed,
synthetic fixtures. The source DB and the original read-only JSONL input bytes
must remain unchanged.

Each case has fixed paths, not journal-selected general-purpose destinations:

```
<case>/source/<canonical-rollout-name>.jsonl   read-only, exact bounded prefix
<case>/stage.sqlite                          closed projector snapshot
<case>/profile/thread_history_1.sqlite       existing synthetic target
<case>/profile/sessions/<same-rollout-name>   create-only or retained
<case>/profile/synthetic-registry.json       metadata stand-in
<case>/profile/codexm-recovery.lock           cooperative flock gate
<case>/profile/codexm-recovery/<operation-id>/
```

Reject unknown plan and rollout fields, noncanonical/duplicate UUIDs, invalid
bounds, symlink components, hardlinks, dot-path escapes, mismatched names, sibling
resources, unsafe SQLite sidecars, and paths outside this fixed layout. A marker
alone never grants production authority. No real session directories are read.

## Bound intent and invariants

The operation ID is SHA-256 of a canonical, complete JSON plan. It includes:

1. Every read-only source prefix's exact byte length, SHA-256, physical rollout ID,
   destination path, and exclusive ordinal bound. Prefixes must be newline-ended
   and contain contiguous ordinals ending at that bound. Native record semantics
   are inherited from the already-passing native projector fixture, not replaced
   by the Python adapter's JSON checks.
2. Exact staging DB SHA-256 and path. This binds the passed native projection
   artifact; it does not independently re-prove projector provenance from JSONL.
3. Each destination's exact full-file preimage hash/length, or explicit absence.
   New files' intended exact bytes equal the bound prefix. Existing matching files
   keep their entire bound tails unchanged.
4. Target DB path and device/inode, initial whole-file hash, and selected logical
   partition fingerprints. The initial whole-file hash records preparation, not
   a whole-DB compare-and-set requirement. Independent unrelated logical writes
   must survive. A replacement DB inode is a conflict even if bytes look equal.
5. Registry path, initial byte hash, selected entries' preimages, and deterministic
   desired selected values derived from paths, hashes, and bounds. Unselected
   entries are re-read and merged, never restored from the initial snapshot.

The original caller plan is the authority. Replay compares `intent.json` exactly
against that plan and recomputes its content ID. Controlled startup similarly
requires a caller-bound plan, validates it, and checks every actual resource.
Journals are evidence, not executable instructions or proof of completion.

## Publication and recovery ordering

All steps run while holding the exclusive cooperative profile flock:

1. Validate the marker, fixed layout, immutable input bytes, staging snapshot,
   and target identity. Create the operation directory and durably write intent
   before making any public resource change. An incomplete directory also blocks
   controlled startup until recovery with the same trusted plan.
2. Inspect all selected actual history partitions and file/registry preimages.
   Reject conflicts before publishing files. Missing partitions must contain no
   orphan rows. Existing partitions must match every visible prefix row and all
   columns; exact or strictly ahead checkpoints are allowed, never truncated.
3. Write each missing JSONL to a private operation-owned temporary file, fsync it,
   publish with Linux `renameat2(RENAME_NOREPLACE)`, then fsync both directories.
   Never overwrite an independent winner. A crash leaves either an owned partial
   temporary or the exact published file. Recovery can rewrite owned temporaries,
   but retains conflicting public files and stops. Re-fsync observed files and
   directories on replay, including a rename interrupted before directory fsync.
4. Durably record SQL intent, then invoke the real publisher. Its exact compiled
   schema/migration guard and one BEGIN IMMEDIATE transaction remain mandatory;
   no target migration is performed. An existing valid SQL intent is retained.
5. Inspect actual rows/checkpoints after return. Missing stdout, nonzero status,
   timeout, or a killed caller is an **uncertain commit outcome**, not proof of
   rollback. A handled timeout kills and reaps the child before inspection. A
   compatible result may be accepted only after verification; an incomplete
   result stays pending; a conflicting result blocks recovery without repair.
   On replay, inspect first and then rerun the idempotent native publisher to
   retain its compiled schema guard. Compatible state proves the desired result,
   not which process committed it. Record uncertainty reconciliation explicitly.
6. Revalidate inputs and files. Re-read the latest registry, compare each selected
   entry to its bound preimage or desired value, merge only those selected keys,
   fsync a temporary, atomically replace the synthetic registry, and fsync its
   directory. Unrelated changes made between attempts survive. Simultaneous
   noncooperating registry writers are outside this quiescent experiment.
7. Reverify source/staging identity, exact destination bytes, all selected history
   prefixes, and selected metadata. Only then durably write the completion marker.
   Returning success or admitting controlled startup always verifies actual state;
   a forged/stale marker alone cannot authorize either.

A publisher child inherits the flock's open file description. If the adapter is
killed while its child still runs, the child keeps the cooperative gate locked.
A dedicated paused-child test kills only the adapter, proves a second adapter and
controlled startup are blocked, then allows normal SQLite completion and replays.
This is a Linux cooperative-process mechanism, not a native Codex ownership lock.

Recovery is monotonic roll-forward or a preserved conflict. It never restores a
whole database, copies a DB snapshot over the target, deletes WAL/SHM/journals,
forces checkpoint truncation, deletes independent files, repairs changed selected
rows, or rewinds a compatible ahead projection. SQLite performs its own normal
hot-journal recovery on disposable targets.

## Coverage and acceptance

For a two-rollout fixture the suite injects 47 SIGKILL points: operation-directory
creation; partial-write/write/fsync/rename/directory-fsync boundaries for intent,
file acknowledgment, SQL intent, SQL verification, registry, and completion; those
five boundaries for each JSONL; publisher spawn, precommit, orphan-parent death,
post-return, post-inspection; and final verification before the marker.

Every case must replay to verified completion, replay again as a logical no-op,
and preserve independent file, registry, and unrelated DB changes. Additional
checks cover a deliberately lost successful publisher acknowledgment, handled
publisher timeout cleanup and retry, retained file tails, source/destination/registry/projection/staging/intent/inode conflicts,
operation-ID changes, gate contention, unknown fields/path escapes/symlinks, and
a forged completion marker. Original projector/source snapshots stay unchanged.

Run locally:

```
python harness/recovery_checks.py --self-test
```

The JSON output explicitly labels this `python-transaction-model`. Native runs
label artifacts `native-publisher`. Neither label claims native writer exclusion,
filesystem/database atomicity, real registry recovery, power-loss resilience,
trusted bootstrap, or production A-to-B recovery. Existing acceptance gates and
unresolved production blockers remain unchanged.
