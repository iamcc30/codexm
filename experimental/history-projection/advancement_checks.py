"""Isolated proof of authenticated advancement of an existing history partition.

All target mutations below manufacture disposable synthetic fixtures. The real
pinned Rust publisher performs every operation being accepted or rejected.
"""
import copy
import hashlib
import json
from pathlib import Path
import selectors
import signal
import sqlite3
import subprocess
import time
from contextlib import closing

from publisher_checks import TABLES, SENTINEL, add_unrelated, clone, mutate, quoted, remove_partition, rows, state


def run_advancement_checks(projector, publisher, source_db, manifest, root, env, artifacts, checks):
    root = root.resolve()
    assert root.name.startswith("codexm-native-acceptance-")
    work = root / "advancement-probe"
    work.mkdir()
    entries = manifest["rollouts"]
    # Prefer an ancestor containing several native records. A midpoint cutoff
    # intentionally exercises incomplete turns whose rows legitimately change.
    entry = max(entries, key=lambda value: value["end_byte_offset"])
    physical = entry["rollout_id"]
    source_path = Path(entry["path"])
    source_before = source_path.read_bytes()
    source_prefix = source_before[:entry["end_byte_offset"]]
    lines = source_prefix.splitlines(keepends=True)
    assert len(lines) >= 4 and all(line.endswith(b"\n") for line in lines)
    started = [index + 1 for index, line in enumerate(lines[:-1])
               if json.loads(line).get("payload", {}).get("type") == "task_started"]
    assert started, "Native fixture must include a turn-start event"
    count = started[0]
    old_prefix = b"".join(lines[:count])
    old_ordinal = json.loads(lines[count - 1])["ordinal"] + 1
    assert old_ordinal < entry["end_ordinal_exclusive"]
    base_manifest = dict(source_root=manifest["source_root"], output_dir=str(work / "base-projection"),
                         rollouts=[dict(entry, end_byte_offset=len(old_prefix),
                                        end_ordinal_exclusive=old_ordinal,
                                        prefix_sha256=hashlib.sha256(old_prefix).hexdigest())])
    result = subprocess.run([str(projector)], input=json.dumps(base_manifest), text=True,
                            capture_output=True, env=env, timeout=120)
    (artifacts / "advancement-base-projection.stdout.json").write_text(result.stdout)
    (artifacts / "advancement-base-projection.stderr.log").write_text(result.stderr)
    assert result.returncode == 0, (result.stdout, result.stderr)
    base_db = Path(base_manifest["output_dir"]) / "thread_history_1.sqlite"
    with closing(sqlite3.connect(base_db)) as connection:
        assert any(rows(connection, table, physical) for table in TABLES[:-1])
    changed_existing = False
    with closing(sqlite3.connect(base_db)) as base, closing(sqlite3.connect(source_db)) as full:
        for table, key in (("thread_turns", "turn_id"), ("thread_items", "item_id")):
            names = [column[1] for column in base.execute("PRAGMA table_info(" + quoted(table) + ")")]
            key_index = names.index(key)
            old_rows = {row[key_index]: row for row in rows(base, table, physical)}
            full_rows = {row[key_index]: row for row in rows(full, table, physical)}
            changed_existing |= any(value != full_rows[identifier] for identifier, value in old_rows.items()
                                    if identifier in full_rows)
    assert changed_existing, "Fixture must change an existing turn/item, not just insert later rows"
    destination_dir = work / "destination"
    destination_dir.mkdir()
    destination = destination_dir / source_path.name
    destination.write_bytes(old_prefix)
    source_state = state(source_db)
    base_state = state(base_db)
    calls = []

    def target(name):
        path = clone(source_db, work / (name + ".sqlite"))
        def make_old(connection):
            add_unrelated(connection, physical)
            remove_partition(connection, physical)
            connection.execute("ATTACH DATABASE ? AS old_source", (str(base_db),))
            for table in TABLES:
                connection.execute("INSERT INTO main." + quoted(table) + " SELECT * FROM old_source."
                                   + quoted(table) + " WHERE thread_id=?", (physical,))
        mutate(path, make_old)
        return path

    def request(path):
        rollouts = [{key: value for key, value in item.items() if key in
                     ("rollout_id", "end_byte_offset", "end_ordinal_exclusive")} for item in entries]
        selected = next(item for item in rollouts if item["rollout_id"] == physical)
        selected["advance"] = dict(source_path=str(source_path), destination_path=str(destination),
                                   base_end_byte_offset=len(old_prefix), base_end_ordinal_exclusive=old_ordinal,
                                   base_prefix_sha256=hashlib.sha256(old_prefix).hexdigest(),
                                   prefix_sha256=hashlib.sha256(source_prefix).hexdigest())
        return dict(synthetic_test_root=str(root), staging_db=str(source_db), target_db=str(path),
                    advance_source_root=manifest["source_root"], rollouts=rollouts, busy_timeout_ms=100)

    def invoke(name, data, success):
        completed = subprocess.run([str(publisher)], input=json.dumps(data), text=True, capture_output=True,
                                   env=env, timeout=120)
        (artifacts / ("advancement-" + name + ".stdout.json")).write_text(completed.stdout)
        (artifacts / ("advancement-" + name + ".stderr.log")).write_text(completed.stderr)
        calls.append(dict(case=name, returncode=completed.returncode))
        assert (completed.returncode == 0) == success, (name, completed.stdout, completed.stderr)
        if success:
            report = json.loads(completed.stdout)
            assert report["status"] == "published" and report["diagnostic_only"] is True
            assert report["native_writer_exclusion"] is False
            assert report["filesystem_database_atomicity"] is False
            return report
        return completed

    def selected(data):
        return next(item for item in data["rollouts"] if item["rollout_id"] == physical)

    expected = clone(source_db, work / "expected.sqlite")
    mutate(expected, lambda connection: add_unrelated(connection, physical))
    expected_state = state(expected)

    missing_proof = target("missing-proof")
    before = state(missing_proof)
    data = request(missing_proof)
    del selected(data)["advance"]
    del data["advance_source_root"]
    invoke("behind-without-proof", data, False)
    assert state(missing_proof) == before
    checks.append("existing shorter partition remains rejected without authenticated native base proof")

    valid = target("valid")
    report = invoke("valid-native-advance", request(valid), True)
    assert report["advanced_rollouts"] == [physical], report
    assert state(valid) == expected_state
    checks.append("native old-prefix equality authorizes exact selected-partition advancement and preserves unrelated rows")
    report = invoke("idempotent-native-advance", request(valid), True)
    assert report["advanced_rollouts"] == [] and state(valid) == expected_state
    checks.append("retry after completed advancement retains the exact full partition")

    for label, change in [
        ("wrong-base-hash", lambda value: value.update(base_prefix_sha256="0" * 64)),
        ("wrong-full-hash", lambda value: value.update(prefix_sha256="0" * 64)),
        ("wrong-base-byte-bound", lambda value: value.update(base_end_byte_offset=len(old_prefix) - 1)),
        ("wrong-base-ordinal", lambda value: value.update(base_end_ordinal_exclusive=old_ordinal + 1)),
    ]:
        path = target(label)
        before = state(path)
        data = request(path)
        change(selected(data)["advance"])
        invoke(label, data, False)
        assert state(path) == before
        checks.append("advancement rejects " + label + " without changing target rows")

    path = target("destination-prefix-conflict")
    before = state(path)
    destination.write_bytes(b"x" + old_prefix[1:])
    try:
        invoke("destination-prefix-conflict", request(path), False)
    finally:
        destination.write_bytes(old_prefix)
    assert state(path) == before
    checks.append("advancement requires installed destination prefix to match native base proof")

    # Already-installed appended bytes are permitted only because the old prefix
    # is unchanged. This is required for journal recovery after file publication.
    path = target("destination-already-new")
    destination.write_bytes(source_prefix)
    try:
        invoke("destination-already-new", request(path), True)
    finally:
        destination.write_bytes(old_prefix)
    assert state(path) == expected_state
    checks.append("advancement recovers when newer file is installed but database is still old")

    extra_record = json.loads(lines[-1])
    extra_record["ordinal"] = entry["end_ordinal_exclusive"]
    compatible_extra = json.dumps(extra_record, separators=(",", ":")).encode() + b"\n"
    divergent = source_prefix[:len(old_prefix)] + b"x" + source_prefix[len(old_prefix) + 1:] + compatible_extra
    for label, tail in (("compatible-longer-tail", source_prefix + compatible_extra),
                        ("divergent-longer-tail", divergent),
                        ("partial-newer-tail", source_prefix[:len(old_prefix) + 1])):
        path = target(label)
        before = state(path)
        destination.write_bytes(tail)
        success = label == "compatible-longer-tail"
        try:
            outcome = invoke(label, request(path), success)
            if label == "divergent-longer-tail":
                assert "SHA-256 mismatch" in outcome.stderr
            assert destination.read_bytes() == tail
        finally:
            destination.write_bytes(old_prefix)
        assert state(path) == (expected_state if success else before)
        checks.append("advancement " + ("preserves " if success else "rejects ") + label)

    path = target("target-payload-conflict")
    def corrupt(connection):
        cursor = connection.execute("UPDATE thread_turns SET status='synthetic_conflict' WHERE thread_id=?", (physical,))
        assert cursor.rowcount > 0
    mutate(path, corrupt)
    before = state(path)
    invoke("target-payload-conflict", request(path), False)
    assert state(path) == before
    checks.append("matching old checkpoint cannot authorize altered existing projection payloads")

    def extra_existing(connection):
        names = [column[1] for column in connection.execute("PRAGMA table_info(thread_turns)")]
        original = rows(connection, "thread_turns", physical)
        used = {row[names.index("rollout_ordinal")] for row in original}
        available = next((ordinal for ordinal in range(old_ordinal) if ordinal not in used), None)
        assert available is not None, "Fixture needs an unused bounded ordinal for an extra row"
        value = list(original[0])
        value[names.index("turn_id")] += "-synthetic-extra"
        value[names.index("rollout_ordinal")] = available
        connection.execute("INSERT INTO thread_turns (" + ",".join(map(quoted, names)) + ") VALUES ("
                           + ",".join("?" for _ in names) + ")", value)

    for label, alter in (
        ("missing-existing-row", lambda connection: connection.execute("DELETE FROM thread_turns WHERE thread_id=?", (physical,))),
        ("extra-existing-row", extra_existing),
    ):
        path = target(label)
        mutate(path, alter)
        before = state(path)
        invoke(label, request(path), False)
        assert state(path) == before
        checks.append("old-prefix equality rejects " + label + " without mutation")

    path = target("stage-payload-conflict")
    bad_stage = clone(source_db, work / "altered-stage.sqlite")
    mutate(bad_stage, corrupt)
    before = state(path)
    data = request(path)
    data["staging_db"] = str(bad_stage)
    invoke("stage-payload-conflict", data, False)
    assert state(path) == before
    checks.append("advancement verifies requested full stage against independently rebuilt native projection")

    path = target("late-partition-conflict")
    other = next(item["rollout_id"] for item in entries if item["rollout_id"] != physical)
    def late_conflict(connection):
        changed = connection.execute("UPDATE thread_items SET item_json=? WHERE thread_id=?",
                                     ('{"synthetic_late_conflict":true}', other))
        assert changed.rowcount > 0
    mutate(path, late_conflict)
    before = state(path)
    data = request(path)
    data["rollouts"].sort(key=lambda item: item["rollout_id"] != physical)
    invoke("late-partition-conflict", data, False)
    assert state(path) == before
    checks.append("later partition conflict rolls back an earlier authenticated advancement")

    for label, source_value in (("source-symlink", source_path), ("destination-symlink", destination)):
        link = work / (label + ".jsonl")
        link.symlink_to(source_value)
        path = target(label)
        before = state(path)
        data = request(path)
        selected(data)["advance"]["source_path" if label.startswith("source") else "destination_path"] = str(link)
        invoke(label, data, False)
        assert state(path) == before
        checks.append("advancement rejects " + label + " without mutation")

    path = target("rollback")
    before = state(path)
    data = request(path)
    data["fail_before_commit"] = True
    failure = invoke("rollback-after-advance", data, False)
    assert "CODEXM_PUBLISH_BEFORE_COMMIT" in failure.stderr
    assert "injected failure before commit" in failure.stderr
    assert state(path) == before
    checks.append("failure after selected-partition replacement rolls back old rows and checkpoint together")

    path = target("killed")
    before = state(path)
    data = request(path)
    data["pause_before_commit_ms"] = 5000
    child = subprocess.Popen([str(publisher)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                             stderr=subprocess.PIPE, text=True, env=env)
    child.stdin.write(json.dumps(data))
    child.stdin.close()
    selector = selectors.DefaultSelector()
    selector.register(child.stderr, selectors.EVENT_READ)
    lines_seen = []
    try:
        deadline = time.monotonic() + 120
        reached = False
        while time.monotonic() < deadline and child.poll() is None:
            if not selector.select(timeout=0.1):
                continue
            line = child.stderr.readline()
            lines_seen.append(line)
            if "CODEXM_PUBLISH_BEFORE_COMMIT" in line:
                reached = True
                break
        assert reached, "Publisher did not reach advancement kill point: " + "".join(lines_seen)
        child.kill()
        child.wait(timeout=5)
        assert child.returncode == -signal.SIGKILL
    finally:
        selector.close()
        if child.poll() is None:
            child.kill()
            child.wait(timeout=5)
        child.stdout.close()
        child.stderr.close()
    assert state(path) == before
    invoke("retry-after-advance-kill", request(path), True)
    assert state(path) == expected_state
    checks.append("SIGKILL before advancement commit preserves old partition and verified retry succeeds")

    assert source_path.read_bytes() == source_before
    assert state(source_db) == source_state and state(base_db) == base_state
    assert destination.read_bytes() == old_prefix
    (artifacts / "advancement-cases.json").write_text(json.dumps(calls, indent=2) + "\n")
    checks.append("advancement probes preserve authenticated source snapshots and source databases")
