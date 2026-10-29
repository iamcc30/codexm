#!/usr/bin/env python3
"""Diagnostic ONLY: test native ephemeral cutoff fork as a projection rebuild.

This does not change codexm, relax native acceptance, or establish a supported
production repair. All data is synthetic and isolated. Native execution belongs
on the explicitly authorized CI runner, never in a blocked local executor.

Native supports .jsonl.zst, not .jsonl.gz. The compressed case uses libzstd only
to encode one required synthetic ancestor, then observes native side effects.
"""
import argparse
import ctypes
import ctypes.util
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import sqlite3
import subprocess
import tempfile
import traceback

# Load helpers without invoking the acceptance script's main function.
_spec = importlib.util.spec_from_file_location("codexm_native_acceptance_helpers",
                                               Path(__file__).with_name("native-history-acceptance.py"))
_helpers = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_helpers)
AppServer = _helpers.AppServer
MockResponses = _helpers.MockResponses
GateResults = _helpers.GateResults
assert_markers = _helpers.assert_markers
MARKER_RE = re.compile(r"codexm-projection-[a-z-]+-c836b2")


def snapshot(home):
    """Byte snapshots exclude only derived SQLite/log/lock data, never rollouts."""
    paths = [home / name for name in ("session_index.jsonl", "config.toml", "auth.json")]
    for collection in ("sessions", "archived_sessions"):
        directory = home / collection
        if directory.exists():
            paths.extend(path for path in directory.rglob("*") if path.is_file())
    result = {}
    for path in paths:
        if path.exists():
            assert not path.is_symlink(), f"Unexpected protected-file symlink: {path.name}"
            result[str(path.relative_to(home))] = path.read_bytes()
    return result


def file_inventory(home):
    """Hash-only inventory of all files in this isolated synthetic profile."""
    result = {}
    for path in sorted(home.rglob("*")):
        if path.is_symlink():
            data = os.readlink(path).encode()
            result[str(path.relative_to(home))] = {"kind": "symlink", "bytes": len(data),
                                                   "sha256": hashlib.sha256(data).hexdigest()}
        elif path.is_file():
            data = path.read_bytes()
            result[str(path.relative_to(home))] = {"bytes": len(data), "sha256": hashlib.sha256(data).hexdigest()}
    return result


def inventory_diagnostics(home, before, stage):
    after = file_inventory(home)
    changes = [{"path": name, "before": before.get(name), "after": after.get(name)}
               for name in sorted(set(before) | set(after)) if before.get(name) != after.get(name)]
    # Normal logs, SQLite, and writer locks are observations, not preservation failures.
    print(json.dumps({"stage": stage, "allFileInventoryChanges": changes}), flush=True)


def differences(before, after):
    result = []
    for name in sorted(set(before) | set(after)):
        if before.get(name) != after.get(name):
            result.append({"path": name, "beforeBytes": len(before[name]) if name in before else None,
                           "afterBytes": len(after[name]) if name in after else None,
                           "beforeSha256": hashlib.sha256(before[name]).hexdigest() if name in before else None,
                           "afterSha256": hashlib.sha256(after[name]).hexdigest() if name in after else None})
    return result


def listed_threads(app):
    result, cursor = {}, None
    while True:
        params = {"limit": 100, "modelProviders": ["mock_provider"]}
        if cursor:
            params["cursor"] = cursor
        page = app.request("thread/list", params)
        for thread in page["data"]:
            result[thread["id"]] = thread.get("path")
        cursor = page.get("nextCursor")
        if not cursor:
            return result


def original_identity(app, thread_id):
    thread = app.request("thread/read", {"threadId": thread_id, "includeTurns": False})["thread"]
    return {"id": thread["id"], "path": thread.get("path")}


def ui_diagnostics(app, thread_id, stage):
    try:
        items = app.items(thread_id)
        result = {"stage": stage, "itemCount": len(items),
                  "markers": sorted(set(MARKER_RE.findall(json.dumps(items))))}
    except AssertionError as error:
        items = None
        result = {"stage": stage, "itemError": str(error)}
    print(json.dumps(result), flush=True)
    return items


def projection_diagnostics(home, stage):
    """Read-only inspection of this freshly created synthetic profile's derived DB.

    Detect full-ancestor projection, including markers outside the visible range.
    Never copy, modify, repair, or open another profile's database.
    """
    reports = []
    for path in sorted(home.glob("*.sqlite")):
        with sqlite3.connect(path.as_uri() + "?mode=ro", uri=True) as connection:
            tables = {row[0] for row in connection.execute("SELECT name FROM sqlite_master WHERE type='table'")}
            if "thread_items" not in tables:
                continue
            columns = {row[1] for row in connection.execute("PRAGMA table_info(thread_items)")}
            if "item_json" not in columns:
                reports.append({"database": path.name, "note": "thread_items layout differs"})
                continue
            rows = connection.execute("SELECT item_json FROM thread_items").fetchall()
            reports.append({"database": path.name, "itemRows": len(rows),
                            "markers": sorted(set(MARKER_RE.findall(json.dumps(rows))))})
    print(json.dumps({"stage": stage, "derivedProjection": reports}), flush=True)
    return reports


