# Opt-in offline native-history synchronization

`codexm session sync --native-history --native-writers-stopped --session UUID`
publishes one existing active paginated thread into its configured profile on
Linux. `--session` may be omitted when the project contains exactly one thread.
Use the existing `--project` and `--profile` arguments to select those configured
roots. Ordinary sync remains JSONL-only for profiles that have not opted in.

This first version requires all external native Codex clients to be stopped and
to remain stopped until synchronization or recovery completes. The process scan
can detect existing writers; it cannot prevent a later unmanaged launch. Native
fork can rebuild ancestor projections without the logical-owner locks used by
native resume, so those locks do not provide global exclusion. This operation is
for profiles whose access the operator controls through codexm. Arbitrary external
native concurrency is unsupported.

## Supported inputs and packaged identity

The operation accepts the exact pinned native Codex 0.166.0 executable, an existing
`state_5.sqlite` and `thread_history_1.sqlite`, active uncompressed paginated
rollouts, and a complete source-bound physical ancestry. It rejects ambiguous or
divergent histories, archive/delete transitions, unexpected storage/configuration
routes, unknown fields, unsafe paths, replaced roots/databases and schema or
migration drift. It does not initialize or migrate a target database.

The trusted build supplies `internal/session.NativeHistoryBinarySHA256` and
`internal/session.NativeHistoryHelperSHA256` through Go linker values. The native
helper must be the `native_history_helper` executable beside codexm. A normal
unpinned developer build refuses native publication. Runtime configuration,
portable repository data and recovery journals cannot select a helper executable
or assert its identity. This package is experimental and is not added to the
normal release/install workflows.

## Locks and launch boundary

The operation owns the existing profile lease namespace, the profile recovery
gate, and the shared project gate. It does not acquire the old lease a second
time inside the recovery path. Native helpers inherit the gate descriptors, so
an orphan helper keeps cooperating processes excluded until it exits. Linux
foreground native launches also inherit their codexm lease descriptors.

One persistent helper acquires the affected logical owners in sorted order before
preparing artifacts, and holds them across file publication, history publication,
tip selection and verification. Physical rollout UUIDs remain projection keys;
they are not substituted for logical-owner lock IDs. Acquisition avoids the
native coordinator's unrelated stale-file cleanup. A partial acquisition is
released when any owner is busy.

An opted-in profile carries `.codexm-native-history`. Pending recovery blocks
cooperating launches and file-only operations. Persistent app-server startup and
unmanaged shells are disabled for these profiles; foreground `codexm run` uses
the managed lease. File-only imports and conflict resolution cannot silently
replace the selected native history beneath that launch boundary.

## Publication and recovery

1. Inspect the actual native selected path while holding ownership; do not infer
   it from filenames. Freeze file preimages, complete bounded input snapshots,
   selected registry preimages, logical old/new paths, and native artifact
   identity into the private authenticated recovery plan.
2. Durably activate the pending barrier before publishing files. Retain exact
   existing prefixes and compatible longer tails. Never overwrite a divergent
   file or discard an independent winner.
3. Publish and verify history using the exact native projector and guarded
   transactional publisher. Same-physical advancement authenticates both old
   and newer projections and verifies every selected row/checkpoint. Preserve
   unrelated partitions and compatible ahead state.
4. Only after history verification, conditionally update the selected thread's
   `rollout_path` from the bound old path to the new path. An already-desired path
   supports replay. A third path or unsupported thread state is a conflict.
   Preserve all other thread columns.
5. Publish selected codexm registry values last. Reverify actual files, native
   history, selected metadata and registry, then remove the pending barrier.

Files, native history and native state are separate resources. Recovery rolls
forward or preserves a conflict; there is no cross-resource atomic transaction.
A killed helper, timeout or lost acknowledgment has an uncertain commit outcome.
Recovery inspects actual state before retrying and does not trust a completion
marker or stdout alone. It uses private authenticated snapshots and configured
roots without needing the original caller or portable session records. It never
restores a whole database, removes WAL/SHM files, truncates compatible ahead data,
or forces a selected tip over an independent change.

## Regression gates

The existing JSONL-only fixture remains a negative control: it must report
`status: blocked` with only the stale selected-tip check failing. Its CI invocation
continues to use `--expect-stale-tip`.

`current_tip_checks.py --native-history` is a separate strict acceptance mode.
Only the final B-to-A transfer uses native publication; the initial A-to-B import
and B export still use the ordinary CLI. The fixture requires:

- Native metadata selects B's physical tip before A starts; only its selected
  `rollout_path` column changes, and unrelated native metadata/projections survive.
- A cold logical-ID resume chooses that path, with complete paginated item/turn
  parity and model replay excluding B's discarded turn.
- A's continuation appends to the new physical tip while preserving its ancestor.
  A second cold resume retains the tip, history and continuation.
- B's source and reverted tip remain unchanged, and no authentication is copied.

The Linux CI workflow builds a separate `nativehistorytest` CLI. Only that tagged
binary recognizes `CODEXM_NATIVE_HISTORY_TEST_FAULT`. Independent real native
fixtures kill it at `files:verified`, `sql:returned`, and `registry:verified`,
verify that pending recovery rejects native startup, withhold the portable session
and history inputs, and recover with the ordinary pinned binary. The operation
then repeats as an idempotent maintenance call before the native resume checks.
Three additional cases keep the tagged CLI alive and kill only its helper at
`history_committed`, `state_before_commit`, and `state_after_commit`. These cover
the gap between the two native database commits, rollback of an uncommitted tip
update, and a committed tip whose acknowledgment is lost. The harness waits for
the debug helper's exact pause marker and the tagged CLI's owned-helper PID,
then requires CLI failure and verified recovery with the ordinary binary.
The helper pause hooks exist only in Rust debug builds, and only the tagged CLI
forwards their test environment and stderr. Ordinary CLI builds neither forward
these controls nor expose the helper PID. The tag and caller process-kill hook
are absent from ordinary binaries.

Go tests additionally cover authenticated bootstrap, selected conflicts, gate
contention, inherited orphan ownership, tampering, and publication boundaries.
The JSON-file model tests establish orchestration behavior only; the native CI
cases are required to establish real SQLite and native resume behavior.

Process-kill acceptance does not establish power-loss, disk-corruption, ENOSPC,
all filesystem behavior, macOS/Windows support, or unmanaged native concurrency.
No merge or deployment follows from a passing experimental workflow.

Native maintenance rejects remote and unknown filesystem types before writing; the initial local allowlist is ext-family, XFS, Btrfs, tmpfs and overlay. This does not establish power-loss durability across those filesystems.
