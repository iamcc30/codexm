# Unintegrated Go recovery prototype

Status: experimental new files only. `Sync`, `Resolve`, the CLI, daemon startup,
native launches, and existing leases do not call this implementation. No real
profile or external integration has been used. This is not merge eligibility or a
production recovery claim.

## API

- `NewFrozenSyncPlan`: read-only planning with complete file preimages, exact
  bounded physical-rollout closure, and selected registry compare-and-set values.
- `CommitFrozenSync`: freeze private authenticated intent, publish files, invoke
  the projection backend, verify actual state, publish registry last, then clear
  the startup barrier.
- `RecoverFrozenSync`: bootstrap from configured manager/profile roots and the
  private durable journal. The original plan, original process, request file,
  and repository source records are not supplied again. Existing destination
  roots must still have their captured filesystem identities.
- `OpenRecoveryGate` and `RecoveryGate.Run`: cooperative profile/project flocks
  and child descriptor inheritance. A surviving child keeps both locks after its
  parent dies. This does not acquire native Codex ownership or stop native clients.
- `ProjectionBackend`: `Identity`, `Prepare`, `Publish`, `Verify`. No native
  implementation is included. Prepare must perform read-only schema/preimage
  checks; Publish must use transactional selected-partition compare-and-set;
  Verify must inspect actual desired rows/schema, not trust an acknowledgment.

Authority lives under the configured manager's private `session-recovery-v2`
tree, with a random local HMAC key. A signed profile-local pending pointer binds
operation identity and manager/profile scope. The plan hash includes a random
operation instance, preventing an interrupted, unactivated preparation from
permanently blocking a retry. Repository input cannot choose a helper executable,
arbitrary absolute target, or recovery authority. This is not protection against
an attacker controlling the same OS account and local key.

## Current supported core operations and restrictions

- Linux only. All three Go files have Linux build constraints; there are no CLI
  imports and no cross-platform recovery claim. Before integration elsewhere,
  add explicit unsupported stubs or separately tested platform implementations.
- Configured manager/profile/project roots must be canonical, disjoint,
  user-owned directories that are not writable by other users. Symlink components,
  hardlinked files, unknown journal fields, changed root identities, and missing
  authentication keys fail closed.
- The file allowlist is inherited from `transactionStore.target`: profile
  sessions, archived sessions, and session_index; project sessions, archived
  sessions, metadata, and tombstones. It does not cover project-format upgrades
  or portable `history/` snapshot creation. Those must not happen as hidden
  mutations outside a future aggregate plan.
- Existing same-physical JSONL may only extend an exact prefix or retain its
  existing compatible longer tail. Divergent replacement, truncation, compressed
  rollouts, incomplete ancestry, and an unprojected new/appended tail are rejected.
  Non-rollout replacements/deletes use exact full-file preimages. Native deletion,
  archive/revert visibility, and state.sqlite metadata behavior remain unproved.
- Independent unrelated registry entries are merged. Independent modifications
  to selected files, selected registry entries, or selected projection preimages
  remain conflicts. A session_index replacement currently uses whole-file CAS,
  so unrelated index changes cause a preserved conflict rather than a name merge.
- Backend artifacts are private, immutable, hash-bound files. A real native
  adapter must bind and verify every additional old/new projection artifact it
  needs; the current model does not establish such a native artifact contract.
- The plan's `0.166.0` label is a protocol restriction, not executable detection.
  Reliable native version/configuration resolution and the all-native-processes-
  closed precondition remain external, unresolved integration requirements.
- The project flock shares the existing Lease namespace. Do not invoke this API
  under an already-held old Lease: integration needs a common inherited gate,
  not a second acquisition of the same lock.

## Tests and validation limits

`recovery_v2_test.go` uses a clearly named JSON-file model backend. Its artifact
is named `projection.sqlite` only to exercise the opaque artifact contract; it is
**not SQLite**, does not use SQLx, and does not update native state metadata.

Authored subprocess tests cover journal/file/model-commit/registry/completion
SIGKILL transitions; replay after deleting the original caller request; inert
preparation retry; lost acknowledgment; verification failure; selected conflicts;
HMAC/unknown-field/path/symlink/hardlink tampering; retained tails; deletion;
shared-project exclusion; orphan inheritance of both gates; and cancellation
cleanup. No whole-database restore or WAL removal is used.

Local Go compilation, gofmt, tests, vet, and race checks have **not run** because
the local `go` command is not the Go compiler. The parent will validate these
files in isolated CI. Process-kill tests do not establish power-loss resilience,
fsync-error/ENOSPC behavior, filesystem portability, or native concurrency safety.
