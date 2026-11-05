"""Measure native lock boundaries using only disposable synthetic profiles.

Success means the expected boundaries were reproduced, including a known bypass.
It does not mean that native writer guards make history repair safe with clients.
"""
from contextlib import closing
import hashlib
import json
import os
from pathlib import Path
import queue
import re
import shutil
import sqlite3
import subprocess
import threading
import tomllib

PIN = "6ff1e1ca3b30fc8b914720681fcf5fe62e95f856"
MARKER = "codexm synthetic native lock diagnostic\n"
UUID = r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"
ROLLOUT = re.compile(rf"rollout-.+-({UUID})(?:_({UUID}))?\.jsonl$")
LOW_OWNER = "00000000-0000-4000-8000-000000000001"
STALE_OWNER = "00000000-0000-4000-8000-000000000002"
TABLES = ("thread_items", "thread_realtime_items", "thread_turns", "thread_history_projection_state")


def inventory(root):
    files = {}
    for path in sorted(root.rglob("*")):
        assert not path.is_symlink(), f"Unexpected synthetic symlink: {path}"
        if path.is_file():
            stat = path.stat()
            files[str(path.relative_to(root))] = (
                hashlib.sha256(path.read_bytes()).hexdigest(), stat.st_mode,
                stat.st_size, stat.st_mtime_ns, stat.st_ino, stat.st_dev,
            )
    return files


def rollout_inventory(home):
    return {str(path.relative_to(home)): hashlib.sha256(path.read_bytes()).hexdigest()
            for path in sorted((home / "sessions").rglob("*.jsonl"))}


def assert_original_rollouts(home, before):
    after = rollout_inventory(home)
    assert all(after.get(path) == digest for path, digest in before.items()), \
        "A copied source rollout was changed during the fork probe"


def copied_lineage(home, child_name):
    by_id = {}
    for path in (home / "sessions").rglob("*.jsonl"):
        match = ROLLOUT.fullmatch(path.name)
        assert match, path.name
        physical = match.group(2) or match.group(1)
        assert physical not in by_id, "Duplicate physical rollout in synthetic fixture"
        records = [json.loads(line) for line in path.read_bytes().splitlines()]
        assert records[0]["type"] == "session_meta"
        assert records[0]["payload"]["id"] == match.group(1)
        by_id[physical] = {"path": path, "physical": physical,
                           "owner": match.group(1), "records": records}
    leaf = next(value for value in by_id.values() if value["path"].name == child_name)
    lineage, seen, current, cutoff = [], set(), leaf, None
    while True:
        assert current["physical"] not in seen, "Cyclic synthetic fixture"
        seen.add(current["physical"])
        current = dict(current, visible_end=cutoff)
        lineage.append(current)
        base = current["records"][0]["payload"].get("history_base")
        if base is None:
            break
        current, cutoff = by_id[base["thread_id"]], base["end_ordinal_exclusive"]
    return list(reversed(lineage))


def partition(path, physical):
    assert path.is_file(), "Native history database was not created"
    # This is an existing mutable synthetic DB. Never use immutable=1 here.
    with closing(sqlite3.connect(path.as_uri() + "?mode=ro", uri=True)) as connection:
        return {table: sorted(connection.execute(
            f'SELECT * FROM "{table}" WHERE thread_id=?', (physical,)).fetchall(), key=repr)
                for table in TABLES}


def remove_synthetic_partition(path, physical):
    with closing(sqlite3.connect(path)) as connection:
        for table in TABLES:
            connection.execute(f'DELETE FROM "{table}" WHERE thread_id=?', (physical,))
        connection.commit()
        assert connection.execute("PRAGMA integrity_check").fetchone() == ("ok",)


