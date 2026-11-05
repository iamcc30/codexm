"""Synthetic-only checks for transactional publication; never a production publisher."""
import copy
import json
import os
from pathlib import Path
import selectors
import shutil
import sqlite3
import subprocess
import time
from contextlib import closing

TABLES = ("thread_turns", "thread_items", "thread_realtime_items", "thread_history_projection_state")
SENTINEL = "00000000-0000-4000-8000-00000000c0de"


def quoted(name):
    return '"' + name.replace('"', '""') + '"'


def rows(connection, table, physical=None):
    sql = "SELECT * FROM " + quoted(table)
    values = ()
    if physical is not None:
        sql += " WHERE thread_id = ?"
        values = (physical,)
    return sorted(connection.execute(sql, values).fetchall(), key=repr)


def state(path):
    # Test targets can have a hot journal after forced termination. Let SQLite
    # perform its normal recovery on this disposable database; never delete WAL.
    with closing(sqlite3.connect(path)) as connection:
        assert connection.execute("PRAGMA integrity_check").fetchone() == ("ok",)
        schema = connection.execute("SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name").fetchall()
        tables = connection.execute("SELECT name FROM sqlite_master WHERE type='table' ORDER BY name").fetchall()
        result = (schema, [(name, rows(connection, name)) for (name,) in tables])
        return result


def mutate(path, operation):
    with closing(sqlite3.connect(path)) as connection:
        operation(connection)
        connection.commit()


def remove_partition(connection, physical):
    for table in TABLES:
        connection.execute("DELETE FROM " + quoted(table) + " WHERE thread_id = ?", (physical,))


def clone(source, destination):
    assert not destination.exists()
    assert not Path(str(source) + "-wal").exists(), "Clone requires a finished database"
    assert not Path(str(source) + "-shm").exists(), "Clone requires a finished database"
    shutil.copyfile(source, destination)
    return destination


def add_unrelated(connection, physical):
    for table in TABLES:
        names = [column[1] for column in connection.execute("PRAGMA table_info(" + quoted(table) + ")")]
        selected = ["?" if name == "thread_id" else quoted(name) for name in names]
        connection.execute("INSERT INTO " + quoted(table) + " (" + ",".join(map(quoted, names)) + ") SELECT "
                           + ",".join(selected) + " FROM " + quoted(table) + " WHERE thread_id = ?", (SENTINEL, physical))


