# Integrated private recovery core

`internal/session/frozen_plan.go` and `recovery_v2.go` now provide the authenticated
private intent, frozen file preimages, inherited profile/project gates, startup
barrier and verified roll-forward used by opt-in Linux native-history sync.
The original sources were moved from `.go.txt` snapshots; there is one active copy.

The JSON-file model tests remain intentionally distinct from native SQLite tests.
`native_history_linux.go` supplies the pinned persistent native helper adapter;
its selected-tip CAS happens after history verification and before registry
publication. Helper failure, timeout or lost acknowledgment leaves recovery
pending until actual files, projections and metadata have been verified.

This is offline maintenance, not arbitrary native-writer exclusion. Every external
native client must remain stopped. Existing native logical owner locks do not
exclude an unmanaged fork's ancestor projection. Controlled launch routes share
the maintenance lease, and a surviving helper inherits those descriptors.

The initial path selects one existing active uncompressed paginated logical thread.
It rejects archive/delete transitions, divergent file prefixes, unsupported native
configuration/schema/binaries, and unbound or changed recovery artifacts. Recovery
uses configured local authority and private snapshots, not a resupplied repository
plan. It preserves unrelated records and compatible file tails and never restores
whole databases or deletes WAL files. SIGKILL coverage does not establish power-loss
or network-filesystem guarantees.

Exact verification results are recorded by the workflow's native maintenance and
recovery steps. See [the design](../../NATIVE_SYNC_DESIGN.md). No merge or deployment
eligibility follows merely from the Go model tests passing.