def compress_zstd(data):
    library = ctypes.util.find_library("zstd")
    if not library:
        raise RuntimeError("Compressed diagnostic requires libzstd; no compressed result can be claimed")
    zstd = ctypes.CDLL(library)
    zstd.ZSTD_compressBound.argtypes = [ctypes.c_size_t]
    zstd.ZSTD_compressBound.restype = ctypes.c_size_t
    zstd.ZSTD_compress.argtypes = [ctypes.c_void_p, ctypes.c_size_t, ctypes.c_void_p, ctypes.c_size_t, ctypes.c_int]
    zstd.ZSTD_compress.restype = ctypes.c_size_t
    zstd.ZSTD_isError.argtypes = [ctypes.c_size_t]
    zstd.ZSTD_isError.restype = ctypes.c_uint
    source = ctypes.create_string_buffer(data)
    capacity = zstd.ZSTD_compressBound(len(data))
    target = ctypes.create_string_buffer(capacity)
    size = zstd.ZSTD_compress(target, capacity, source, len(data), 3)
    if zstd.ZSTD_isError(size):
        raise RuntimeError("libzstd compression failed")
    return target.raw[:size]


def check_preservation(gates, home, before, label):
    after = snapshot(home)
    changes = differences(before, after)
    print(json.dumps({"stage": label, "protectedFileChanges": changes}), flush=True)
    with gates.check(label + " byte preservation", fatal=False):
        assert not changes, "Native operation changed original rollout/source/index/config/auth bytes or paths"


