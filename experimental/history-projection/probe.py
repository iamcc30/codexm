#!/usr/bin/env python3
"""Experimental, synthetic-only proof of a version-pinned UI projector.

This does not modify codexm or publish into existing profile databases. It wraps
the independent native acceptance harness and supplies a derived database only
to the fresh synthetic target, after proving its rollout-only baseline is empty.
"""
import argparse
import copy
from contextlib import closing
import hashlib
import importlib.util
import json
import os
from pathlib import Path
import re
import shutil
import sqlite3
import subprocess
import traceback


UUID = r"[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}"
ROLLOUT = re.compile(rf"rollout-.+-({UUID})(?:_({UUID}))?\.jsonl$")


def digest(data):
    return hashlib.sha256(data).hexdigest()


def inventory(root):
    result = {}
    for path in sorted(root.rglob("*")):
        assert not path.is_symlink(), f"Unexpected symlink in synthetic source: {path}"
        if path.is_file():
            metadata = path.stat()
            result[str(path.relative_to(root))] = {
                "sha256": digest(path.read_bytes()), "mode": metadata.st_mode,
                "bytes": metadata.st_size, "mtime_ns": metadata.st_mtime_ns,
                "inode": metadata.st_ino, "device": metadata.st_dev,
            }
    return result


def physical_id(path):
    match = ROLLOUT.fullmatch(path.name)
    assert match, f"Unrecognized native rollout name: {path.name}"
    return match.group(2) or match.group(1)


def manifest_for_child(source, target_name, output):
    by_id = {}
    for path in source.rglob("*.jsonl"):
        key = physical_id(path)
        assert key not in by_id, "Ambiguous physical rollout in fixture"
        by_id[key] = path
    target = next(path for path in by_id.values() if path.name == target_name)
    next_id = physical_id(target)
    cutoff, seen, rollouts = None, set(), []
    while True:
        assert next_id not in seen, "Cyclic synthetic lineage"
        seen.add(next_id)
        path = by_id[next_id]
        data = path.read_bytes()
        records = [json.loads(line) for line in data.splitlines()]
        meta = records[0]
        assert meta["type"] == "session_meta"
        assert meta["payload"]["history_mode"] == "paginated"
        boundary = cutoff or {
            "end_byte_offset": len(data),
            "end_ordinal_exclusive": records[-1]["ordinal"] + 1,
        }
        prefix = data[:boundary["end_byte_offset"]]
        assert len(prefix) == boundary["end_byte_offset"] and prefix.endswith(b"\n")
        rollouts.append({
            "rollout_id": next_id,
            "path": str(path.resolve()),
            "end_byte_offset": boundary["end_byte_offset"],
            "end_ordinal_exclusive": boundary["end_ordinal_exclusive"],
            "prefix_sha256": digest(prefix),
        })
        cutoff = meta["payload"].get("history_base")
        if not cutoff:
            break
        next_id = cutoff["thread_id"]
    return {"source_root": str(source.resolve()), "output_dir": str(output.resolve()),
            "rollouts": list(reversed(rollouts))}


def invoke(helper, manifest, env):
    process = subprocess.run([str(helper)], input=json.dumps(manifest), text=True,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE,
                             env=env, cwd=manifest["source_root"], timeout=120)
    return process