class Probe:
    """Own one helper process and clean it up on every success/failure path."""
    def __init__(self, binary, request, env, cwd, artifacts, label):
        self.incoming = queue.Queue()
        self.events = []
        self.stdout_log = (artifacts / f"native-lock-{label}.stdout.jsonl").open("w")
        self.stderr_log = (artifacts / f"native-lock-{label}.stderr.log").open("w")
        self.process = None
        self.reader = None
        try:
            self.process = subprocess.Popen(
                [str(binary)], stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                stderr=self.stderr_log, text=True, bufsize=1, env=env, cwd=cwd,
            )
            self.reader = threading.Thread(target=self._read, daemon=True)
            self.reader.start()
            self.process.stdin.write(json.dumps(request) + "\n")
            self.process.stdin.flush()
            self.first = self.event()
            assert self.first["diagnostic_only"] is True
            assert self.first["native_global_exclusion"] is False
            assert self.first["native_stale_cleanup_enabled"] is True
            assert self.first["upstream_commit"] == PIN
        except BaseException:
            self.close()
            raise

    def _read(self):
        try:
            for line in self.process.stdout:
                self.stdout_log.write(line)
                self.stdout_log.flush()
                self.incoming.put(json.loads(line))
        except BaseException as error:
            self.incoming.put(error)
        finally:
            self.incoming.put(EOFError("native lock helper stdout closed"))

    def event(self, timeout=40):
        try:
            event = self.incoming.get(timeout=timeout)
        except queue.Empty as error:
            raise TimeoutError("native lock helper did not respond") from error
        if isinstance(event, BaseException):
            raise event
        self.events.append(event)
        return event

    def acquired(self):
        assert self.first["status"] == "acquired", self.first
        assert self.process.poll() is None, "Native guard exited before observation"

    def blocked(self, owner):
        assert self.first["status"] == "blocked", self.first
        assert self.first["native_error_kind"] == "WouldBlock", self.first
        assert self.first["owner_id"] == owner, self.first
        assert self.process.wait(timeout=10) == 3

    def release(self):
        self.acquired()
        self.process.stdin.write("release\n")
        self.process.stdin.flush()
        assert self.event()["status"] == "released"
        assert self.process.wait(timeout=40) == 0

    def close(self):
        if self.process is not None:
            if self.process.poll() is None:
                try:
                    if not self.process.stdin.closed:
                        self.process.stdin.write("release\n")
                        self.process.stdin.flush()
                        self.process.stdin.close()
                    self.process.wait(timeout=40)
                except (BrokenPipeError, OSError, subprocess.TimeoutExpired):
                    if self.process.poll() is None:
                        self.process.kill()
                    self.process.wait(timeout=10)
            if self.reader is not None:
                self.reader.join(timeout=5)
            for stream in (self.process.stdin, self.process.stdout):
                if stream is not None and not stream.closed:
                    stream.close()
        self.stdout_log.close()
        self.stderr_log.close()

    def __enter__(self):
        return self

    def __exit__(self, *_args):
        self.close()


def native_error(app, method, params):
    """Get the actual RPC error, not a timeout or any arbitrary Python failure."""
    app.next_id += 1
    request_id = app.next_id
    app.send({"id": request_id, "method": method, "params": params})
    result = app.wait(lambda message: message.get("id") == request_id)
    assert "error" in result and "result" not in result, result
    return result["error"]


