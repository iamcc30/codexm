#!/usr/bin/env python3
"""Independent synthetic diagnostic of logical-ID resume after a remote revert.

No native database is copied, injected, repaired, or written by this harness.
By default native state and history DBs are created only by the pinned binary.
The explicit native-history mode tests codexm publication into those existing DBs.
The existing codexm CLI performs every A/B history transfer.
"""
import argparse
from contextlib import closing
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import signal
import time
import sqlite3
import subprocess
import tempfile
import traceback


VERSION = "0.166.0"
UUID = r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"
ROLLOUT = re.compile(rf"rollout-.+-({UUID})(?:_({UUID}))?\.jsonl$")
TIP_SELECTION_CHECK = "cold logical-ID resume selects B's intended physical tip"


def digest(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def native_db_inventory(home):
    """Observe native DB files without opening them or modifying sidecars."""
    result = {}
    for path in sorted(home.glob("*.sqlite*")):
        assert path.is_file() and not path.is_symlink(), path
        stat = path.stat()
        result[path.name] = {"sha256": digest(path), "bytes": stat.st_size,
                             "mtime_ns": stat.st_mtime_ns, "inode": stat.st_ino}
    return result


def native_metadata(home, logical_id):
    """Read only the synthetic native thread's path, preserving WAL semantics."""
    databases = sorted(home.glob("state_*.sqlite"))
    assert len(databases) == 1, f"Expected one native state DB, found {databases}"
    with closing(sqlite3.connect(databases[0].as_uri() + "?mode=ro", uri=True)) as connection:
        rows = connection.execute("SELECT id,rollout_path FROM threads WHERE id=?", (logical_id,)).fetchall()
    assert len(rows) == 1, "Native metadata for synthetic thread is missing or ambiguous"
    return {"database": databases[0].name, "logical_id": rows[0][0], "rollout_path": rows[0][1]}


def native_thread_snapshot(home, logical_id):
    """Read only one synthetic native thread's metadata and projection partition."""
    state = next(home.glob("state_*.sqlite"))
    with closing(sqlite3.connect(state.as_uri() + "?mode=ro", uri=True)) as connection:
        connection.row_factory = sqlite3.Row
        row = connection.execute("SELECT * FROM threads WHERE id=?", (logical_id,)).fetchone()
        assert row is not None, "Synthetic native metadata missing"
        metadata = dict(row)
    partitions = {}
    history = home / "thread_history_1.sqlite"
    with closing(sqlite3.connect(history.as_uri() + "?mode=ro", uri=True)) as connection:
        for table in ("thread_turns", "thread_items", "thread_realtime_items", "thread_history_projection_state"):
            partitions[table] = sorted(connection.execute(f"SELECT * FROM {table} WHERE thread_id=?", (logical_id,)).fetchall(), key=repr)
    return {"metadata": metadata, "history": partitions}


def paginate(app, method, logical_id, *, page_size=2):
    items, cursor, cursors, seen = [], None, set(), set()
    pages = 0
    while True:
        params = {"threadId": logical_id, "limit": page_size, "sortDirection": "asc"}
        if cursor is not None:
            params["cursor"] = cursor
        page = app.request(method, params)
        pages += 1
        for item in page["data"]:
            identity = json.dumps(item, sort_keys=True, separators=(",", ":"))
            assert identity not in seen, f"Duplicate payload from {method} pagination"
            seen.add(identity)
            items.append(item)
        cursor = page.get("nextCursor")
        if not cursor:
            return items, pages
        assert cursor not in cursors and len(cursors) < 10000, f"Nonadvancing {method} cursor"
        cursors.add(cursor)


def marker_checks(value, markers):
    encoded = json.dumps(value)
    return {name: marker in encoded for name, marker in markers.items()}


def physical_id(path):
    match = ROLLOUT.fullmatch(path.name)
    assert match, f"Unexpected native rollout filename: {path.name}"
    return match.group(2) or match.group(1)


def kill_helper_at_pause(command, env, cwd, artifacts, point):
    """Kill only the helper identified by this test-owned tagged CLI at its pause."""
    stdout_path = artifacts / "current-tip-helper-fault-stdout.log"
    stderr_path = artifacts / "current-tip-helper-fault-stderr.log"
    helper_pid = None
    process = None
    with stdout_path.open("w") as stdout, stderr_path.open("w") as stderr:
        try:
            process = subprocess.Popen(command, cwd=cwd, env=env, text=True,
                                       stdout=stdout, stderr=stderr)
            deadline = time.monotonic() + 90
            while process.poll() is None and time.monotonic() < deadline:
                observed = stderr_path.read_text()
                identity = re.search(r"CODEXM_NATIVE_HISTORY_TEST_HELPER_PID:(\d+)", observed)
                if identity:
                    helper_pid = int(identity.group(1))
                if helper_pid and f"CODEXM_NATIVE_HISTORY_TEST_PAUSED:{point}" in observed:
                    os.kill(helper_pid, signal.SIGKILL)
                    process.wait(timeout=35)
                    assert process.returncode != 0, "Tagged CLI accepted a killed native publisher"
                    return subprocess.CompletedProcess(command, process.returncode,
                                                       stdout_path.read_text(), stderr_path.read_text())
                time.sleep(0.02)
            raise AssertionError(f"Native helper pause {point} was not reached: {stderr_path.read_text()}")
        finally:
            if process is not None and process.poll() is None:
                if helper_pid:
                    try:
                        os.kill(helper_pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                process.kill()
                process.wait(timeout=10)


def run_current_tip_checks(codex, codexm, native_module, app_server_class,
                           artifacts, checks, *, sync_extra_args=(), native_history=False,
                           fault_codexm=None, fault_point=None, helper_fault_point=None):
    """Return a full report; caller must fail acceptance unless status is passed.

    Use the saved original native AppServer class, not the projection wrapper.
    This function owns an independent temporary fixture and localhost provider.
    native_history opts only the final B-to-A transfer into real CLI publication.
    Fault injection is available only through a separately built test-tag binary.
    """
    codex, codexm = Path(codex).resolve(strict=True), Path(codexm).resolve(strict=True)
    injecting = bool(fault_point or helper_fault_point)
    if (bool(fault_codexm) != injecting or (injecting and not native_history)
            or (fault_point and helper_fault_point)):
        raise ValueError("Fault acceptance requires native-history mode, a fault binary and exactly one point")
    if fault_codexm:
        fault_codexm = Path(fault_codexm).resolve(strict=True)
    artifacts = Path(artifacts).resolve()
    artifacts.mkdir(parents=True, exist_ok=True)
    report = {"status": "failed", "diagnostic_only": True,
              "scenario": "B appends and reverts; cold A resumes by logical ID",
              "required_version": VERSION, "native_state_injection": False,
              "native_history_injection": False, "native_history_sync": native_history,
              "fault_point": fault_point, "helper_fault_point": helper_fault_point,
              "native_tip_resolution": "not assessed",
              "state_adapter_needed_evidence": False, "completed_diagnostic": False,
              "checks": [], "failures": []}

    def record(name, passed, **details):
        item = {"check": name, "passed": bool(passed), **details}
        report["checks"].append(item)
        if passed:
            checks.append("current-tip diagnostic: " + name)
        else:
            report["failures"].append(name)
        return passed

    def save(name, value):
        (artifacts / f"current-tip-{name}.json").write_text(json.dumps(value, indent=2) + "\n")

    stage = "initialization"
    try:
        with tempfile.TemporaryDirectory(prefix="codexm-native-acceptance-current-tip-") as temporary:
            root = Path(temporary).resolve()
            for name in ("home", "manager", "profiles", "tmp", "runtime", "unused-codex", "project"):
                (root / name).mkdir(mode=0o700)
            project = root / "project"
            env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "LANG": "C.UTF-8",
                   "HOME": str(root / "home"), "CODEX_HOME": str(root / "unused-codex"),
                   "CODEXM_HOME": str(root / "manager"), "CODEXM_PROFILES_HOME": str(root / "profiles"),
                   "TMPDIR": str(root / "tmp"), "XDG_RUNTIME_DIR": str(root / "runtime"),
                   "CODEXM_CODEX_BIN": str(codex)}
            version = subprocess.run([str(codex), "--version"], cwd=project, env=env, text=True,
                                     capture_output=True, check=True, timeout=15).stdout.strip()
            assert version == "codex-cli " + VERSION, f"Unsupported native test binary: {version}"
            report["codex_version"] = version
            with (artifacts / "current-tip-codexm.log").open("w") as command_log:
                def manager(*arguments, binary=codexm, extra_env=None, require_success=True):
                    completed = subprocess.run([str(binary), *arguments], cwd=project,
                                               env=dict(env, **(extra_env or {})),
                                               text=True, capture_output=True, timeout=90)
                    command_log.write(json.dumps(arguments) + "\n" + completed.stdout + completed.stderr + "\n")
                    command_log.flush()
                    if require_success:
                        assert completed.returncode == 0, \
                            f"codexm {arguments}: {completed.stdout}{completed.stderr}"
                    return completed

                def sync(profile, *, repair=False):
                    arguments = ("session", "sync", "--project", str(project), "--profile", profile,
                                 *sync_extra_args)
                    if not repair:
                        return manager(*arguments)
                    arguments += ("--native-history", "--native-writers-stopped", "--session", logical)
                    if fault_point or helper_fault_point:
                        if helper_fault_point:
                            killed = kill_helper_at_pause([str(fault_codexm), *arguments],
                                dict(env, CODEXM_NATIVE_HISTORY_TEST_PAUSE_AT=helper_fault_point),
                                project, artifacts, helper_fault_point)
                            command_log.write(json.dumps(arguments) + "\n" + killed.stdout + killed.stderr + "\n")
                            command_log.flush()
                            observed_tip = Path(native_metadata(first, logical)["rollout_path"]).resolve()
                            expected_tip = (first / b_tip_path.relative_to(second)
                                            if helper_fault_point == "state_after_commit" else original_a_path)
                            record("helper death exposes the correct committed or rolled-back native tip",
                                   observed_tip == expected_tip.resolve(), point=helper_fault_point,
                                   expected=str(expected_tip), actual=str(observed_tip))
                            history = first / "thread_history_1.sqlite"
                            with closing(sqlite3.connect(history.as_uri() + "?mode=ro", uri=True)) as db:
                                checkpoint = db.execute("SELECT next_rollout_byte_offset,next_rollout_ordinal FROM thread_history_projection_state WHERE thread_id=?",
                                                        (physical_id(b_tip_path),)).fetchone()
                            expected_checkpoint = (len(b_tip_bytes), json.loads(b_tip_bytes.splitlines()[-1])["ordinal"] + 1)
                            record("history commit survives native helper death before acknowledgment",
                                   checkpoint == expected_checkpoint, point=helper_fault_point,
                                   expected=list(expected_checkpoint), actual=list(checkpoint) if checkpoint else None)
                        else:
                            killed = manager(*arguments, binary=fault_codexm,
                                             extra_env={"CODEXM_NATIVE_HISTORY_TEST_FAULT": fault_point},
                                             require_success=False)
                            assert killed.returncode == -signal.SIGKILL, \
                                f"Fault point was not reached: {killed.stdout}{killed.stderr}"
                        pending = first / ".codexm-sync-v2" / "pending.json"
                        assert pending.is_file(), "Killed native publication has no durable pending barrier"
                        deadline = time.monotonic() + 35
                        while True:
                            blocked = manager("run", "--project", str(project), "first", "--", "--version",
                                              require_success=False)
                            assert blocked.returncode != 0, \
                                f"Pending publication admitted native startup: {blocked.stdout}{blocked.stderr}"
                            if "pending" in blocked.stderr.lower():
                                break
                            busy = "using this project or profile" in blocked.stderr or "gate busy" in blocked.stderr
                            assert busy and time.monotonic() < deadline, blocked.stdout + blocked.stderr
                            time.sleep(0.05)
                        # Recovery must use its private authenticated snapshots, not portable inputs.
                        withheld = root / "withheld-project-inputs"
                        withheld.mkdir()
                        moved = []
                        try:
                            for name in ("sessions", "history"):
                                source = project / ".codexm" / name
                                if source.exists():
                                    target = withheld / name
                                    source.rename(target)
                                    moved.append((source, target))
                            deadline = time.monotonic() + 35
                            while True:
                                recovered = manager(*arguments, require_success=False)
                                if recovered.returncode == 0:
                                    break
                                busy = "using this project or profile" in recovered.stderr or "gate busy" in recovered.stderr
                                if not busy or time.monotonic() >= deadline:
                                    raise AssertionError(f"Native recovery failed: {recovered.stdout}{recovered.stderr}")
                                time.sleep(0.05)
                            assert not pending.exists(), "Verified recovery retained its startup barrier"
                            assert "recovered native-history operation" in recovered.stdout, recovered.stdout
                            record("native publication recovers after process death without portable source records", True,
                                   point=fault_point or helper_fault_point,
                                   killed_process="helper" if helper_fault_point else "caller")
                        finally:
                            for source, target in moved:
                                target.rename(source)
                    return manager(*arguments)

                manager("add", "first")
                manager("add", "second")
                configured = json.loads((root / "manager" / "config.json").read_text())
                homes = {name: Path(configured["profiles"][name]["codex_home"]).resolve()
                         for name in ("first", "second")}
                assert all(home.is_relative_to(root) for home in homes.values())
                server = native_module.MockResponses()
                try:
                    config = f'''model = "mock-model"
model_provider = "mock_provider"
approval_policy = "never"
sandbox_mode = "read-only"
[model_providers.mock_provider]
name = "Local synthetic current-tip diagnostic only"
base_url = "http://127.0.0.1:{server.server_port}/v1"
wire_api = "responses"
requires_openai_auth = false
request_max_retries = 0
stream_max_retries = 0
'''
                    for home in homes.values():
                        home.mkdir(mode=0o700, parents=True, exist_ok=True)
                        (home / "config.toml").write_text(config)
                    first, second = homes["first"], homes["second"]
                    first_env, second_env = dict(env, CODEX_HOME=str(first)), dict(env, CODEX_HOME=str(second))
                    markers = {name: f"codexm-current-tip-{name}-4df612" for name in
                               ("a-one", "a-two", "b-keep", "b-discard", "a-probe")}
                    stage = "native A creates existing metadata and history"
                    with app_server_class(codex, first_env, project, artifacts, "current-tip-a-create") as app:
                        thread = app.request("thread/start", {
                            "cwd": str(project), "historyMode": "paginated", "model": "mock-model",
                            "modelProvider": "mock_provider", "approvalPolicy": "never", "sandbox": "read-only",
                            "baseInstructions": "Synthetic current-tip acceptance. Reply briefly. Do not use tools.",
                        })["thread"]
                        logical = thread["id"]
                        app.turn(logical, markers["a-one"])
                        app.turn(logical, markers["a-two"])
                        if thread.get("path") is None:
                            thread = app.request("thread/read", {"threadId": logical, "includeTurns": False})["thread"]
                        original_a_path = Path(thread["path"])
                        assert original_a_path.is_relative_to(first)
                        a_initial_items, _ = paginate(app, "thread/items/list", logical)
                        initial_markers = marker_checks(a_initial_items, markers)
                        assert initial_markers["a-one"] and initial_markers["a-two"]
                        if native_history:
                            unrelated = app.request("thread/start", {
                                "cwd": str(project), "historyMode": "paginated", "model": "mock-model",
                                "modelProvider": "mock_provider", "approvalPolicy": "never", "sandbox": "read-only",
                                "baseInstructions": "Synthetic unrelated thread preservation fixture. Do not use tools.",
                            })["thread"]["id"]
                            app.turn(unrelated, "codexm-unrelated-native-tip-preservation-4df612")
                    report["logical_id"] = logical
                    report["a_initial_path"] = str(original_a_path)
                    a_original_source_bytes = original_a_path.read_bytes()
                    a_original_metadata = native_metadata(first, logical)
                    save("a-metadata-original", a_original_metadata)
                    assert Path(a_original_metadata["rollout_path"]).resolve() == original_a_path.resolve()
                    assert (first / "thread_history_1.sqlite").is_file()
                    record("A starts with native metadata and a populated native history database", True)

                    stage = "JSONL-only A to B transfer"
                    manager("session", "init", "--project", str(project), "--profile", "first", "--import-existing")
                    sync("second")
                    assert not list(second.glob("*.sqlite*")), "A to B sync copied or injected a native DB"
                    assert not (second / "auth.json").exists()
                    record("A to B transfer does not copy a native database or auth", True)

                    stage = "native B appends and reverts to a new physical tip"
                    with app_server_class(codex, second_env, project, artifacts, "current-tip-b-revert") as app:
                        resumed_b = app.request("thread/resume", {"threadId": logical, "excludeTurns": True})["thread"]
                        b_source_path = Path(resumed_b["path"])
                        assert b_source_path.is_relative_to(second)
                        app.turn(logical, markers["b-keep"])
                        discard_turn = app.turn(logical, markers["b-discard"])
                        reverted = app.request("thread/revert", {"threadId": logical,
                                                                  "beforeTurnId": discard_turn})["thread"]
                        b_tip_path = Path(reverted["path"])
                        assert reverted["id"] == logical and b_tip_path != b_source_path
                        assert b_tip_path.is_relative_to(second)
                        assert physical_id(b_tip_path) != physical_id(b_source_path)
                        b_items, b_item_pages = paginate(app, "thread/items/list", logical)
                        b_turns, b_turn_pages = paginate(app, "thread/turns/list", logical)
                        b_marker_results = marker_checks(b_items, markers)
                        b_ui_ok = all(b_marker_results[name] for name in ("a-one", "a-two", "b-keep")) \
                            and not b_marker_results["b-discard"]
                        # Missing inherited UI is an existing known baseline risk. Record it,
                        # but never let it prevent measuring A's logical-ID tip selection.
                        report["b_ui_markers"] = b_marker_results
                        report["b_baseline_ui_complete"] = b_ui_ok
                    b_source_bytes, b_tip_bytes = b_source_path.read_bytes(), b_tip_path.read_bytes()
                    assert markers["b-discard"].encode() in b_source_bytes
                    b_tip_meta = json.loads(b_tip_bytes.splitlines()[0])
                    assert b_tip_meta["type"] == "session_meta"
                    inherited = b_tip_meta["payload"]["history_base"]
                    assert inherited["thread_id"] == physical_id(b_source_path)
                    inherited_bytes = inherited["end_byte_offset"]
                    assert 0 < inherited_bytes <= len(b_source_bytes)
                    assert b_source_bytes[:inherited_bytes].endswith(b"\n")
                    assert b_source_bytes.startswith(a_original_source_bytes)
                    report.update({"b_previous_physical_id": physical_id(b_source_path),
                                   "b_intended_physical_id": physical_id(b_tip_path),
                                   "b_intended_tip": str(b_tip_path), "b_item_pages": b_item_pages,
                                   "b_turn_pages": b_turn_pages})
                    save("expected-b-items", b_items)
                    save("expected-b-turns", b_turns)
                    save("b-metadata-after-revert", native_metadata(second, logical))
                    record("B revert creates a distinct physical tip", True)
                    record("B baseline UI contains retained history and excludes its discarded turn", b_ui_ok,
                           category="baseline_observation", markers=b_marker_results)
                    record("B fixture exercises multiple native item and turn pages", b_item_pages > 1 and b_turn_pages > 1,
                           category="coverage_observation", item_pages=b_item_pages, turn_pages=b_turn_pages)

                    stage = "native-history B to A publication" if native_history else "JSONL-only B to A transfer into A's existing native DB"
                    if native_history:
                        native_selected_before = native_thread_snapshot(first, logical)["metadata"]
                        native_unrelated_before = native_thread_snapshot(first, unrelated)
                    a_db_before = native_db_inventory(first)
                    assert a_db_before, "A lost its pre-existing native database"
                    sync("second")
                    sync("first", repair=native_history)
                    a_db_after = native_db_inventory(first)
                    if native_history:
                        selected = native_metadata(first, logical)
                        expected = first / b_tip_path.relative_to(second)
                        record("native publication selects the transferred tip before native startup",
                               Path(selected["rollout_path"]).resolve() == expected.resolve())
                        assert (first / ".codexm-native-history").is_file()
                        record("native publication installs the managed-profile launch boundary", True)
                        selected_after = native_thread_snapshot(first, logical)["metadata"]
                        changed_columns = sorted(name for name in native_selected_before
                                                 if native_selected_before[name] != selected_after[name])
                        record("native publication changes only the selected rollout_path metadata column",
                               changed_columns == ["rollout_path"], changed_columns=changed_columns)
                        record("native publication preserves unrelated native metadata and projection rows",
                               native_thread_snapshot(first, unrelated) == native_unrelated_before)
                    else:
                        record("B to A transfer preserves A's existing native database files", a_db_after == a_db_before)
                        assert a_db_after == a_db_before, "Transfer touched native DBs; this is no longer the JSONL-only diagnostic"
                    intended_a_path = first / b_tip_path.relative_to(second)
                    old_a_source = first / b_source_path.relative_to(second)
                    assert intended_a_path.read_bytes() == b_tip_bytes, "Imported new-tip bytes differ from native B"
                    imported_source = old_a_source.read_bytes()
                    # Sync imports only the inherited dependency cutoff when A's source
                    # is shorter. It must not be required to copy B's discarded tail.
                    source_prefix_ok = imported_source[:inherited_bytes] == b_source_bytes[:inherited_bytes]
                    original_prefix_ok = imported_source.startswith(a_original_source_bytes)
                    if len(a_original_source_bytes) > inherited_bytes:
                        source_prefix_ok = source_prefix_ok and imported_source == a_original_source_bytes
                    report["source_transfer"] = {
                        "a_original_bytes": len(a_original_source_bytes),
                        "b_source_bytes": len(b_source_bytes), "inherited_byte_cutoff": inherited_bytes,
                        "a_imported_bytes": len(imported_source),
                        "inherited_prefix_matches": source_prefix_ok,
                        "a_existing_prefix_preserved": original_prefix_ok,
                        "b_discarded_tail_required_in_a": False,
                    }
                    report["a_intended_tip"] = str(intended_a_path)
                    save("a-metadata-before-cold-resume", native_metadata(first, logical))
                    save("a-db-preservation", {"before": a_db_before, "after": a_db_after})
                    record("B to A transfer preserves new-tip bytes and the bounded inherited source prefix",
                           source_prefix_ok and original_prefix_ok)

                    stage = "cold native A resumes only by logical ID"
                    with app_server_class(codex, first_env, project, artifacts, "current-tip-a-cold-resume") as app:
                        # Deliberately no path override and no DB repair. This is the subject of the test.
                        resumed_a = app.request("thread/resume", {"threadId": logical, "excludeTurns": True})["thread"]
                        actual_a_path = Path(resumed_a["path"])
                        assert resumed_a["id"] == logical and actual_a_path.is_relative_to(first)
                        correct_tip = actual_a_path.resolve() == intended_a_path.resolve()
                        report["a_actual_tip"] = str(actual_a_path)
                        report["native_tip_resolution"] = "intended_tip" if correct_tip else "stale_or_wrong_tip"
                        report["state_adapter_needed_evidence"] = not correct_tip
                        record(TIP_SELECTION_CHECK, correct_tip,
                               intended=str(intended_a_path), actual=str(actual_a_path))
                        a_items, a_item_pages = paginate(app, "thread/items/list", logical)
                        a_turns, a_turn_pages = paginate(app, "thread/turns/list", logical)
                        save("actual-a-items-before-continuation", a_items)
                        save("actual-a-turns-before-continuation", a_turns)
                        report.update({"a_item_pages": a_item_pages, "a_turn_pages": a_turn_pages,
                                       "a_ui_markers": marker_checks(a_items, markers),
                                       "full_item_parity": a_items == b_items,
                                       "full_turn_parity": a_turns == b_turns})
                        a_visible = report["a_ui_markers"]
                        a_ui_ok = all(a_visible[name] for name in ("a-one", "a-two", "b-keep")) \
                            and not a_visible["b-discard"]
                        record("cold A UI contains retained history and excludes B's discarded turn", a_ui_ok,
                               markers=a_visible)
                        record("cold A full paginated item payloads equal B after revert", a_items == b_items)
                        record("cold A full paginated turn payloads equal B after revert", a_turns == b_turns)

                        # Continue even after a wrong tip so diagnostics establish whether discarded
                        # history reaches model input. Everything here is synthetic and disposable.
                        stage = "native A model replay and continuation"
                        with server.lock:
                            prior_requests = len(server.requests)
                        app.turn(logical, markers["a-probe"])
                        with server.lock:
                            new_requests = list(server.requests[prior_requests:])
                        assert new_requests, "No local mock model request was observed"
                        inputs = [request["input"] for request in new_requests]
                        replay = marker_checks(inputs, markers)
                        report["model_replay_markers"] = replay
                        save("a-model-input", inputs)
                        model_ok = all(replay[name] for name in ("a-one", "a-two", "b-keep", "a-probe")) \
                            and not replay["b-discard"]
                        record("A model replay includes retained history and excludes B's discarded turn", model_ok,
                               markers=replay)
                        a_after_items, _ = paginate(app, "thread/items/list", logical)
                        save("actual-a-items-after-continuation", a_after_items)
                        after_markers = marker_checks(a_after_items, markers)
                        report["a_ui_markers_after_continuation"] = after_markers
                        after_ok = all(after_markers[name] for name in ("a-one", "a-two", "b-keep", "a-probe")) \
                            and not after_markers["b-discard"]
                        record("A UI after continuation retains history and excludes B's discarded turn", after_ok)
                    save("a-metadata-after-cold-resume", native_metadata(first, logical))
                    if native_history:
                        record("continuation appends to the selected physical tip and preserves its ancestor",
                               intended_a_path.read_bytes().startswith(b_tip_bytes)
                               and markers["a-probe"].encode() in intended_a_path.read_bytes()
                               and old_a_source.read_bytes() == imported_source)
                        stage = "second cold native A resume after continuation"
                        with app_server_class(codex, first_env, project, artifacts, "current-tip-a-second-cold-resume") as app:
                            restarted = app.request("thread/resume", {"threadId": logical, "excludeTurns": True})["thread"]
                            record("second cold logical-ID resume retains the selected physical tip",
                                   Path(restarted["path"]).resolve() == intended_a_path.resolve())
                            restarted_items, _ = paginate(app, "thread/items/list", logical)
                            restarted_markers = marker_checks(restarted_items, markers)
                            record("second cold resume retains history and excludes discarded context",
                                   all(restarted_markers[name] for name in ("a-one", "a-two", "b-keep", "a-probe"))
                                   and not restarted_markers["b-discard"], markers=restarted_markers)
                        save("a-metadata-after-second-cold-resume", native_metadata(first, logical))
                    record("B's full source and reverted tip remain unchanged after A continuation",
                           b_source_path.read_bytes() == b_source_bytes and b_tip_path.read_bytes() == b_tip_bytes)
                    record("synthetic profiles contain no copied authentication files",
                           all(not (home / "auth.json").exists() for home in homes.values()))
                    with server.lock:
                        report["mock_response_count"] = len(server.requests)
                    report["completed_diagnostic"] = True
                    if not report["failures"]:
                        report["status"] = "passed"
                        report["conclusion"] = ("Opt-in native-history sync preserved selected tip, UI payloads, model replay and restart"
                                                if native_history else "JSONL-only sync preserved current-tip selection, UI payloads and model replay in this fixture")
                    elif report["state_adapter_needed_evidence"]:
                        report["status"] = "blocked"
                        report["conclusion"] = "JSONL-only sync did not update native logical-ID tip selection; a state metadata publication mechanism is required"
                    else:
                        report["status"] = "blocked"
                        report["conclusion"] = "Native selected the intended tip, but history projection or replay acceptance still failed"
                finally:
                    server.close()
    except BaseException as error:
        report["error"] = str(error)
        report["failed_stage"] = stage
        (artifacts / "current-tip-traceback.log").write_text(traceback.format_exc())
    finally:
        save("summary", report)
    return report


def diagnostic_exit_code(report, *, expect_stale_tip=False):
    """Verify an explicit experimental baseline without changing acceptance status."""
    if not expect_stale_tip:
        return 0 if report.get("status") == "passed" and report.get("completed_diagnostic") is True and "error" not in report else 1
    expected_blocker = (
        report.get("status") == "blocked"
        and not report.get("native_history_sync")
        and report.get("completed_diagnostic") is True
        and report.get("native_tip_resolution") == "stale_or_wrong_tip"
        and report.get("state_adapter_needed_evidence") is True
        and report.get("failures") == [TIP_SELECTION_CHECK]
        and "error" not in report
    )
    return 0 if expected_blocker else 1


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--codex", required=True, type=Path)
    parser.add_argument("--codexm", required=True, type=Path)
    parser.add_argument("--native-harness", required=True, type=Path)
    parser.add_argument("--artifacts", required=True, type=Path)
    parser.add_argument("--sync-extra-arg", action="append", default=[])
    parser.add_argument("--native-history", action="store_true",
                        help="Require real opt-in native publication to pass, including a second cold resume")
    parser.add_argument("--fault-codexm", type=Path, help="Separate CLI built with nativehistorytest")
    parser.add_argument("--fault-point", choices=("files:verified", "sql:returned", "registry:verified"))
    parser.add_argument("--helper-fault-point", choices=("history_committed", "state_before_commit", "state_after_commit"))
    parser.add_argument("--expect-stale-tip", action="store_true",
                        help="Verify only the known JSONL-only tip blocker; acceptance remains blocked")
    args = parser.parse_args()
    if args.native_history and args.expect_stale_tip:
        parser.error("native-history acceptance cannot expect the JSONL-only blocker")
    injecting = bool(args.fault_point or args.helper_fault_point)
    if (bool(args.fault_codexm) != injecting or (injecting and not args.native_history)
            or (args.fault_point and args.helper_fault_point)):
        parser.error("fault acceptance requires --native-history, --fault-codexm and exactly one fault point")
    specification = importlib.util.spec_from_file_location("native_tip_acceptance", args.native_harness.resolve(strict=True))
    native = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(native)
    checks = []
    result = run_current_tip_checks(args.codex, args.codexm, native, native.AppServer,
                                    args.artifacts, checks, sync_extra_args=tuple(args.sync_extra_arg),
                                    native_history=args.native_history, fault_codexm=args.fault_codexm,
                                    fault_point=args.fault_point, helper_fault_point=args.helper_fault_point)
    print(json.dumps(result, indent=2))
    raise SystemExit(diagnostic_exit_code(result, expect_stale_tip=args.expect_stale_tip))


if __name__ == "__main__":
    main()