def rejection_checks(helper, manifest, stage, env, checks):
    """Fail-closed probes use only this harness's synthetic copies."""
    cases = []
    wrong_hash = copy.deepcopy(manifest)
    wrong_hash["rollouts"][0]["prefix_sha256"] = "0" * 64
    cases.append(("hash mismatch", wrong_hash))
    wrong_id = copy.deepcopy(manifest)
    wrong_id["rollouts"][0]["rollout_id"] = "00000000-0000-4000-8000-000000000999"
    cases.append(("physical id mismatch", wrong_id))
    wrong_bound = copy.deepcopy(manifest)
    wrong_bound["rollouts"][0]["end_ordinal_exclusive"] += 1
    cases.append(("ordinal boundary mismatch", wrong_bound))
    duplicate = copy.deepcopy(manifest)
    duplicate["rollouts"].append(copy.deepcopy(duplicate["rollouts"][0]))
    cases.append(("duplicate physical id", duplicate))
    extra_field = copy.deepcopy(manifest)
    extra_field["unrecognized"] = True
    cases.append(("unknown manifest field", extra_field))

    original = manifest["rollouts"][0]
    original_bytes = Path(original["path"]).read_bytes()[:original["end_byte_offset"]]
    for name in ("partial record", "malformed JSON", "unknown record", "ordinal regression", "compressed source", "symlink source"):
        root = stage / ("reject-" + name.replace(" ", "-"))
        root.mkdir()
        path = root / Path(original["path"]).name
        data = original_bytes
        if name == "partial record":
            data = data[:-1]
        elif name == "malformed JSON":
            lines = data.splitlines(keepends=True)
            lines[-1] = b"not-json\n"
            data = b"".join(lines)
        elif name in ("unknown record", "ordinal regression"):
            lines = [json.loads(line) for line in data.splitlines()]
            if name == "unknown record":
                lines[-1]["type"] = "unsupported_future_record"
            else:
                lines[-1]["ordinal"] = lines[0]["ordinal"]
            data = b"".join(json.dumps(line, separators=(",", ":")).encode() + b"\n" for line in lines)
        elif name == "compressed source":
            path = path.with_name(path.name + ".zst")
        if name == "symlink source":
            path.symlink_to(Path(original["path"]))
        else:
            path.write_bytes(data)
            path.chmod(0o444)
        entry = dict(original, path=str(path), end_byte_offset=len(data), prefix_sha256=digest(data))
        cases.append((name, {"source_root": str(root), "output_dir": "", "rollouts": [entry]}))
    for index, (name, bad) in enumerate(cases):
        output = stage / f"rejected-output-{index}"
        bad["output_dir"] = str(output)
        result = invoke(helper, bad, env)
        assert result.returncode != 0, f"Helper accepted {name}"
        assert not output.exists(), f"Invalid {name} initialized output storage"
        checks.append("rejects " + name + " before storage initialization")
    occupied = stage / "existing-output"
    occupied.mkdir()
    sentinel = occupied / "sentinel"
    sentinel.write_bytes(b"synthetic preservation sentinel")
    bad = dict(manifest, output_dir=str(occupied))
    result = invoke(helper, bad, env)
    assert result.returncode != 0
    assert {p.name for p in occupied.iterdir()} == {"sentinel"}
    assert sentinel.read_bytes() == b"synthetic preservation sentinel"
    checks.append("rejects existing output without changing it")


