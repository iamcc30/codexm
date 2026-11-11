# Independent current-tip diagnostic

`current_tip_checks.py` isolates one unresolved question: after native B appends
to a logical thread and reverts it onto a new physical rollout, does JSONL-only
codexm export/import make an already-used native A resume the intended new tip
**by logical ID alone**?

The diagnostic also records complete paginated UI payloads and model replay.
It does not copy, inject, migrate, or repair native state/history databases. No
projector or publisher helper is used. All database creation and mutation is by
the real pinned native binary, except ordinary JSONL transfer by codexm.

## Integration

Run as an independent CI step, preferably after the other synthetic experiments:

```sh
python3 experimental/history-projection/current_tip_checks.py \
  --codex "$CODEXM_NATIVE_TEST_BINARY" \
  --codexm "$RUNNER_TEMP/codexm-native-test" \
  --native-harness scripts/native-history-acceptance.py \
  --artifacts "$RUNNER_TEMP/current-tip-logs"
```

The current native binary must report exactly `codex-cli 0.166.0`. The script
imports only the existing harness's `MockResponses` and `AppServer` facilities.
It does not change `probe.py`, the original fixture, or native helper modules.

Callable API:

```python
report = run_current_tip_checks(codex, codexm, native_module,
                                original_app_server_class, artifacts, checks)
assert report["status"] == "passed", report
```

Pass the saved original `AppServer` class, not a subclass that injects history.
`sync_extra_args=()` optionally supplies a later explicit maintenance flag to
`session sync`; CLI form is `--sync-extra-arg=--flag`. It does not change init or
any native request. No flag is required by the current JSONL-only test build.

## Fixture and observations

The script owns a new `codexm-native-acceptance-current-tip-*` temporary root,
isolated HOME/CODEX_HOME, empty project, allowlisted process environment, and a
credential-free localhost mock provider. No pre-existing profile is accessed.
The inherited native test client denies unexpected server tool/approval calls.
Native clients close before each transfer and are cleaned up by context managers.

1. Native A starts one paginated logical thread and completes two synthetic
   turns. Its real state DB records the original physical rollout, and its real
   history DB contains initial UI rows. Snapshot the original source bytes.
2. codexm initializes/imports the project and synchronizes A to B. Verify B has
   no native database or auth file before the first native B launch.
3. Native B resumes by logical ID, appends a retained turn and a discarded turn,
   then reverts before the latter. The returned logical ID must remain the same
   and the physical rollout must change. Read all item and turn pages, forcing
   page size two. Record baseline marker visibility; incomplete B inherited UI
   is a measured baseline blocker, never a reason to skip the A tip test.
4. With both native processes closed, codexm exports B then imports into A.
   Require A's pre-existing native DB file inventories to remain unchanged, so
   this remains a JSONL-only diagnostic. Verify the new tip bytes are exact.
   Verify A's imported ancestor prefix equals B through the new tip's inherited
   byte cutoff, and A's pre-existing prefix is preserved. Do **not** require
   unreferenced discarded B tail bytes to be newly copied to A. If A already
   has a longer compatible tail, require that existing tail to remain intact.
5. Cold native A resumes with `threadId` and `excludeTurns` only, without a path
   override. Record the exact returned rollout path and compare it to the new
   native B tip's corresponding path inside A. Query full item/turn pagination
   before any A continuation and compare every payload with B's output.
6. Submit one synthetic A probe turn. Record the mock provider's request input,
   which exposes the retained context used for that continuation. It must include
   A's two initial turns, B's retained turn, and the probe, while excluding B's
   discarded turn. Then check A UI markers after continuation. This is not a
   standalone model-context read: the context is observed in the continuation
   request, before the mock model generates its response.
7. Verify B's complete source file, including discarded tail, and reverted tip
   stayed unchanged across A's continuation. Native metadata is queried read-only
   to show the logical ID/path before import and after cold resume.

