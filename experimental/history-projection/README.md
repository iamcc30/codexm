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

The original fresh-profile experiment passed all 13 native flow gates, both full
paginated payload comparisons, and 12 rejection tests in [CI run 41946567278](https://github.com/iamcc30/codexm/actions/runs/41946567278).
It uses an unchanged native acceptance harness plus explicit diagnostic injection.

A separate `publish_projection` example now tests existing synthetic database
publication. Its contract and remaining production limits are in
[the publisher design](helper/PUBLISHER_DESIGN.md). It is not called by codexm sync.

The existing-database publisher passed 24 isolated checks alongside all 13 native
flow gates in [CI run 41949899004](https://github.com/iamcc30/codexm/actions/runs/41949899004).
This remains separate from actual codexm synchronization.

Additional experiments measure [native owner-lock boundaries](helper/NATIVE_LOCK_DESIGN.md)
and [quiescent create-only recovery](helper/RECOVERY_DESIGN.md). A passing diagnostic
can confirm an unsafe production boundary, such as a fork bypassing an owner lock.
No diagnostic result grants arbitrary native concurrency, existing-file update
recovery, older-version compatibility, or permission to merge the production PR.
