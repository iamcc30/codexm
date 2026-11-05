#!/usr/bin/env python3
"""Isolated Linux synthetic recovery proof, not a codexm production publisher.

Run --self-test for the Python transaction model. Native coverage requires
run_recovery_checks() with the built, pinned publish_projection executable.
"""
import argparse
import copy
import ctypes
import fcntl
import hashlib
import json
import os
from pathlib import Path
import selectors
import signal
import sqlite3
import stat
import subprocess
import sys
import tempfile
import time
import uuid
from contextlib import closing, contextmanager

from publisher_checks import TABLES, SENTINEL, add_unrelated, clone, mutate, remove_partition, rows, state

MARKER = "CODEXM_SYNTHETIC_PROJECTION_TEST"
MARKER_BYTES = b"codexm synthetic projection publication\n"
VERSION = "codexm-synthetic-recovery-v1"
PLAN_FIELDS = {"version", "root", "profile", "staging_db", "staging_sha256", "target_db", "target_identity",
               "initial_target_sha256", "initial_partitions", "registry", "initial_registry_sha256", "rollouts"}
ENTRY_FIELDS = {"rollout_id", "end_byte_offset", "end_ordinal_exclusive", "prefix_sha256", "source",
                "destination", "destination_before", "registry_before"}


class Conflict(RuntimeError):
    pass


def require(condition, message):
    if not condition:
        raise Conflict(message)


def sha(data):
    return hashlib.sha256(data).hexdigest()


def encoded(value):
    return (json.dumps(value, sort_keys=True, separators=(",", ":"), ensure_ascii=True) + "\n").encode()


def path_in(root, value, missing=False, single=True):
    path = Path(value)
    require(path.is_absolute() and ".." not in path.parts, "absolute, dot-free path required")
    require(path == root or root in path.parents, "path outside caller-created synthetic root")
    current = Path(path.anchor)
    for part in path.parts[1:]:
        current /= part
        try:
            info = current.lstat()
        except FileNotFoundError:
            require(missing and current == path, "missing parent/input")
            return path
        require(not stat.S_ISLNK(info.st_mode), "symlink component forbidden")
    if path.is_file() and single:
        require(path.stat().st_nlink == 1, "hardlinked file forbidden")
    return path


def checked_root(value):
    root = Path(value)
    path_in(root, root)
    require(root.is_dir() and root.name.startswith("codexm-native-acceptance-"), "synthetic acceptance root required")
    marker = path_in(root, root / MARKER)
    require(marker.is_file() and marker.read_bytes() == MARKER_BYTES, "explicit caller-created marker required")
    return root


def sync_dir(path):
    fd = os.open(path, os.O_DIRECTORY | os.O_RDONLY)
    try:
        os.fsync(fd)
    finally:
        os.close(fd)


def sync_file(path):
    with path.open("rb") as handle:
        os.fsync(handle.fileno())


def hit(point, fault):
    if point == fault:
        os.kill(os.getpid(), signal.SIGKILL)


