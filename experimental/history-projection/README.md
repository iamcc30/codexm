# Experimental projection-only feasibility probe

This is not a production repair and is not wired into codexm session synchronization.
It evaluates whether the exact upstream Codex projector can reconstruct complete
paginated UI history in a disposable synthetic profile while preserving rollout bytes.

The source pin is Codex rust-v0.166.0, commit
6ff1e1ca3b30fc8b914720681fcf5fe62e95f856. The helper must never initialize a Codex
session, StateRuntime, app-server, model, MCP, hooks, plugins, or user configuration.
It opens only a new, separate staging history database. All input files are synthetic
copies created by the test harness. There is no operation that publishes rows into a
real or existing profile database.

The native-history acceptance script remains an independent production gate.
Even a passing probe does not waive its failure or authorize merging PR #2.

Production work still requires validated per-rollout transactional publication,
compatibility and schema guards, native writer exclusion, interrupted-publication
recovery, complete lineage and payload parity, and supported platform packaging.