Even if A chooses a wrong path, the script continues through UI and mock-model
checks to distinguish stale metadata from projection-only problems. All changed
history belongs to this disposable synthetic fixture. It never forces the
correct path to make a failing diagnostic pass.

## Result meanings

`current-tip-summary.json` distinguishes:

- `status: passed`, `completed_diagnostic: true`: this fixture selected the
  intended tip and passed every recorded UI/model/preservation check.
- `status: blocked`, `completed_diagnostic: true`: a measured native/transfer
  acceptance check failed. `failures` and per-check observations identify it.
  This is not an infrastructure failure or production acceptance pass.
- `status: failed`, `completed_diagnostic: false`: setup, native RPC, process
  lifecycle, schema expectation, or other harness prerequisite failed. Inspect
  `failed_stage`, `error`, RPC/stderr logs and `current-tip-traceback.log`.

`native_tip_resolution: stale_or_wrong_tip` plus exact intended/actual paths is
evidence that JSONL-only transfer did not synchronize native logical-ID tip
selection. `state_adapter_needed_evidence: true` records that observation; it
does not authorize direct database edits or choose a particular adapter design.

`native_tip_resolution: intended_tip` with item/turn mismatches points to a
history projection issue rather than proving a state-path adapter is needed.
Baseline missing B UI is recorded separately and does not conceal the tip result.
By default, all non-passed statuses return exit 1 from the standalone CLI. A caller
must not ignore the returned status when using the callable API.

The experimental workflow adds `--expect-stale-tip` to verify the measured
JSONL-only baseline. It returns exit 0 only for a completed `blocked` diagnostic
whose sole failure is the intended physical-tip selection check, with stale-tip
and state-adapter evidence and no harness error. Any additional failure, incomplete
run, or unexpected native acceptance pass returns exit 1 and requires review.
The JSON report remains `blocked`; this mode does not repair native metadata or
waive the independent production acceptance gate.

Runs [41955232050](https://github.com/iamcc30/codexm/actions/runs/41955232050),
[41956624999](https://github.com/iamcc30/codexm/actions/runs/41956624999), and
[41957427540](https://github.com/iamcc30/codexm/actions/runs/41957427540) all measured
this sole current-tip blocker. UI payload parity and model replay passed because
A received the retained ancestor prefix, even while selecting its old path.
The pinned native [resolver](https://github.com/openai/codex/blob/6ff1e1ca3b30fc8b914720681fcf5fe62e95f856/codex-rs/thread-store/src/local/thread_rollout_resolver.rs#L87-L117)
treats SQLite's selected rollout path as authoritative for paginated threads.
JSONL-only transfer deliberately preserves that database and cannot publish B's
new selected path. History projection alone does not solve this metadata gap.

## Retained artifacts

The dedicated artifact directory retains:

- Structured summary with intended/actual physical IDs and paths, source cutoff
  and preservation evidence, full-payload parity, markers and page counts.
- Full expected B and actual A item/turn JSON before continuation, and A items
  afterward. Pagination rejects repeated cursors and duplicate item payloads.
- A model input JSON, native metadata/path snapshots, and A DB preservation
  inventory before/after JSONL-only transfer.
- Every native RPC/stderr log and codexm command log, all synthetic.
- A traceback with the failed stage for unexpected harness failure.

The temporary profiles are removed after their processes stop, including on
failure. Evidence is written outside those temporary profiles. No real sessions,
credentials, or other user data are copied into artifacts.

## Validation performed locally

`python3 -m unittest discover -s experimental/history-projection/tests -v`
covers strict acceptance, the explicit expected blocker, and rejection of
additional failures, incomplete diagnostics, wrong resolutions and harness errors.

The checksum-verified official macOS arm64 0.166.0 package reproduced the same
sole tip-selection blocker with isolated synthetic profiles. The original and
updated strict commands exited 1; `--expect-stale-tip` exited 0 while retaining
`status: blocked`. Full item/turn payload parity and all other recorded checks
passed. The full Linux Rust workflow was not rerun locally.