def prepare_projection(helper, home, child_name, env, artifacts, checks, publisher=None, recovery=False):
    assert not list(home.glob("*.sqlite*")), "Only a brand-new synthetic profile is supported"
    base = home.parents[1]
    assert base.name.startswith("codexm-native-acceptance-"), "Refusing non-harness home"
    stage = base / "projection-only-probe"
    source, output = stage / "source", stage / "output"
    source.mkdir(parents=True)
    shutil.copytree(home / "sessions", source / "sessions")
    destination_before = inventory(home)
    for path in source.rglob("*"):
        path.chmod(0o555 if path.is_dir() else 0o444)
    source.chmod(0o555)
    before = inventory(source)
    manifest = manifest_for_child(source, child_name, output)
    # The helper is launched with an allowlisted environment from the native harness,
    # and no auth file or user/project configuration is present in its input root.
    result = invoke(helper, manifest, env)
    (artifacts / "projector.stdout.json").write_text(result.stdout)
    (artifacts / "projector.stderr.log").write_text(result.stderr)
    assert result.returncode == 0, result.stderr or result.stdout
    report = json.loads(result.stdout)
    assert report["status"] == "projected", report
    assert inventory(source) == before, "Projector changed immutable source bytes"
    assert inventory(home) == destination_before, "Projector touched destination profile"
    assert {p.name for p in output.iterdir()} == {"thread_history_1.sqlite"}, "Unexpected helper output"
    db = output / "thread_history_1.sqlite"
    # The helper has closed every connection and the inventory proved no WAL/SHM.
    # Immutable reads avoid creating sidecars on this finished snapshot.
    with closing(sqlite3.connect(db.as_uri() + "?mode=ro&immutable=1", uri=True)) as connection:
        assert connection.execute("PRAGMA integrity_check").fetchone() == ("ok",)
        assert connection.execute("SELECT count(*) FROM thread_items").fetchone()[0] > 0
    checks.append("bounded native projection preserves source and target files")
    rejection_checks(helper, manifest, stage, env, checks)
    assert inventory(source) == before, "Rejection probes changed immutable sources"
    assert inventory(home) == destination_before, "Rejection probes touched destination profile"
    if publisher is not None:
        from publisher_checks import run_publisher_checks
        run_publisher_checks(publisher, db, manifest, base, env, artifacts, checks,
                             native_source_home=home.parent / "first")
        from advancement_checks import run_advancement_checks
        run_advancement_checks(helper, publisher, db, manifest, base, env, artifacts, checks)
        if recovery:
            from recovery_checks import run_recovery_checks
            run_recovery_checks(publisher, db, manifest, base, env, artifacts, checks)
        assert inventory(source) == before, "Publisher/recovery probes changed immutable rollout sources"
        assert inventory(home) == destination_before, "Publisher probes touched destination profile"
    # Diagnostic-only injection into a fresh synthetic home, never a production publish path.
    shutil.copyfile(db, home / db.name)
    checks.append("fresh synthetic target receives only derived history database")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--helper", required=True, type=Path)
    parser.add_argument("--publisher", type=Path, help="Optional separate synthetic publisher proof")
    parser.add_argument("--lock-probe", type=Path, help="Optional synthetic native lock boundary proof")
    parser.add_argument("--recovery-probe", action="store_true", help="Run quiescent synthetic recovery checks")
    parser.add_argument("--codex", required=True, type=Path)
    parser.add_argument("--codexm", required=True, type=Path)
    parser.add_argument("--artifacts", required=True, type=Path)
    parser.add_argument("--expected-version", default="0.166.0")
    parser.add_argument("--native-harness", required=True, type=Path)
    args = parser.parse_args()
    for field in ("helper", "codex", "codexm", "native_harness"):
        setattr(args, field, getattr(args, field).resolve(strict=True))
    if args.publisher is not None:
        args.publisher = args.publisher.resolve(strict=True)
    if args.lock_probe is not None:
        args.lock_probe = args.lock_probe.resolve(strict=True)
    if args.recovery_probe and args.publisher is None:
        parser.error("--recovery-probe requires --publisher")
    args.artifacts = args.artifacts.resolve()
    args.artifacts.mkdir(parents=True, exist_ok=True)
    specification = importlib.util.spec_from_file_location("native_acceptance", args.native_harness)
    native = importlib.util.module_from_spec(specification)
    specification.loader.exec_module(native)
    original = native.AppServer
    captured, checks = {}, []

    class ProbeAppServer(original):
        def __init__(self, binary, env, cwd, logs, label, launcher=None):
            if label == "second-resume":
                # Establish the negative control in a disjoint synthetic copy. Native
                # shutdown can leave WAL/SHM, so never open or replace target DB state.
                home = Path(env["CODEX_HOME"])
                if args.lock_probe is not None:
                    from native_lock_checks import run_native_lock_checks
                    run_native_lock_checks(args.lock_probe, args.codex, original,
                                           home.parent / "first", captured["child_id"], captured["child_name"],
                                           home.parents[1], env, cwd, args.artifacts, checks)
                assert not list(home.glob("*.sqlite*")), "Target must remain unopened"
                target_before = inventory(home)
                baseline_home = home.parent / "second-unprojected"
                baseline_home.mkdir()
                shutil.copytree(home / "sessions", baseline_home / "sessions")
                shutil.copyfile(home / "config.toml", baseline_home / "config.toml")
                baseline_env = dict(env, CODEX_HOME=str(baseline_home))
                with original(binary, baseline_env, cwd, logs, "unprojected-baseline") as baseline:
                    baseline.request("thread/resume", {"threadId": captured["child_id"], "excludeTurns": True})
                    items = baseline.items(captured["child_id"])
                    assert items == [], "Baseline changed; reassess before applying experimental projection"
                assert inventory(home) == target_before, "Negative control touched target profile"
                assert not list(home.glob("*.sqlite*")), "Negative control opened target storage"
                checks.append("negative control returns empty UI without touching target profile")
                prepare_projection(args.helper, home, captured["child_name"], env, args.artifacts, checks, args.publisher, args.recovery_probe)
            super().__init__(binary, env, cwd, logs, label, launcher=launcher)

        def request(self, method, params):
            if method == "thread/items/list":
                # Force multiple pages for this small fixture rather than proving only
                # the first page happens to contain the expected markers.
                params = dict(params, limit=2)
            result = super().request(method, params)
            if self.label == "first-create" and method == "thread/revert":
                captured["child_id"] = result["thread"]["id"]
                captured["child_name"] = Path(result["thread"]["path"]).name
            return result

        def items(self, thread_id):
            result, cursor, seen_cursors, seen_items = [], None, set(), set()
            while True:
                params = {"threadId": thread_id, "limit": 2, "sortDirection": "asc"}
                if cursor is not None:
                    params["cursor"] = cursor
                page = self.request("thread/items/list", params)
                for item in page["data"]:
                    identity = json.dumps(item, sort_keys=True)
                    assert identity not in seen_items, "Native pagination duplicated an item"
                    seen_items.add(identity)
                    result.append(item)
                cursor = page.get("nextCursor")
                if not cursor:
                    break
                assert cursor not in seen_cursors and len(seen_cursors) < 10000, "Native pagination failed to advance"
                seen_cursors.add(cursor)
            print(json.dumps({"stage": self.label, "method": "thread/items/list complete",
                              "pages": len(seen_cursors) + 1, "items": len(result)}), flush=True)
            if self.label == "first-create":
                captured["expected_items"] = result
            if self.label == "second-resume" and "parity_checked" not in captured:
                assert result == captured["expected_items"], "Full native item payload parity failed"
                captured["parity_checked"] = True
                checks.append("full paginated item payload parity for fixture before B continuation")
            elif self.label == "second-resume":
                captured["second_after_items"] = result
            if self.label == "first-resume" and "roundtrip_parity_checked" not in captured:
                assert result == captured["second_after_items"], "Roundtrip full item payload parity failed"
                captured["roundtrip_parity_checked"] = True
                checks.append("full paginated item payload parity for fixture before A continuation")
            return result

    native.AppServer = ProbeAppServer
    gates = native.GateResults()
    try:
        result = native.run_acceptance(args, args.artifacts, gates)
        assert captured.get("parity_checked"), "Native parity gate never ran"
        assert captured.get("roundtrip_parity_checked"), "Native roundtrip parity gate never ran"
        result.update({"diagnosticOnly": True, "productionAcceptance": "not assessed",
                       "projectorChecks": checks,
                       "limitations": result.get("limitations", []) + [
                           "experimental pinned projector, not part of codexm sync",
                           "plain JSONL only; compressed sources remain unsupported",
                           "fresh isolated DB only; existing-profile transactional publication unimplemented"]})
    except BaseException as error:
        traceback.print_exc()
        result = {"status": "failed", "diagnosticOnly": True,
                  "productionAcceptance": "not assessed", "error": str(error),
                  "projectorChecks": checks, "gates": gates.results}
    (args.artifacts / "projection-helper-summary.json").write_text(json.dumps(result, indent=2) + "\n")
    print(json.dumps(result, indent=2))
    if result["status"] != "passed":
        raise SystemExit(1)


if __name__ == "__main__":
    main()