def run_probe(args, logs, gates):
    with tempfile.TemporaryDirectory(prefix="codexm-projection-diagnostic-") as temporary:
        base = Path(temporary).resolve()
        project = base / "project"
        project.mkdir()
        for name in ("home", "manager", "profiles", "tmp", "runtime", "unused-codex"):
            (base / name).mkdir(mode=0o700)
        env = {"PATH": os.environ.get("PATH", "/usr/bin:/bin"), "LANG": "C.UTF-8",
               "HOME": str(base / "home"), "CODEX_HOME": str(base / "unused-codex"),
               "CODEXM_HOME": str(base / "manager"), "CODEXM_PROFILES_HOME": str(base / "profiles"),
               "TMPDIR": str(base / "tmp"), "XDG_RUNTIME_DIR": str(base / "runtime"),
               "CODEXM_CODEX_BIN": str(args.codex)}
        command_log = (logs / "codexm.log").open("w", encoding="utf-8")

        def codexm(*arguments):
            result = subprocess.run([str(args.codexm), *arguments], cwd=project, env=env,
                                    text=True, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=60)
            command_log.write(json.dumps(arguments) + "\n" + result.stdout + "\n")
            command_log.flush()
            assert result.returncode == 0, result.stdout

        version = subprocess.run([str(args.codex), "--version"], cwd=project, env=env,
                                 text=True, capture_output=True, check=True, timeout=15).stdout.strip()
        assert args.expected_version in version, version
        for name in ("origin", "raw", "compressed", "invalid", "standalone-compressed", "long-tail"):
            codexm("add", name)
        config = json.loads((base / "manager" / "config.json").read_text())
        profiles = {name: Path(value["codex_home"]).resolve() for name, value in config["profiles"].items()}
        assert all(home.is_relative_to(base) for home in profiles.values())
        server = MockResponses()
        try:
            provider = f'''model = "mock-model"
model_provider = "mock_provider"
approval_policy = "never"
sandbox_mode = "read-only"
[model_providers.mock_provider]
name = "Local synthetic diagnostic only"
base_url = "http://127.0.0.1:{server.server_port}/v1"
wire_api = "responses"
requires_openai_auth = false
request_max_retries = 0
stream_max_retries = 0
'''
            for home in profiles.values():
                (home / "config.toml").write_text(provider)
            markers = {name: f"codexm-projection-{name}-c836b2" for name in
                       ("parent-one", "parent-two", "child-keep", "child-discard")}
            origin_env = dict(env, CODEX_HOME=str(profiles["origin"]))
            with AppServer(args.codex, origin_env, project, logs, "projection-origin") as app:
                parent = app.request("thread/start", {
                    "cwd": str(project), "historyMode": "paginated", "model": "mock-model",
                    "modelProvider": "mock_provider", "approvalPolicy": "never", "sandbox": "read-only",
                    "baseInstructions": "Synthetic diagnostic. No tools.",
                })["thread"]
                parent_kept_turn = app.turn(parent["id"], markers["parent-one"])
                app.turn(parent["id"], markers["parent-two"])
                child = app.request("thread/fork", {"threadId": parent["id"], "cwd": str(project),
                                                     "excludeTurns": True})["thread"]
                kept_turn = app.turn(child["id"], markers["child-keep"])
                discarded_turn = app.turn(child["id"], markers["child-discard"])
                reverted = app.request("thread/revert", {"threadId": child["id"],
                                                          "beforeTurnId": discarded_turn})["thread"]
                expected = [markers[name] for name in ("parent-one", "parent-two", "child-keep")]
                assert_markers(app.items(child["id"]), expected, [markers["child-discard"]])
            codexm("session", "init", "--project", str(project), "--profile", "origin", "--import-existing")
            request_count = len(server.requests)
            child_relative = Path(child["path"]).relative_to(profiles["origin"])

            print(json.dumps({"stage": "compression-availability", "libzstd": ctypes.util.find_library("zstd")}), flush=True)
            for case in ("invalid", "raw", "long-tail", "compressed", "standalone-compressed"):
                with gates.check(case + " diagnostic setup and execution", fatal=False):
                    home = profiles[case]
                    target_id, cutoff, visible_markers = child["id"], kept_turn, expected
                    if case == "standalone-compressed":
                        # Copy only the original native standalone parent (no descendants,
                        # databases, index, or credentials) into this separate synthetic case.
                        target_id, cutoff = parent["id"], parent_kept_turn
                        visible_markers = [markers["parent-one"], markers["parent-two"]]
                        parent_relative = Path(parent["path"]).relative_to(profiles["origin"])
                        target = home / parent_relative
                        target.parent.mkdir(parents=True, exist_ok=True)
                        source_bytes = Path(parent["path"]).read_bytes()
                        compressed = target.with_name(target.name + ".zst")
                        compressed.write_bytes(compress_zstd(source_bytes))
                        print(json.dumps({"stage": "standalone-compressed-fixture",
                                          "path": str(compressed.relative_to(home)),
                                          "rawBytes": len(source_bytes), "compressedBytes": compressed.stat().st_size}), flush=True)
                    else:
                        codexm("session", "sync", "--project", str(project), "--profile", case)
                    assert not list(home.glob("*.sqlite*")), "Fixture copied a native database"
                    if case == "long-tail":
                        # Simulate an already-present compatible destination source with a
                        # longer tail. The reverted tip's inherited cutoff is not changed.
                        full_ancestor = Path(child["path"]).read_bytes()
                        assert markers["child-discard"].encode() in full_ancestor
                        ancestor = home / child_relative
                        assert full_ancestor.startswith(ancestor.read_bytes()), "Not a compatible source prefix"
                        ancestor.write_bytes(full_ancestor)
                        print(json.dumps({"stage": "long-tail-fixture", "path": str(child_relative),
                                          "bytes": len(full_ancestor), "discardedMarkerBeyondCutoff": True}), flush=True)
                    if case == "compressed":
                        ancestor = home / child_relative
                        assert ancestor.exists(), "Required child-history ancestor not copied"
                        data = ancestor.read_bytes()
                        encoded = compress_zstd(data)
                        compressed = ancestor.with_name(ancestor.name + ".zst")
                        compressed.write_bytes(encoded)
                        ancestor.unlink()
                        print(json.dumps({"stage": "compressed-fixture", "path": str(compressed.relative_to(home)),
                                          "rawBytes": len(data), "compressedBytes": len(encoded)}), flush=True)
                    before = snapshot(home)
                    before_inventory = file_inventory(home)
                    case_env = dict(env, CODEX_HOME=str(home))
                    with AppServer(args.codex, case_env, project, logs, "projection-" + case) as app:
                        initial_identity = original_identity(app, target_id)
                        initial_list = listed_threads(app)
                        print(json.dumps({"stage": case + " baseline", "identity": initial_identity,
                                          "listedThreads": initial_list}), flush=True)
                        baseline_items = ui_diagnostics(app, target_id, case + " baseline UI")
                        with gates.check(case + " cold baseline UI empty", fatal=False):
                            assert baseline_items == [], "Expected empty cold projection before the diagnostic operation"
                        before_projection = projection_diagnostics(home, case + " baseline projection")
                        # Snapshot predates startup/list/read so unexpected preprocessing is visible too.
                        check_preservation(gates, home, before, case + " pre-fork startup")
                        if case == "invalid":
                            rejection = None
                            try:
                                app.request("thread/fork", {"threadId": target_id, "ephemeral": True,
                                                            "excludeTurns": True,
                                                            "lastTurnId": "00000000-0000-4000-8000-000000000999"})
                            except AssertionError as error:
                                rejection = str(error)
                            with gates.check("invalid cutoff rejected", fatal=False):
                                assert rejection and rejection.startswith("thread/fork:"), rejection
                            print(json.dumps({"stage": "invalid-cutoff", "error": rejection}), flush=True)
                            after_projection = projection_diagnostics(home, "invalid cutoff after projection")
                            print(json.dumps({"stage": "invalid-cutoff projection side effect",
                                              "projectionChanged": before_projection != after_projection}), flush=True)
                            ui_diagnostics(app, target_id, "invalid cutoff after UI")
                        else:
                            for repetition in (1, 2):
                                label = f"{case} ephemeral cutoff fork {repetition}"
                                fork = app.request("thread/fork", {"threadId": target_id, "ephemeral": True,
                                                                   "excludeTurns": True, "lastTurnId": cutoff})["thread"]
                                with gates.check(label + " ephemeral identity", fatal=False):
                                    assert fork["id"] != target_id and fork.get("path") is None, fork
                                items = ui_diagnostics(app, target_id, label + " original UI")
                                with gates.check(label + " reconstructs original visible UI", fatal=False):
                                    assert items is not None
                                    assert_markers(items, visible_markers, [markers["child-discard"]])
                                projection_diagnostics(home, label + " full physical projection")
                                with gates.check(label + " original identity and list unchanged", fatal=False):
                                    assert original_identity(app, target_id) == initial_identity
                                    assert listed_threads(app) == initial_list
                                check_preservation(gates, home, before, label)
                        check_preservation(gates, home, before, case + " end before shutdown")
                        with gates.check(case + " no model call from fork", fatal=False):
                            assert len(server.requests) == request_count
                        with gates.check(case + " final original identity and list", fatal=False):
                            assert original_identity(app, target_id) == initial_identity
                            assert listed_threads(app) == initial_list
                    check_preservation(gates, home, before, case + " after shutdown")
                    inventory_diagnostics(home, before_inventory, case + " inventory after shutdown")
                    with AppServer(args.codex, case_env, project, logs, "projection-" + case + "-cold") as app:
                        with gates.check(case + " cold original identity and list", fatal=False):
                            assert original_identity(app, target_id) == initial_identity
                            assert listed_threads(app) == initial_list
                        items = ui_diagnostics(app, target_id, case + " cold original UI")
                        if case != "invalid":
                            with gates.check(case + " reconstruction survives cold restart", fatal=False):
                                assert items is not None
                                assert_markers(items, visible_markers, [markers["child-discard"]])
                        projection_diagnostics(home, case + " cold projection")
                    check_preservation(gates, home, before, case + " after cold shutdown")
                    inventory_diagnostics(home, before_inventory, case + " inventory after cold restart")
                    with gates.check(case + " final no model calls or auth", fatal=False):
                        assert len(server.requests) == request_count
                        assert not (home / "auth.json").exists()
            return {"diagnosticOnly": True, "productionAcceptance": "not assessed",
                    "status": gates.status(), "codexVersion": version, "gates": gates.results,
                    "mockSeedRequests": request_count, "mockFinalRequests": len(server.requests),
                    "assumptions": ["native completed child turn ID used as lastTurnId; no Latest shortcut",
                                    "isolated copies of real native synthetic rollouts; no auth/database copying",
                                    "SQLite inspected read-only only in synthetic profiles",
                                    "compression fixture uses native .jsonl.zst; .jsonl.gz unsupported"],
                    "caveats": ["ephemeral cutoff fork is an experiment, not a supported repair contract",
                                "full ancestor indexing and decompression may have side effects",
                                "success here does not waive independent native acceptance failures"]}
        finally:
            server.close()
            command_log.close()


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--codex", type=Path, required=True)
    parser.add_argument("--codexm", type=Path, required=True)
    parser.add_argument("--artifacts", type=Path, required=True)
    parser.add_argument("--expected-version", default="0.162.1")
    args = parser.parse_args()
    args.codex = args.codex.resolve(strict=True)
    args.codexm = args.codexm.resolve(strict=True)
    args.artifacts.mkdir(parents=True, exist_ok=True)
    gates = GateResults()
    try:
        result = run_probe(args, args.artifacts, gates)
    except BaseException as error:
        result = {"diagnosticOnly": True, "productionAcceptance": "not assessed", "status": "failed",
                  "error": str(error), "gates": gates.results}
        traceback.print_exc()
    (args.artifacts / "projection-summary.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))
    if result["status"] != "passed":
        raise SystemExit(1)


if __name__ == "__main__":
    main()