def atomic_json(path, value, point, fault):
    """Only owned journal files or the synthetic registry, while the gate is held."""
    temp = path.with_name(path.name + ".pending")
    require(not temp.is_symlink() and (not temp.exists() or temp.stat().st_nlink == 1), "unsafe owned temporary file")
    with temp.open("wb") as output:
        data = encoded(value)
        midpoint = max(1, len(data) // 2)
        output.write(data[:midpoint]); output.flush()
        hit(point + ":partial", fault)
        output.write(data[midpoint:])
        output.flush()
        hit(point + ":written", fault)
        os.fsync(output.fileno())
        hit(point + ":synced", fault)
    os.replace(temp, path)
    hit(point + ":renamed", fault)
    sync_dir(path.parent)
    hit(point + ":durable", fault)


def rename_noreplace(source, target):
    """Linux create-only publication: unlike os.rename, never replaces a winner."""
    libc = ctypes.CDLL(None, use_errno=True)
    function = getattr(libc, "renameat2", None)
    require(function is not None, "Linux renameat2 required by this diagnostic")
    function.argtypes = (ctypes.c_int, ctypes.c_char_p, ctypes.c_int, ctypes.c_char_p, ctypes.c_uint)
    function.restype = ctypes.c_int
    if function(-100, os.fsencode(source), -100, os.fsencode(target), 1) != 0:
        error = ctypes.get_errno()
        raise OSError(error, os.strerror(error), str(target))


@contextmanager
def gate(profile):
    lock = profile / "codexm-recovery.lock"
    require(not lock.is_symlink() and (not lock.exists() or lock.stat().st_nlink == 1), "unsafe gate file")
    with lock.open("a+b") as handle:
        try:
            fcntl.flock(handle, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError as error:
            raise Conflict("cooperative gate busy; a publisher may still be running") from error
        yield handle


def read_registry(path):
    value = json.loads(path.read_bytes())
    require(isinstance(value, dict) and set(value) == {"entries"} and isinstance(value["entries"], dict), "invalid synthetic registry")
    return value


def db_connection(path, immutable=False):
    # Target mode=rw permits SQLite's own hot-journal recovery; never delete sidecars.
    return sqlite3.connect(path.as_uri() + ("?mode=ro&immutable=1" if immutable else "?mode=rw"), uri=True, timeout=0.2)


def db_partitions(path, rollouts, immutable=False):
    with closing(db_connection(path, immutable)) as db:
        return {entry["rollout_id"]: sha(repr([rows(db, table, entry["rollout_id"]) for table in TABLES]).encode()) for entry in rollouts}


def inspect_projection(plan):
    """Inspect actual state, never infer commit from exit status or journal phase."""
    output = {}
    with closing(db_connection(Path(plan["staging_db"]), True)) as stage, closing(db_connection(Path(plan["target_db"]))) as target:
        require(target.execute("PRAGMA integrity_check").fetchone() == ("ok",), "target integrity failure")
        schema_sql = "SELECT type,name,tbl_name,sql FROM sqlite_master ORDER BY type,name"
        require(stage.execute(schema_sql).fetchall() == target.execute(schema_sql).fetchall(), "target schema differs from bound stage")
        migration_sql = "SELECT version,description,success,checksum FROM _sqlx_migrations ORDER BY version"
        require(stage.execute(migration_sql).fetchall() == target.execute(migration_sql).fetchall(), "target migration drift")
        for entry in plan["rollouts"]:
            physical, cutoff = entry["rollout_id"], entry["end_ordinal_exclusive"]
            bound = (entry["end_byte_offset"], cutoff)
            sql = "SELECT next_rollout_byte_offset,next_rollout_ordinal FROM thread_history_projection_state WHERE thread_id=?"
            require(stage.execute(sql, (physical,)).fetchone() == bound, "staging bounds mismatch")
            actual = target.execute(sql, (physical,)).fetchone()
            if actual is None:
                require(not any(rows(target, table, physical) for table in TABLES), "orphan target partition")
                output[physical] = "absent"
                continue
            require(actual == bound or (actual[0] > bound[0] and actual[1] > bound[1]), "incompatible target checkpoint")
            for table in TABLES[:-1]:
                for db, limit in ((stage, cutoff), (target, actual[1])):
                    invalid = db.execute(f'SELECT 1 FROM "{table}" WHERE thread_id=? AND (rollout_ordinal<0 OR rollout_ordinal>=?) LIMIT 1', (physical, limit)).fetchone()
                    require(not invalid, "rows outside checkpoint")
                expected = rows(stage, table, physical)
                visible = sorted(target.execute(f'SELECT * FROM "{table}" WHERE thread_id=? AND rollout_ordinal<?', (physical, cutoff)).fetchall(), key=repr)
                require(visible == expected, "changed or missing target prefix rows")
            for db, limit in ((stage, cutoff), (target, actual[1])):
                invalid = db.execute("SELECT 1 FROM thread_items WHERE thread_id=? AND (updated_at_ordinal<rollout_ordinal OR updated_at_ordinal>=?) LIMIT 1", (physical, limit)).fetchone()
                require(not invalid, "invalid update ordinal")
            output[physical] = "compatible"
    return output


def validate_layout(plan):
    require(isinstance(plan, dict) and set(plan) == PLAN_FIELDS, "unknown or missing plan fields")
    require(plan["version"] == VERSION, "unsupported protocol version")
    root = checked_root(plan["root"])
    profile = path_in(root, plan["profile"])
    require(profile.name == "profile" and profile.parent.parent == root / "recovery-probe", "profile outside fixed fixture layout")
    require(Path(plan["target_db"]) == profile / "thread_history_1.sqlite", "unexpected target DB path")
    require(Path(plan["registry"]) == profile / "synthetic-registry.json", "unexpected registry path")
    require(Path(plan["staging_db"]) == profile.parent / "stage.sqlite", "unexpected staging path")
    return root, profile


def validate_plan(plan):
    root, profile = validate_layout(plan)
    for name in ("staging_db", "target_db", "registry"):
        path_in(root, plan[name])
    require(Path(plan["target_db"]).parent == profile and Path(plan["registry"]).parent == profile, "target outside synthetic profile")
    stage = Path(plan["staging_db"])
    require(stage != Path(plan["target_db"]) and sha(stage.read_bytes()) == plan["staging_sha256"], "staging bytes changed")
    for name in ("staging_db", "target_db"):
        for suffix in ("-wal", "-shm", "-journal"):
            sidecar = path_in(root, plan[name] + suffix, missing=True)
            if name == "staging_db":
                require(not sidecar.exists(), "staging must be closed without sidecars")
    target_info = Path(plan["target_db"]).stat()
    require([target_info.st_dev, target_info.st_ino] == plan["target_identity"], "target DB identity changed")
    seen, destinations = set(), set()
    require(0 < len(plan["rollouts"]) <= 1024, "invalid rollout count")
    for entry in plan["rollouts"]:
        require(isinstance(entry, dict) and set(entry) == ENTRY_FIELDS, "unknown or missing rollout fields")
        physical = entry["rollout_id"]
        require(str(uuid.UUID(physical)) == physical and physical not in seen, "invalid/duplicate rollout ID")
        seen.add(physical)
        size, cutoff = entry["end_byte_offset"], entry["end_ordinal_exclusive"]
        require(type(size) is int and 0 < size <= 64 * 1024 * 1024 and type(cutoff) is int and 0 < cutoff < 2**63, "invalid bounds")
        source = path_in(root, entry["source"])
        require(source.parent == profile.parent / "source" and source.suffix == ".jsonl" and source.name.endswith(physical + ".jsonl"), "source outside fixed bounded-input layout")
        require(source.is_file() and not source.stat().st_mode & 0o222, "source must be read-only")
        data = source.read_bytes()
        require(len(data) == size and data.endswith(b"\n") and sha(data) == entry["prefix_sha256"], "source prefix bytes changed")
        records = [json.loads(line) for line in data.splitlines()]
        ordinals = [line["ordinal"] for line in records]
        require(ordinals == list(range(ordinals[0], cutoff)), "source ordinal bounds mismatch")
        destination = path_in(root, entry["destination"], missing=True)
        require(destination.parent == profile / "sessions" and destination not in destinations, "invalid/duplicate destination")
        require(destination.name == source.name, "destination name must match bound source")
        destinations.add(destination)
        require(destination != source, "source/destination alias")
        before = entry["destination_before"]
        require(before is None or (isinstance(before, dict) and set(before) == {"sha256", "bytes"}
                and type(before["bytes"]) is int and before["bytes"] >= size), "invalid destination preimage")
    return root, profile


def verify_files(plan):
    for entry in plan["rollouts"]:
        path = Path(entry["destination"])
        require(path.is_file(), "destination JSONL missing")
        expected = entry["destination_before"] or {"sha256": entry["prefix_sha256"], "bytes": entry["end_byte_offset"]}
        data = path.read_bytes()
        require(len(data) == expected["bytes"] and sha(data) == expected["sha256"], "destination bytes independently changed")
        require(sha(data[:entry["end_byte_offset"]]) == entry["prefix_sha256"], "destination prefix mismatch")


def wanted_registry(entry):
    return {"path": entry["destination"], "prefix_sha256": entry["prefix_sha256"], "end_byte_offset": entry["end_byte_offset"], "end_ordinal_exclusive": entry["end_ordinal_exclusive"]}


def verify_complete(plan):
    validate_plan(plan)
    verify_files(plan)
    require(all(value == "compatible" for value in inspect_projection(plan).values()), "projection incomplete")
    registry = read_registry(Path(plan["registry"]))["entries"]
    require(all(registry.get(entry["rollout_id"]) == wanted_registry(entry) for entry in plan["rollouts"]), "registry incomplete or changed")


def publish_sql(binary, plan, env, gate_handle, fault):
    request = {"synthetic_test_root": plan["root"], "staging_db": plan["staging_db"], "target_db": plan["target_db"],
               "rollouts": [{key: entry[key] for key in ("rollout_id", "end_byte_offset", "end_ordinal_exclusive")} for entry in plan["rollouts"]], "busy_timeout_ms": 100}
    if fault in ("sql:before_commit", "sql:orphan-paused", "sql:handled-timeout"):
        request["pause_before_commit_ms"] = 5000
    command = [str(binary)] if binary != "python-model" else [sys.executable, str(Path(__file__).resolve()), "--model-publisher"]
    # The child inherits this open description so killing its parent cannot
    # release the cooperative gate while an orphan publisher is still writing.
    child = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             env=env, pass_fds=(gate_handle.fileno(),))
    child.stdin.write(encoded(request)); child.stdin.close(); child.stdin = None
    hit("sql:spawned", fault)
    if fault in ("sql:before_commit", "sql:orphan-paused"):
        selector = selectors.DefaultSelector()
        selector.register(child.stderr, selectors.EVENT_READ)
        found, transcript = False, b""
        deadline = time.monotonic() + 10
        try:
            while time.monotonic() < deadline and child.poll() is None:
                if selector.select(0.1):
                    transcript += os.read(child.stderr.fileno(), 65536)
                    if b"CODEXM_PUBLISH_BEFORE_COMMIT" in transcript:
                        found = True
                        break
            require(found, "publisher did not reach pre-commit pause: " + transcript.decode(errors="replace"))
            # A real SIGKILL skips Python finally, leaving only the inherited
            # lock in the paused child. The harness must observe exclusion.
            hit("sql:orphan-paused", fault)
        finally:
            selector.close()
            if child.poll() is None:
                child.kill()
            child.wait(timeout=5)
        hit("sql:before_commit", fault)
    timed_out = False
    try:
        stdout, stderr = child.communicate(timeout=0.05 if fault == "sql:handled-timeout" else 20)
    except subprocess.TimeoutExpired:
        timed_out = True
        child.kill()
        stdout, stderr = child.communicate(timeout=5)
    hit("sql:returned", fault)
    # Nonzero, malformed, or absent stdout is an uncertain outcome, not rollback.
    acknowledged = False
    if fault == "sql:lost-ack":
        stdout = b""  # Deliberately lose a real successful publisher acknowledgment.
    if child.returncode == 0 and not timed_out:
        try:
            result = json.loads(stdout)
            acknowledged = result.get("status") == "published" and result.get("diagnostic_only") is True
        except (ValueError, AttributeError):
            pass
    actual = inspect_projection(plan)
    require(all(value == "compatible" for value in actual.values()), "publisher outcome incomplete; retry requires inspection: " + stderr.decode(errors="replace"))
    hit("sql:verified", fault)
    return {"acknowledged": acknowledged, "outcome": "verified-compatible", "returncode": child.returncode, "publisher_timed_out": timed_out}


def execute(plan, binary, env, fault=None):
    root, profile = validate_layout(plan)
    with gate(profile) as gate_handle:
        validate_plan(plan)
        operation_id = sha(encoded(plan))
        journal_root = profile / "codexm-recovery"
        path_in(root, journal_root, missing=True)
        journal_root.mkdir(exist_ok=True)
        sync_dir(profile)
        operation = journal_root / operation_id
        path_in(root, operation, missing=True)
        operation.mkdir(exist_ok=True)
        sync_dir(journal_root)
        hit("journal:directory", fault)
        intent = operation / "intent.json"
        path_in(root, intent, missing=True)
        if intent.exists():
            require(json.loads(intent.read_bytes()) == {"operation_id": operation_id, "plan": plan}, "journal intent differs from caller-bound operation")
        else:
            atomic_json(intent, {"operation_id": operation_id, "plan": plan}, "journal:intent", fault)
        sync_file(intent); sync_dir(operation)
        # Journal phases are hints only. Every restart inspects all actual resources.
        complete = path_in(root, operation / "complete.json", missing=True)
        if complete.exists():
            verify_complete(plan)
            require(json.loads(complete.read_bytes()) == {"operation_id": operation_id, "status": "verified", "diagnostic_only": True}, "wrong completion identity or fields")
            sync_file(complete); sync_dir(operation)
            return {"operation_id": operation_id, "status": "verified", "replayed": True}
        initial_projection = inspect_projection(plan)
        registry = read_registry(Path(plan["registry"]))["entries"]
        for entry in plan["rollouts"]:
            require(registry.get(entry["rollout_id"]) in (entry["registry_before"], wanted_registry(entry)), "selected registry independently changed")
        # Preflight every selected file before creating any of them.
        for entry in plan["rollouts"]:
            target = Path(entry["destination"])
            if target.exists():
                expected = entry["destination_before"] or {"sha256": entry["prefix_sha256"], "bytes": entry["end_byte_offset"]}
                require(sha(target.read_bytes()) == expected["sha256"] and target.stat().st_size == expected["bytes"], "destination conflict")
            else:
                require(entry["destination_before"] is None, "existing destination disappeared")
        for index, entry in enumerate(plan["rollouts"]):
            target = Path(entry["destination"])
            if not target.exists():
                temp = operation / f"payload-{index}.pending"
                path_in(root, temp, missing=True)
                with temp.open("wb") as output:
                    data = Path(entry["source"]).read_bytes()
                    midpoint = max(1, len(data) // 2)
                    output.write(data[:midpoint]); output.flush()
                    hit(f"file:{index}:partial", fault)
                    output.write(data[midpoint:]); output.flush()
                    hit(f"file:{index}:written", fault)
                    os.fsync(output.fileno())
                    hit(f"file:{index}:synced", fault)
                try:
                    rename_noreplace(temp, target)
                except FileExistsError:
                    raise Conflict("destination acquired an independent winner")
                hit(f"file:{index}:renamed", fault)
                sync_dir(target.parent); sync_dir(operation)
                hit(f"file:{index}:durable", fault)
        verify_files(plan)
        for entry in plan["rollouts"]:
            # Recovery may observe a rename that happened before directory fsync.
            sync_file(Path(entry["destination"]))
        sync_dir(profile / "sessions"); sync_dir(operation)
        atomic_json(operation / "files.json", {"operation_id": operation_id}, "journal:files", fault)
        sql_intent = path_in(root, operation / "sql-intent.json", missing=True)
        previous_intent = sql_intent.exists()
        if not previous_intent:
            atomic_json(sql_intent, {"operation_id": operation_id, "observed": initial_projection}, "journal:sql-intent", fault)
        else:
            old = json.loads(sql_intent.read_bytes())
            require(set(old) == {"operation_id", "observed"} and old["operation_id"] == operation_id
                    and set(old["observed"]) == set(initial_projection)
                    and set(old["observed"].values()) <= {"absent", "compatible"}, "invalid SQL intent")
        # Always invoke native publisher, including apparent committed outcomes:
        # its compiled schema pin and transactional prefix check remain mandatory.
        result = publish_sql(binary, plan, env, gate_handle, fault)
        atomic_json(operation / "sql-verified.json", {"operation_id": operation_id, **result}, "journal:sql-verified", fault)
        validate_plan(plan); verify_files(plan)
        registry_path = Path(plan["registry"])
        registry = read_registry(registry_path)  # Re-read, preserving unrelated changes.
        for entry in plan["rollouts"]:
            physical, desired = entry["rollout_id"], wanted_registry(entry)
            require(registry["entries"].get(physical) in (entry["registry_before"], desired), "registry compare-and-set conflict")
            registry["entries"][physical] = desired
        atomic_json(registry_path, registry, "registry", fault)
        verify_complete(plan)
        hit("verify:all", fault)
        atomic_json(operation / "complete.json", {"operation_id": operation_id, "status": "verified", "diagnostic_only": True}, "journal:complete", fault)
        return {"operation_id": operation_id, "status": "verified", "replayed": False,
                "reconciled_prior_uncertain_sql": previous_intent, "observed_before_retry": initial_projection, **result}


def startup_allowed(profile, trusted_plan):
    """Codexm-only synthetic launch gate; direct native startup ignores this."""
    root = next((path for path in profile.parents if path.name.startswith("codexm-native-acceptance-")), None)
    require(root is not None, "not a synthetic profile")
    checked_root(root)
    require(Path(trusted_plan["profile"]) == profile, "caller plan/profile mismatch")
    validate_layout(trusted_plan)
    with gate(profile):
        for operation in (profile / "codexm-recovery").glob("*"):
            path_in(root, operation)
            require(operation.is_dir() and not operation.is_symlink(), "invalid journal directory")
            for name in ("intent.json", "complete.json"):
                path_in(root, operation / name, missing=True)
            require((operation / "intent.json").is_file() and (operation / "complete.json").is_file(), "pending or incomplete recovery blocks controlled startup")
            intent = json.loads((operation / "intent.json").read_bytes())
            require(intent == {"operation_id": sha(encoded(trusted_plan)), "plan": trusted_plan}, "journal differs from caller-bound plan")
            require(sha(encoded(intent["plan"])) == operation.name == intent["operation_id"], "operation ID not bound to intent")
            require(json.loads((operation / "complete.json").read_bytes()) == {"operation_id": operation.name, "status": "verified", "diagnostic_only": True}, "completion identity mismatch")
            verify_complete(trusted_plan)
    return True


def make_fixture(work, name, source_db, manifest, existing_file=False):
    case = work / name
    case.mkdir()
    source, profile = case / "source", case / "profile"
    source.mkdir(); profile.mkdir(); (profile / "sessions").mkdir()
    stage = clone(source_db, case / "stage.sqlite")
    target = clone(source_db, profile / "thread_history_1.sqlite")
    registry = profile / "synthetic-registry.json"
    registry.write_bytes(encoded({"entries": {"unrelated": {"value": "retain me"}}}))
    def missing(db):
        add_unrelated(db, manifest["rollouts"][0]["rollout_id"])
        for entry in manifest["rollouts"]:
            remove_partition(db, entry["rollout_id"])
    mutate(target, missing)
    entries = []
    for index, entry in enumerate(manifest["rollouts"]):
        data = Path(entry["path"]).read_bytes()[:entry["end_byte_offset"]]
        require(sha(data) == entry["prefix_sha256"], "upstream fixture hash mismatch")
        path = source / Path(entry["path"]).name
        path.write_bytes(data); path.chmod(0o444)
        destination = profile / "sessions" / path.name
        before = None
        if existing_file and index == 0:
            # Full retained bytes bind the preimage, while the projection remains bounded.
            retained = data + b'{"synthetic_independent_tail":true}\n'
            destination.write_bytes(retained)
            before = {"sha256": sha(retained), "bytes": len(retained)}
        entries.append({key: entry[key] for key in ("rollout_id", "end_byte_offset", "end_ordinal_exclusive", "prefix_sha256")})
        entries[-1].update(source=str(path), destination=str(destination), destination_before=before, registry_before=None)
    info = target.stat()
    plan = {"version": VERSION, "root": str(work.parents[0]), "profile": str(profile), "staging_db": str(stage),
            "staging_sha256": sha(stage.read_bytes()), "target_db": str(target), "target_identity": [info.st_dev, info.st_ino],
            "initial_target_sha256": sha(target.read_bytes()), "initial_partitions": db_partitions(target, entries),
            "registry": str(registry), "initial_registry_sha256": sha(registry.read_bytes()), "rollouts": entries}
    # work is always root/recovery-probe.
    return plan


def invoke_adapter(plan, binary, env, fault=None):
    command = [sys.executable, str(Path(__file__).resolve()), "--execute", str(binary)]
    if fault:
        command += ["--fault", fault]
    return subprocess.run(command, input=encoded(plan), capture_output=True, env=env, timeout=40)


def expect_conflict(operation, description):
    try:
        operation()
    except (Conflict, ValueError, OSError):
        return
    raise AssertionError("accepted " + description)


def await_released(profile):
    # A killed adapter may leave its native child finishing under the inherited gate.
    deadline = time.monotonic() + 25
    while True:
        try:
            with gate(profile):
                return
        except Conflict:
            if time.monotonic() >= deadline:
                raise
            time.sleep(0.02)


def run_recovery_checks(publisher_binary, source_db, manifest, root, env, artifacts, checks):
    """Public test hook compatible with probe.prepare_projection's fixtures."""
    root = checked_root(Path(root))
    source_db = path_in(root, Path(source_db))
    for suffix in ("-wal", "-shm", "-journal"):
        require(not path_in(root, str(source_db) + suffix, missing=True).exists(), "source DB must be a closed synthetic snapshot")
    for entry in manifest["rollouts"]:
        source_path = path_in(root, entry["path"])
        require(source_path.is_file() and source_path.suffix == ".jsonl" and not source_path.stat().st_mode & 0o222, "original source must be a read-only synthetic JSONL file")
    require(publisher_binary == "python-model" or Path(publisher_binary).is_file(), "publisher binary missing")
    check_start = len(checks)
    work = root / "recovery-probe"
    work.mkdir()
    source_before = sha(source_db.read_bytes())
    input_before = {entry["path"]: sha(Path(entry["path"]).read_bytes()) for entry in manifest["rollouts"]}
    results = []
    points = ["journal:directory"]
    for phase in ("journal:intent", "journal:files", "journal:sql-intent", "journal:sql-verified", "registry", "journal:complete"):
        points += [phase + ":" + transition for transition in ("partial", "written", "synced", "renamed", "durable")]
    for index in range(len(manifest["rollouts"])):
        points += [f"file:{index}:" + transition for transition in ("partial", "written", "synced", "renamed", "durable")]
    points += ["sql:spawned", "sql:before_commit", "sql:orphan-paused", "sql:returned", "sql:verified", "verify:all"]
    for index, point in enumerate(points):
        plan = make_fixture(work, f"kill-{index:02}", source_db, manifest)
        profile = Path(plan["profile"])
        target_before = state(Path(plan["target_db"]))
        killed = invoke_adapter(plan, publisher_binary, env, point)
        require(killed.returncode == -signal.SIGKILL, f"fault not reached {point}: {killed.stderr.decode()}")
        if point == "sql:orphan-paused":
            blocked = invoke_adapter(plan, publisher_binary, env)
            require(blocked.returncode != 0 and b"cooperative gate busy" in blocked.stderr, "orphan publisher did not retain gate")
            expect_conflict(lambda: startup_allowed(profile, plan), "startup while orphan publisher remains active")
        await_released(profile)
        complete = profile / "codexm-recovery" / sha(encoded(plan)) / "complete.json"
        if not complete.exists():
            expect_conflict(lambda: startup_allowed(profile, plan), "startup before verified completion")
        if point == "sql:before_commit":
            require(state(Path(plan["target_db"])) == target_before, "killed SQL did not recover to original rows")
        # Independent unrelated edits after a crash must survive the entire replay.
        registry_path = Path(plan["registry"])
        registry = read_registry(registry_path)
        registry["entries"]["independent-after-kill"] = {"point": point}
        registry_path.write_bytes(encoded(registry))
        unrelated = profile / "sessions" / "independent.txt"
        unrelated.write_bytes(b"preserve independent file\n")
        mutate(Path(plan["target_db"]), lambda db: db.execute("UPDATE thread_items SET item_json=? WHERE thread_id=?", (json.dumps({"independent_after_kill": point}), SENTINEL)))
        with closing(db_connection(Path(plan["target_db"]))) as db:
            sentinel_before = [rows(db, table, SENTINEL) for table in TABLES]
        recovered = invoke_adapter(plan, publisher_binary, env)
        require(recovered.returncode == 0, f"recovery failed {point}: {recovered.stderr.decode()}")
        if point == "sql:returned":
            observed = json.loads(recovered.stdout)
            require(observed["reconciled_prior_uncertain_sql"] and set(observed["observed_before_retry"].values()) == {"compatible"}, "lost SQL acknowledgment not reconciled")
        require(startup_allowed(profile, plan), "verified startup blocked")
        logical = state(Path(plan["target_db"]))
        again = invoke_adapter(plan, publisher_binary, env)
        require(again.returncode == 0 and state(Path(plan["target_db"])) == logical, "replay not idempotent")
        require(read_registry(registry_path)["entries"]["independent-after-kill"] == {"point": point}, "lost independent registry edit")
        require(unrelated.read_bytes() == b"preserve independent file\n", "lost independent file")
        with closing(db_connection(Path(plan["target_db"]))) as db:
            require([rows(db, table, SENTINEL) for table in TABLES] == sentinel_before, "lost unrelated DB partition")
        results.append({"fault": point, "killed": True, "recovered": True, "idempotent": True})
    checks.append(f"recovery survives SIGKILL at all {len(points)} journal/file/SQL/registry transitions and preserves unrelated state")

    lost = make_fixture(work, "lost-acknowledgment", source_db, manifest)
    response = invoke_adapter(lost, publisher_binary, env, "sql:lost-ack")
    require(response.returncode == 0 and json.loads(response.stdout)["acknowledged"] is False, "lost acknowledgment was treated as failure or assumed success")
    require(startup_allowed(Path(lost["profile"]), lost), "lost acknowledgment not reconciled from actual resources")
    checks.append("missing publisher acknowledgment is reconciled by actual projection verification, never assumed rollback")

    timed = make_fixture(work, "handled-publisher-timeout", source_db, manifest)
    timed_before = state(Path(timed["target_db"]))
    response = invoke_adapter(timed, publisher_binary, env, "sql:handled-timeout")
    require(response.returncode != 0, "incomplete timeout was reported as success")
    with gate(Path(timed["profile"])):
        pass  # Handled timeout must reap its child, not leave an orphan holding the gate.
    require(state(Path(timed["target_db"])) == timed_before, "timed-out precommit publisher changed logical state")
    expect_conflict(lambda: startup_allowed(Path(timed["profile"]), timed), "startup after incomplete publisher timeout")
    require(invoke_adapter(timed, publisher_binary, env).returncode == 0, "replay after handled timeout failed")
    checks.append("handled publisher timeout kills/reaps child, leaves incomplete recovery blocked, and retries safely")

    retained = make_fixture(work, "retain-ahead-file", source_db, manifest, existing_file=True)
    full_before = Path(retained["rollouts"][0]["destination"]).read_bytes()
    require(invoke_adapter(retained, publisher_binary, env).returncode == 0, "retained file rejected")
    require(Path(retained["rollouts"][0]["destination"]).read_bytes() == full_before, "ahead file truncated")
    checks.append("recovery preserves an existing matching-prefix JSONL file including its separately bound tail")

    # Pre- and post-SQL conflicts are preserved, never rolled back with old snapshots.
    for kind in ("source", "destination", "registry", "projection", "stage", "intent", "target-identity"):
        plan = make_fixture(work, "conflict-" + kind, source_db, manifest)
        require(invoke_adapter(plan, publisher_binary, env, "sql:returned").returncode == -signal.SIGKILL, "setup fault not reached")
        entry = plan["rollouts"][0]
        if kind == "source":
            path = Path(entry["source"]); path.chmod(0o644); path.write_bytes(path.read_bytes() + b"changed\n"); path.chmod(0o444)
        elif kind == "destination":
            Path(entry["destination"]).write_bytes(b"independent replacement\n")
        elif kind == "registry":
            path = Path(plan["registry"]); value = read_registry(path); value["entries"][entry["rollout_id"]] = {"independent": True}; path.write_bytes(encoded(value))
        elif kind == "projection":
            mutate(Path(plan["target_db"]), lambda db: db.execute("UPDATE thread_items SET item_json=? WHERE thread_id=?", ('{"independent":true}', entry["rollout_id"])))
        elif kind == "stage":
            mutate(Path(plan["staging_db"]), lambda db: db.execute("UPDATE thread_items SET item_json=?", ('{"changed":true}',)))
        elif kind == "intent":
            path = Path(plan["profile"]) / "codexm-recovery" / sha(encoded(plan)) / "intent.json"
            value = json.loads(path.read_bytes()); value["plan"]["rollouts"][0]["end_ordinal_exclusive"] += 1; path.write_bytes(encoded(value))
        else:
            path = Path(plan["target_db"]); other = path.with_name("replacement.sqlite"); clone(path, other); other.replace(path)
        before_db = state(Path(plan["target_db"]))
        before_files = {p: p.read_bytes() for p in Path(plan["profile"]).joinpath("sessions").iterdir()}
        before_registry = Path(plan["registry"]).read_bytes()
        rejected = invoke_adapter(plan, publisher_binary, env)
        require(rejected.returncode != 0, "conflict silently accepted: " + kind)
        require(state(Path(plan["target_db"])) == before_db and Path(plan["registry"]).read_bytes() == before_registry, "conflict changed DB/registry")
        require(all(path.read_bytes() == data for path, data in before_files.items()), "conflict changed files")
        expect_conflict(lambda: startup_allowed(Path(plan["profile"]), plan), "startup after conflict")
        results.append({"conflict": kind, "preserved": True})
    checks.append("recovery fails closed and preserves source/file/registry/DB/staging/intent/identity conflicts")

    # Changed bounds/path/preimage must produce another identity, not resume old work.
    identity_plan = make_fixture(work, "identity", source_db, manifest)
    operation_id = sha(encoded(identity_plan))
    for key, value in (("end_ordinal_exclusive", identity_plan["rollouts"][0]["end_ordinal_exclusive"] + 1), ("prefix_sha256", "0" * 64), ("destination", str(Path(identity_plan["profile"]) / "sessions" / "other.jsonl"))):
        changed = copy.deepcopy(identity_plan); changed["rollouts"][0][key] = value
        require(sha(encoded(changed)) != operation_id, "operation identity omitted " + key)
    with gate(Path(identity_plan["profile"])):
        require(invoke_adapter(identity_plan, publisher_binary, env).returncode != 0, "second adapter bypassed gate")
    checks.append("operation identity binds exact source/destination plan and bounds; cooperative gate excludes competing adapters")

    guard = make_fixture(work, "untrusted-plan", source_db, manifest)
    guard_before = state(Path(guard["target_db"]))
    bad_plans = []
    changed = copy.deepcopy(guard); changed["unknown"] = True; bad_plans.append(changed)
    changed = copy.deepcopy(guard); changed["rollouts"][0]["unknown"] = True; bad_plans.append(changed)
    changed = copy.deepcopy(guard); changed["registry"] = str(root / "other-registry.json"); bad_plans.append(changed)
    changed = copy.deepcopy(guard); changed["rollouts"][0]["destination"] = str(Path(guard["profile"]) / "sessions" / ".." / "escape.jsonl"); bad_plans.append(changed)
    changed = copy.deepcopy(guard); changed["rollouts"][0]["source"] = manifest["rollouts"][0]["path"]; bad_plans.append(changed)
    changed = copy.deepcopy(guard); changed["profile"] = str(root); bad_plans.append(changed)
    for changed in bad_plans:
        require(invoke_adapter(changed, publisher_binary, env).returncode != 0, "untrusted plan accepted")
        require(state(Path(guard["target_db"])) == guard_before, "untrusted plan mutated DB")
    protected = root / "protected-synthetic.txt"; protected.write_bytes(b"not a recovery destination")
    link = Path(guard["rollouts"][0]["destination"]); link.symlink_to(protected)
    require(invoke_adapter(guard, publisher_binary, env).returncode != 0 and protected.read_bytes() == b"not a recovery destination", "symlink destination followed")
    checks.append("untrusted plan rejects unknown fields, path escapes, sibling resources, and symlinks without target mutation")

    forged = make_fixture(work, "forged-completion", source_db, manifest)
    require(invoke_adapter(forged, publisher_binary, env, "journal:intent:durable").returncode == -signal.SIGKILL, "forgery setup fault not reached")
    operation = Path(forged["profile"]) / "codexm-recovery" / sha(encoded(forged))
    (operation / "complete.json").write_bytes(encoded({"operation_id": operation.name, "status": "verified", "diagnostic_only": True}))
    expect_conflict(lambda: startup_allowed(Path(forged["profile"]), forged), "forged completion with absent resources")
    require(invoke_adapter(forged, publisher_binary, env).returncode != 0, "forged completion trusted")
    checks.append("completion marker alone never authorizes success or controlled startup")
    require(sha(source_db.read_bytes()) == source_before, "original projection fixture changed")
    require(all(sha(Path(path).read_bytes()) == value for path, value in input_before.items()), "original source fixture changed")
    report = {"mode": "python-transaction-model" if publisher_binary == "python-model" else "native-publisher", "diagnostic_only": True,
              "native_writer_exclusion": False, "filesystem_database_atomicity": False,
              "fault_count": len(points), "cases": results, "checks": checks[check_start:]}
    (Path(artifacts) / "recovery-cases.json").write_text(json.dumps(report, indent=2) + "\n")
    return report


def model_publisher():
    """Local test double only. This is not a native schema/projector substitute."""
    request = json.loads(sys.stdin.buffer.read())
    with closing(sqlite3.connect(request["target_db"])) as db, closing(sqlite3.connect(Path(request["staging_db"]).as_uri() + "?mode=ro&immutable=1", uri=True)) as stage:
        db.execute("PRAGMA synchronous=FULL")
        db.execute("BEGIN IMMEDIATE")
        for entry in request["rollouts"]:
            physical = entry["rollout_id"]
            if not rows(db, TABLES[-1], physical):
                for table in TABLES:
                    for row in rows(stage, table, physical):
                        db.execute(f'INSERT INTO "{table}" VALUES ({",".join("?" for _ in row)})', row)
        print("CODEXM_PUBLISH_BEFORE_COMMIT", file=sys.stderr, flush=True)
        time.sleep(request.get("pause_before_commit_ms", 0) / 1000)
        db.commit()
    print(json.dumps({"status": "published", "diagnostic_only": True}))


def self_test():
    with tempfile.TemporaryDirectory(prefix="codexm-native-acceptance-recovery-") as directory:
        root = Path(directory); (root / MARKER).write_bytes(MARKER_BYTES)
        stage = root / "source.sqlite"
        manifest = {"rollouts": []}
        with closing(sqlite3.connect(stage)) as db:
            db.executescript("""
            CREATE TABLE _sqlx_migrations(version INTEGER PRIMARY KEY,description TEXT,success INTEGER,checksum BLOB);
            INSERT INTO _sqlx_migrations VALUES(1,'synthetic model only',1,x'01');
            CREATE TABLE thread_turns(thread_id TEXT,turn_id TEXT,rollout_ordinal INTEGER,status TEXT,PRIMARY KEY(thread_id,turn_id));
            CREATE TABLE thread_items(thread_id TEXT,item_id TEXT,rollout_ordinal INTEGER,item_json TEXT,updated_at_ordinal INTEGER,PRIMARY KEY(thread_id,item_id));
            CREATE TABLE thread_realtime_items(thread_id TEXT,item_id TEXT,rollout_ordinal INTEGER,item_json TEXT,PRIMARY KEY(thread_id,item_id));
            CREATE TABLE thread_history_projection_state(thread_id TEXT PRIMARY KEY,next_rollout_byte_offset INTEGER,next_rollout_ordinal INTEGER);
            """)
            for index in range(2):
                physical = f"00000000-0000-4000-8000-{index+1:012}"
                path = root / f"rollout-synthetic-{physical}.jsonl"
                data = b'{"ordinal":0,"type":"session_meta"}\n{"ordinal":1,"type":"event_msg"}\n'
                path.write_bytes(data); path.chmod(0o444)
                manifest["rollouts"].append({"rollout_id": physical, "path": str(path), "end_byte_offset": len(data), "end_ordinal_exclusive": 2, "prefix_sha256": sha(data)})
                db.execute("INSERT INTO thread_turns VALUES(?,?,1,'completed')", (physical, "turn"))
                db.execute("INSERT INTO thread_items VALUES(?,?,1,'{}',1)", (physical, "item"))
                db.execute("INSERT INTO thread_history_projection_state VALUES(?,?,2)", (physical, len(data)))
            db.commit()
        checks = []
        report = run_recovery_checks("python-model", stage, manifest, root, os.environ.copy(), root, checks)
        print(json.dumps({"mode": report["mode"], "fault_count": report["fault_count"], "checks": checks}, indent=2))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--self-test", action="store_true")
    parser.add_argument("--model-publisher", action="store_true")
    parser.add_argument("--execute")
    parser.add_argument("--fault")
    args = parser.parse_args()
    if args.self_test:
        self_test()
    elif args.model_publisher:
        model_publisher()
    elif args.execute:
        print(json.dumps(execute(json.loads(sys.stdin.buffer.read()), args.execute, os.environ.copy(), args.fault)))
    else:
        parser.error("choose --self-test or --execute")


if __name__ == "__main__":
    main()
