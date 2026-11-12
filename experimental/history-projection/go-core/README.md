# Integrated Go maintenance core

The former `.go.txt` proof snapshots now live in `internal/historycompat` and
`internal/session/{frozen_plan,recovery_v2}.go`, with their tests beside them.
CI tests those active sources directly; it must not copy historical snapshots
over the implementation.

The recovery model tests verify orchestration, authentication, inherited gates
and restart behavior. The production native backend is `native_history_linux.go`;
its actual native SQLite and selected-tip acceptance is a separate strict CI gate.
See [the native synchronization design](../NATIVE_SYNC_DESIGN.md) for its narrow
Linux, pinned-binary, stopped-writer operating boundary.