def run_native_lock_checks(binary, codex, app_server_class, source_home, child_id,
                           child_name, root, env, cwd, artifacts, checks):
    """Call before original second-resume, while the original source is closed."""
    root, source_home, cwd = (Path(value).resolve(strict=True) for value in (root, source_home, cwd))
    assert root.name.startswith("codexm-native-acceptance-")
    assert source_home.is_relative_to(root) and cwd.is_relative_to(root)
    original = inventory(source_home)
    marker = root / "CODEXM_SYNTHETIC_NATIVE_LOCK_TEST"
    assert not marker.exists()
    marker.write_text(MARKER)
    work = root / "native-lock-probe"
    work.mkdir(mode=0o700)
    config_text = (source_home / "config.toml").read_text()
    config = tomllib.loads(config_text)
    assert set(config) == {"model", "model_provider", "approval_policy", "sandbox_mode", "model_providers"}
    assert config["model_provider"] == "mock_provider" and config["model"] == "mock-model"
    assert config["approval_policy"] == "never" and config["sandbox_mode"] == "read-only"
    assert set(config["model_providers"]) == {"mock_provider"}
    provider = config["model_providers"]["mock_provider"]
    assert set(provider) == {"name", "base_url", "wire_api", "requires_openai_auth",
                             "request_max_retries", "stream_max_retries"}
    assert re.fullmatch(r"http://127\.0\.0\.1:[0-9]+/v1", provider["base_url"])
    assert provider["requires_openai_auth"] is False and provider["wire_api"] == "responses"
    safe_env = {key: env[key] for key in ("PATH", "LANG", "HOME", "TMPDIR", "XDG_RUNTIME_DIR")}
    for key in ("HOME", "TMPDIR", "XDG_RUNTIME_DIR"):
        assert Path(safe_env[key]).resolve(strict=True).is_relative_to(root)
    results = {"status": "failed", "diagnostic_only": True, "upstream_commit": PIN,
               "native_global_exclusion": False, "production_acceptance": "not assessed",
               "stale_lock_cleanup_observed": False,
               "fork_ancestor_projection_bypass_observed": False, "cases": []}

    def record(name, classification="boundary_verified", **evidence):
        results["cases"].append({"case": name, "classification": classification, **evidence})
        prefix = "native lock limitation observed: " if classification == "known_limitation_observed" else "native lock boundary: "
        checks.append(prefix + name)

    def fresh(name):
        home = work / name
        home.mkdir(mode=0o700)
        shutil.copytree(source_home / "sessions", home / "sessions")
        (home / "config.toml").write_text(config_text)
        assert {entry.name for entry in home.iterdir()} == {"sessions", "config.toml"}
        paths = list((home / "sessions").rglob(child_name))
        assert len(paths) == 1
        return home, dict(safe_env, CODEX_HOME=str(home)), paths[0]

    def request(home, owners, **extra):
        data = dict(synthetic_test_root=str(root), codex_home=str(home),
                    owner_ids=owners, max_hold_ms=120_000)
        data.update(extra)
        return data

    def probe(home, owners, label, **extra):
        return Probe(binary, request(home, owners, **extra),
                     dict(safe_env, CODEX_HOME=str(home)), cwd, artifacts, label)

    def resume(app, path):
        response = app.request("thread/resume", {"threadId": child_id, "path": str(path),
                                                 "excludeTurns": True})
        assert response["thread"]["id"] == child_id
        assert Path(response["thread"]["path"]).resolve() == path.resolve()

    try:
        home, isolated, _ = fresh("preflight-rejections")
        before = inventory(home)
        invalid = [
            ("duplicate owner", request(home, [child_id, child_id])),
            ("unknown field", request(home, [child_id], unexpected=True)),
            ("unbounded hold", request(home, [child_id], max_hold_ms=120_001)),
            ("mismatched home", request(home, [child_id], codex_home=str(work))),
        ]
        for label, data in invalid:
            rejected = subprocess.run([str(binary)], input=json.dumps(data) + "\n",
                                      text=True, capture_output=True, env=isolated, cwd=cwd, timeout=40)
            assert rejected.returncode == 1 and not rejected.stdout, (label, rejected)
            assert inventory(home) == before, f"Rejected {label} changed the synthetic home"
            record(f"preflight rejects {label} without acquiring a lock")
        try:
            marker.write_text("wrong synthetic marker\n")
            rejected = subprocess.run([str(binary)], input=json.dumps(request(home, [child_id])) + "\n",
                                      text=True, capture_output=True, env=isolated, cwd=cwd, timeout=40)
            assert rejected.returncode == 1 and not rejected.stdout
            assert inventory(home) == before
        finally:
            marker.write_text(MARKER)
        record("preflight requires the exact explicit synthetic marker")

        home, isolated, path = fresh("active-writer")
        with app_server_class(codex, isolated, cwd, artifacts, "native-lock-active-writer") as app:
            resume(app, path)
            with probe(home, [child_id], "active-writer-contender") as contender:
                contender.blocked(child_id)
            record("live native resume excludes diagnostic owner acquisition")
            assert LOW_OWNER < child_id
            with probe(home, [child_id, LOW_OWNER], "partial-contention") as contender:
                contender.blocked(child_id)
                assert contender.first["released_partial_count"] == 1
            assert not (home / "thread-writer-locks" / f"{LOW_OWNER}.lock").exists()
            with probe(home, [LOW_OWNER], "partial-reacquire") as guard:
                guard.acquired()
                guard.release()
            record("sorted multi-owner contention releases earlier native guards")

        home, isolated, path = fresh("guard-blocks-resume")
        with app_server_class(codex, isolated, cwd, artifacts, "native-lock-warm-resume") as app:
            resume(app, path)
        before = rollout_inventory(home)
        with probe(home, [child_id], "guard-before-resume") as guard:
            guard.acquired()
            with app_server_class(codex, isolated, cwd, artifacts, "native-lock-blocked-resume") as app:
                error = native_error(app, "thread/resume", {"threadId": child_id,
                                      "path": str(path), "excludeTurns": True})
                encoded = json.dumps(error)
                assert child_id in encoded and "already has an active writer" in encoded, error
                guard.acquired()
            assert rollout_inventory(home) == before
            guard.release()
        record("held logical native guard excludes resume without changing rollout bytes",
               rpc_error=error)
        with app_server_class(codex, isolated, cwd, artifacts, "native-lock-resume-after-release") as app:
            resume(app, path)
            with probe(home, [child_id], "released-native-owner") as contender:
                contender.blocked(child_id)
        record("native resume succeeds after diagnostic guard release")

        home, isolated, path = fresh("wrong-physical-id")
        lineage = copied_lineage(home, child_name)
        leaf = lineage[-1]
        assert leaf["owner"] == child_id and leaf["physical"] != child_id
        with probe(home, [leaf["physical"]], "physical-not-owner") as guard:
            guard.acquired()
            with app_server_class(codex, isolated, cwd, artifacts, "native-lock-physical-resume") as app:
                resume(app, path)
                with probe(home, [child_id], "physical-native-owner") as contender:
                    contender.blocked(child_id)
                guard.acquired()
            guard.release()
        record("physical rollout lock does not substitute for logical owner after revert",
               "known_limitation_observed", logical_owner=child_id, physical_rollout=leaf["physical"])

        home, _, _ = fresh("stale-cleanup")
        directory = home / "thread-writer-locks"
        directory.mkdir()
        stale = directory / f"{STALE_OWNER}.lock"
        stale.write_text("Synthetic stale lock sentinel. Native cleanup is expected.\n")
        unrelated = directory / "not-a-thread.txt"
        unrelated.write_text("Synthetic non-lock sentinel. Must remain.\n")
        with probe(home, [child_id], "stale-cleanup") as guard:
            guard.acquired()
            assert not stale.exists(), "Pinned native stale cleanup behavior changed"
            assert unrelated.read_text() == "Synthetic non-lock sentinel. Must remain.\n"
            guard.release()
        results["stale_lock_cleanup_observed"] = True
        record("public acquire removes an unrelated stale UUID lock file",
               "known_limitation_observed")

        home, _, _ = fresh("bounded-lifetime")
        data = request(home, [child_id])
        data["max_hold_ms"] = 1000
        with Probe(binary, data, dict(safe_env, CODEX_HOME=str(home)), cwd, artifacts, "expiry") as guard:
            guard.acquired()
            assert guard.event(timeout=10)["status"] == "expired"
            assert guard.process.wait(timeout=40) == 4
        with probe(home, [child_id], "expiry-reacquire") as guard:
            guard.acquired()
            guard.release()
        record("bounded diagnostic hold expires and releases its native guard")

        home, _, _ = fresh("abrupt-exit")
        with probe(home, [child_id], "abrupt-exit") as guard:
            guard.acquired()
            guard.process.kill()  # This test-owned helper only; no native client is killed.
            assert guard.process.wait(timeout=10) != 0
        assert (home / "thread-writer-locks" / f"{child_id}.lock").exists()
        with probe(home, [child_id], "abrupt-exit-reacquire") as guard:
            guard.acquired()
            guard.release()
        record("abrupt helper exit permits native reacquisition without harness lock deletion")

        # Bootstrap only this copy. Native metadata paths now point into this home.
        # Forking through an ancestor projects the lineage, unlike ordinary resume.
        home, isolated, path = fresh("fork-bypass")
        lineage = copied_lineage(home, child_name)
        assert len(lineage) >= 2
        ancestor = lineage[0]
        terminal = [record["payload"]["turn_id"] for record in ancestor["records"]
                    if record.get("type") == "event_msg"
                    and record.get("payload", {}).get("type") in ("task_complete", "turn_complete")
                    and (ancestor["visible_end"] is None or record["ordinal"] < ancestor["visible_end"])]
        assert terminal, "Synthetic ancestor lacks a visible completed turn"
        params = {"threadId": child_id, "path": str(path), "lastTurnId": terminal[0],
                  "cwd": str(cwd), "excludeTurns": True}
        before = rollout_inventory(home)
        with app_server_class(codex, isolated, cwd, artifacts, "native-lock-bootstrap-fork") as app:
            bootstrap = app.request("thread/fork", params)["thread"]
            assert bootstrap["id"] not in {entry["owner"] for entry in lineage}
        assert_original_rollouts(home, before)
        db = home / "thread_history_1.sqlite"
        expected = partition(db, ancestor["physical"])
        assert expected["thread_history_projection_state"] and expected["thread_turns"] and expected["thread_items"]
        remove_synthetic_partition(db, ancestor["physical"])
        assert not any(partition(db, ancestor["physical"]).values())
        owners = sorted({entry["owner"] for entry in lineage})
        with probe(home, owners, "all-lineage-owners") as guard:
            guard.acquired()
            assert guard.first["owner_ids"] == owners
            with app_server_class(codex, isolated, cwd, artifacts, "native-lock-fork-bypass") as app:
                # Fail rather than attributing unrelated startup writes to fork.
                assert not any(partition(db, ancestor["physical"]).values()), \
                    "Ancestor was reprojected before the fork request"
                forked = app.request("thread/fork", params)["thread"]
                assert forked["id"] not in owners
                assert partition(db, ancestor["physical"]) == expected, \
                    "Fork did not recreate the exact missing ancestor projection"
                guard.acquired()
                for index, owner in enumerate(owners):
                    with probe(home, [owner], f"fork-owner-still-held-{index}") as contender:
                        contender.blocked(owner)
                assert_original_rollouts(home, before)
            guard.acquired()
            guard.release()
        results["fork_ancestor_projection_bypass_observed"] = True
        record("native fork rebuilds an ancestor projection while all lineage owner guards are held",
               "known_limitation_observed", logical_owners=owners,
               ancestor_physical_rollout=ancestor["physical"],
               restored_rows={table: len(rows) for table, rows in expected.items()})
        assert inventory(source_home) == original, "Native lock probes changed original fixture"
        record("all native lock probes preserve original fixture inventory")
        results["status"] = "passed"
        return results
    except BaseException as error:
        results["error"] = str(error)
        raise
    finally:
        results["original_fixture_unchanged"] = inventory(source_home) == original
        (artifacts / "native-lock-summary.json").write_text(json.dumps(results, indent=2) + "\n")