def run_publisher_checks(binary, source_db, manifest, root, env, artifacts, checks, native_source_home=None):
    root = root.resolve()
    assert root.name.startswith("codexm-native-acceptance-"), "Only native acceptance synthetic roots allowed"
    (root / "CODEXM_SYNTHETIC_PROJECTION_TEST").write_text("codexm synthetic projection publication\n")
    work = root / "publisher-probe"
    work.mkdir()
    rollouts = [{key: value for key, value in entry.items() if key in
                 ("rollout_id", "end_byte_offset", "end_ordinal_exclusive")} for entry in manifest["rollouts"]]
    physical = rollouts[0]["rollout_id"]
    assert len(rollouts) >= 2
    source_before = source_db.read_bytes()
    source_state = state(source_db)
    calls = []

    def request(target, **extra):
        return dict(synthetic_test_root=str(root), staging_db=str(source_db), target_db=str(target),
                    rollouts=copy.deepcopy(rollouts), busy_timeout_ms=100, **extra)

    def invoke(name, data, success):
        completed = subprocess.run([str(binary)], input=json.dumps(data), text=True, capture_output=True,
                                   env=env, timeout=15)
        (artifacts / ("publisher-" + name + ".stdout.json")).write_text(completed.stdout)
        (artifacts / ("publisher-" + name + ".stderr.log")).write_text(completed.stderr)
        calls.append({"case": name, "returncode": completed.returncode})
        assert (completed.returncode == 0) == success, (name, completed.stdout, completed.stderr)
        if success:
            result = json.loads(completed.stdout)
            assert result["status"] == "published" and result["diagnostic_only"] is True, result
            assert result["source_hashes_verified"] is False, "Do not overstate synthetic publisher guarantees"
            assert result["native_writer_exclusion"] is False
            assert result["filesystem_database_atomicity"] is False
            return result
        return completed

    rejection_target = clone(source_db, work / "preflight-target.sqlite")
    rejection_before = state(rejection_target)
    guard_cases = []
    invalid = request(rejection_target); invalid["unexpected"] = True
    guard_cases.append(("unknown-field", invalid))
    invalid = request(rejection_target); invalid["rollouts"].append(copy.deepcopy(invalid["rollouts"][0]))
    guard_cases.append(("duplicate-id", invalid))
    invalid = request(rejection_target); invalid["rollouts"][0]["end_byte_offset"] = 0
    guard_cases.append(("invalid-bound", invalid))
    invalid = request(source_db)
    guard_cases.append(("same-database", invalid))
    link = work / "target-symlink.sqlite"; link.symlink_to(rejection_target)
    guard_cases.append(("target-symlink", request(link)))
    hardlink = work / "hardlinked.sqlite"; hardlink_copy = work / "hardlinked-copy.sqlite"
    clone(source_db, hardlink); os.link(hardlink, hardlink_copy)
    guard_cases.append(("target-hardlink", request(hardlink)))
    for name, data in guard_cases:
        actual_target = Path(data["target_db"]).resolve()
        actual_before = state(actual_target)
        invoke(name, data, False)
        assert state(actual_target) == actual_before
        assert state(rejection_target) == rejection_before
        assert source_db.read_bytes() == source_before
        checks.append("publisher rejects " + name + " without changing target rows")
    marker = root / "CODEXM_SYNTHETIC_PROJECTION_TEST"
    marker_backup = marker.with_name(marker.name + ".saved")
    marker.rename(marker_backup)
    try:
        invoke("missing-synthetic-marker", request(rejection_target), False)
    finally:
        marker_backup.rename(marker)
    assert state(rejection_target) == rejection_before
    checks.append("publisher requires explicit synthetic-root marker")

    # Repair an absent partition while an unrelated thread and other requested
    # partitions already exist. Never replace a whole existing DB.
    repair = clone(source_db, work / "repair.sqlite")
    def missing(connection):
        add_unrelated(connection, physical)
        remove_partition(connection, physical)
    mutate(repair, missing)
    with closing(sqlite3.connect(repair)) as connection:
        unrelated_before = [rows(connection, table, SENTINEL) for table in TABLES]
    report = invoke("insert-missing", request(repair), True)
    assert report["inserted_rollouts"] == [physical], report
    with closing(sqlite3.connect(repair)) as connection, closing(sqlite3.connect(source_db)) as source:
        assert [rows(connection, table, SENTINEL) for table in TABLES] == unrelated_before
        for entry in rollouts:
            assert [rows(connection, table, entry["rollout_id"]) for table in TABLES] == [rows(source, table, entry["rollout_id"]) for table in TABLES]
    checks.append("transaction inserts only missing physical partition and preserves unrelated rows")
    before = state(repair)
    report = invoke("idempotent", request(repair), True)
    assert report["inserted_rollouts"] == [] and state(repair) == before
    checks.append("repeated publication is an exact logical no-op")

    failed = clone(source_db, work / "rollback.sqlite")
    mutate(failed, lambda connection: remove_partition(connection, physical))
    before = state(failed)
    injected = invoke("rollback-before-commit", request(failed, fail_before_commit=True), False)
    assert "CODEXM_PUBLISH_BEFORE_COMMIT" in injected.stderr
    assert "injected failure before commit" in injected.stderr
    assert state(failed) == before
    checks.append("injected failure after writes rolls back the complete transaction")

    # A conflict after an earlier missing partition must also roll back that insert.
    conflict = clone(source_db, work / "conflict.sqlite")
    def conflicting(connection):
        remove_partition(connection, physical)
        other = rollouts[1]["rollout_id"]
        assert connection.execute("SELECT count(*) FROM thread_items WHERE thread_id=?", (other,)).fetchone()[0] > 0
        connection.execute("UPDATE thread_items SET item_json=? WHERE thread_id=?", ('{"synthetic_conflict":true}', other))
    mutate(conflict, conflicting)
    before = state(conflict)
    invoke("payload-conflict", request(conflict), False)
    assert state(conflict) == before
    checks.append("later partition conflict rolls back earlier inserts without overwriting payloads")

    # Checkpoint state cannot authorize missing or modified rows.
    partial = clone(source_db, work / "partial.sqlite")
    mutate(partial, lambda connection: connection.execute("DELETE FROM thread_items WHERE thread_id=?", (physical,)))
    before = state(partial)
    invoke("partial-projection", request(partial), False)
    assert state(partial) == before
    checks.append("partial existing projection fails closed")

    future = clone(source_db, work / "beyond-checkpoint.sqlite")
    def orphan_future(connection):
        names = [column[1] for column in connection.execute("PRAGMA table_info(thread_items)")]
        expressions = []
        for name in names:
            if name == "item_id": expressions.append("item_id || '-synthetic-orphan'")
            elif name in ("rollout_ordinal", "updated_at_ordinal"): expressions.append(str(rollouts[0]["end_ordinal_exclusive"] + 100))
            else: expressions.append(quoted(name))
        connection.execute("INSERT INTO thread_items (" + ",".join(map(quoted, names)) + ") SELECT " + ",".join(expressions)
                           + " FROM thread_items WHERE thread_id=? LIMIT 1", (physical,))
    mutate(future, orphan_future)
    before = state(future)
    invoke("rows-beyond-checkpoint", request(future), False)
    assert state(future) == before
    checks.append("matching visible prefix cannot hide orphan rows beyond destination checkpoint")

    for name, ordinal in (("future-update", rollouts[0]["end_ordinal_exclusive"]), ("regressed-update", -1)):
        target = clone(source_db, work / (name + ".sqlite"))
        mutate(target, lambda connection: connection.execute("UPDATE thread_items SET updated_at_ordinal=? WHERE thread_id=?", (ordinal, physical)))
        before = state(target)
        invoke(name, request(target), False)
        assert state(target) == before
        checks.append("publisher rejects " + name + " without changing payloads")

    behind = clone(source_db, work / "behind.sqlite")
    mutate(behind, lambda connection: connection.execute("UPDATE thread_history_projection_state SET next_rollout_byte_offset=0,next_rollout_ordinal=0 WHERE thread_id=?", (physical,)))
    before = state(behind)
    invoke("behind-checkpoint", request(behind), False)
    assert state(behind) == before
    checks.append("checkpoint behind requested prefix fails closed")

    for label, operation in [
        ("unknown-migration", lambda connection: connection.execute("INSERT INTO _sqlx_migrations (version,description,success,checksum,execution_time) SELECT 999,'synthetic future',1,checksum,0 FROM _sqlx_migrations LIMIT 1")),
        ("migration-checksum", lambda connection: connection.execute("UPDATE _sqlx_migrations SET checksum=x'00' WHERE version=1")),
        ("schema-drift", lambda connection: connection.execute("CREATE TABLE synthetic_unknown (value TEXT)")),
    ]:
        target = clone(source_db, work / (label + ".sqlite"))
        def apply_checked(connection):
            cursor = operation(connection)
            if label != "schema-drift":
                assert cursor.rowcount == 1, (label, cursor.rowcount)
        mutate(target, apply_checked)
        before = state(target)
        invoke(label, request(target), False)
        assert state(target) == before
        checks.append("rejects " + label + " without changing existing data")

    # Consistent drift in BOTH supplied DBs must not defeat the compiled pin.
    bad_stage = clone(source_db, work / "bad-stage.sqlite")
    bad_target = clone(source_db, work / "bad-target.sqlite")
    for path in (bad_stage, bad_target):
        mutate(path, lambda connection: connection.execute("CREATE TABLE synthetic_unknown (value TEXT)"))
    before = state(bad_target)
    data = request(bad_target); data["staging_db"] = str(bad_stage)
    invoke("matching-drift-not-pin", data, False)
    assert state(bad_target) == before
    checks.append("matching stage and target drift cannot bypass compiled schema pin")

    locked = clone(source_db, work / "locked.sqlite")
    before = state(locked)
    with closing(sqlite3.connect(locked)) as locker:
        locker.execute("BEGIN IMMEDIATE")
        start = time.monotonic()
        busy = invoke("busy-writer", request(locked), False)
        assert "locked" in busy.stderr.lower() or "busy" in busy.stderr.lower(), busy.stderr
        assert "CODEXM_PUBLISH_BEFORE_COMMIT" not in busy.stderr
        assert time.monotonic() - start < 5
        locker.rollback()
    assert state(locked) == before
    checks.append("concurrent SQLite writer causes bounded failure without mutation")

    killed = clone(source_db, work / "killed.sqlite")
    mutate(killed, lambda connection: remove_partition(connection, physical))
    before = state(killed)
    child = subprocess.Popen([str(binary)], stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             text=True, env=env)
    child.stdin.write(json.dumps(request(killed, pause_before_commit_ms=5000)))
    child.stdin.close()
    selector = selectors.DefaultSelector()
    selector.register(child.stderr, selectors.EVENT_READ)
    marker_seen = False
    transcript = []
    deadline = time.monotonic() + 10
    try:
        while time.monotonic() < deadline and child.poll() is None:
            if not selector.select(timeout=0.1):
                continue
            line = child.stderr.readline()
            transcript.append(line)
            if "CODEXM_PUBLISH_BEFORE_COMMIT" in line:
                marker_seen = True
                break
        assert marker_seen, "Publisher did not reach pre-commit kill point: " + "".join(transcript)
        child.kill(); child.wait(timeout=5)
    finally:
        selector.close()
        if child.poll() is None:
            child.kill(); child.wait(timeout=5)
        child.stdout.close(); child.stderr.close()
    (artifacts / "publisher-killed.stderr.log").write_text("".join(transcript))
    assert state(killed) == before, "Normal SQLite crash recovery did not preserve pre-transaction rows"
    invoke("retry-after-kill", request(killed), True)
    assert state(killed) == source_state
    checks.append("process kill before commit recovers atomically and retry succeeds")

    # The caller has already fully closed the native first-create process.
    # Never use this raw file-copy fixture with a concurrently running writer.
    # Native A deliberately retains the reverted/discarded tail. Preserve that
    # ahead partition in full rather than truncating it to a descendant cutoff.
    if native_source_home is not None:
        original = native_source_home / "thread_history_1.sqlite"
        snapshot = work / "native-copy"
        snapshot.mkdir()
        for suffix in ("", "-wal", "-shm"):
            path = Path(str(original) + suffix)
            if path.exists():
                shutil.copyfile(path, snapshot / (original.name + suffix))
        ahead = work / "ahead.sqlite"
        with closing(sqlite3.connect(snapshot / original.name)) as source, closing(sqlite3.connect(ahead)) as target:
            source.backup(target)
        before = state(ahead)
        with closing(sqlite3.connect(ahead)) as connection:
            assert any(connection.execute("SELECT next_rollout_ordinal FROM thread_history_projection_state WHERE thread_id=?", (entry["rollout_id"],)).fetchone()[0] > entry["end_ordinal_exclusive"] for entry in rollouts), "Fixture must include a genuinely ahead partition"
        report = invoke("retain-native-ahead", request(ahead), True)
        assert report["inserted_rollouts"] == [] and state(ahead) == before
        checks.append("bounded publication preserves native longer discarded-tail partition exactly")

    assert source_db.read_bytes() == source_before, "Publisher changed its staging source bytes"
    assert state(source_db) == source_state
    (artifacts / "publisher-cases.json").write_text(json.dumps(calls, indent=2) + "\n")
    checks.append("publisher leaves staging database unchanged")
