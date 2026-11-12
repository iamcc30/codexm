"""Keep the known JSONL-only blocker separate from native acceptance and regressions."""
import importlib.util
from pathlib import Path
import unittest
import subprocess
import sys
import sqlite3
import tempfile


spec = importlib.util.spec_from_file_location(
    "current_tip_checks", Path(__file__).resolve().parents[1] / "current_tip_checks.py")
diagnostic = importlib.util.module_from_spec(spec)
spec.loader.exec_module(diagnostic)

TIP_CHECK = "cold logical-ID resume selects B's intended physical tip"


class CurrentTipExitCode(unittest.TestCase):
    def setUp(self):
        self.blocked = {
            "status": "blocked", "completed_diagnostic": True,
            "native_tip_resolution": "stale_or_wrong_tip",
            "state_adapter_needed_evidence": True, "failures": [TIP_CHECK],
        }

    def test_known_blocker_requires_explicit_expectation(self):
        self.assertEqual(1, diagnostic.diagnostic_exit_code(self.blocked))
        self.assertEqual(0, diagnostic.diagnostic_exit_code(self.blocked, expect_stale_tip=True))
        self.assertEqual("blocked", self.blocked["status"])

    def test_native_acceptance_pass_is_not_the_expected_blocker(self):
        passed = dict(self.blocked, status="passed", failures=[],
                      native_tip_resolution="intended_tip", state_adapter_needed_evidence=False)
        self.assertEqual(0, diagnostic.diagnostic_exit_code(passed))
        self.assertEqual(1, diagnostic.diagnostic_exit_code(passed, expect_stale_tip=True))

    def test_native_repair_cannot_use_expected_blocker_waiver(self):
        report = dict(self.blocked, native_history_sync=True)
        self.assertEqual(1, diagnostic.diagnostic_exit_code(report, expect_stale_tip=True))

    def test_cleanup_error_or_incomplete_native_acceptance_fails(self):
        passed = dict(self.blocked, status="passed", failures=[])
        self.assertEqual(1, diagnostic.diagnostic_exit_code(dict(passed, error="native shutdown failed")))
        self.assertEqual(1, diagnostic.diagnostic_exit_code(dict(passed, completed_diagnostic=False)))

    def test_native_snapshot_observes_only_the_selected_partition(self):
        with tempfile.TemporaryDirectory() as temporary:
            home = Path(temporary)
            with sqlite3.connect(home / "state_5.sqlite") as db:
                db.execute("CREATE TABLE threads(id TEXT PRIMARY KEY,rollout_path TEXT,title TEXT) WITHOUT ROWID")
                db.execute("INSERT INTO threads VALUES('chosen','old.jsonl','preserved')")
                db.execute("INSERT INTO threads VALUES('other','other.jsonl','independent')")
            with sqlite3.connect(home / "thread_history_1.sqlite") as db:
                for table in ("thread_turns", "thread_items", "thread_realtime_items", "thread_history_projection_state"):
                    db.execute(f"CREATE TABLE {table}(thread_id TEXT PRIMARY KEY,value TEXT) WITHOUT ROWID")
                    db.execute(f"INSERT INTO {table} VALUES('chosen','expected')")
                    db.execute(f"INSERT INTO {table} VALUES('other','independent')")
            before = diagnostic.native_thread_snapshot(home, "chosen")
            with sqlite3.connect(home / "state_5.sqlite") as db:
                db.execute("UPDATE threads SET title='changed' WHERE id='other'")
            self.assertEqual(before, diagnostic.native_thread_snapshot(home, "chosen"))
            with sqlite3.connect(home / "thread_history_1.sqlite") as db:
                db.execute("UPDATE thread_items SET value='regression' WHERE thread_id='chosen'")
            self.assertNotEqual(before, diagnostic.native_thread_snapshot(home, "chosen"))

    @unittest.skipIf(sys.platform == "win32", "native helper faults require Unix signals")
    def test_helper_pause_fault_kills_child_and_preserves_caller(self):
        script = """import subprocess,sys
child = subprocess.Popen([sys.executable, '-c', 'import time;time.sleep(30)'])
print('CODEXM_NATIVE_HISTORY_TEST_HELPER_PID:' + str(child.pid), file=sys.stderr, flush=True)
print('CODEXM_NATIVE_HISTORY_TEST_PAUSED:state_after_commit', file=sys.stderr, flush=True)
result = child.wait()
print('helper-exit:' + str(result), flush=True)
sys.exit(1 if result == -9 else 2)
"""
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            result = diagnostic.kill_helper_at_pause([sys.executable, "-c", script],
                                                    None, root, root, "state_after_commit")
            self.assertEqual(1, result.returncode)
            self.assertIn("helper-exit:-9", result.stdout)
            self.assertIn("CODEXM_NATIVE_HISTORY_TEST_PAUSED:state_after_commit", result.stderr)

    def test_cli_rejects_conflicting_or_incomplete_acceptance_modes(self):
        script = Path(__file__).resolve().parents[1] / "current_tip_checks.py"
        required = ["--codex", "unused", "--codexm", "unused", "--native-harness", "unused", "--artifacts", "unused"]
        for arguments in (["--native-history", "--expect-stale-tip"],
                          ["--native-history", "--fault-point", "files:verified"],
                          ["--fault-codexm", "unused", "--fault-point", "files:verified"],
                          ["--native-history", "--helper-fault-point", "history_committed"],
                          ["--native-history", "--fault-codexm", "unused", "--fault-point", "files:verified",
                           "--helper-fault-point", "state_before_commit"]):
            with self.subTest(arguments=arguments):
                result = subprocess.run([sys.executable, str(script), *required, *arguments],
                                        capture_output=True, text=True, timeout=10)
                self.assertEqual(2, result.returncode, result.stdout + result.stderr)

    def test_unexpected_failures_are_never_waived(self):
        changes = [
            {"status": "failed"},
            {"completed_diagnostic": False},
            {"native_tip_resolution": "not assessed"},
            {"native_tip_resolution": "intended_tip"},
            {"state_adapter_needed_evidence": False},
            {"failures": []},
            {"failures": ["cold A full paginated item payloads equal B after revert"]},
            {"failures": [TIP_CHECK, "A model replay includes retained history and excludes B's discarded turn"]},
            {"error": "native RPC failed after observing the stale tip"},
        ]
        for change in changes:
            with self.subTest(change=change):
                report = dict(self.blocked, **change)
                self.assertEqual(1, diagnostic.diagnostic_exit_code(report))
                self.assertEqual(1, diagnostic.diagnostic_exit_code(report, expect_stale_tip=True))


if __name__ == "__main__":
    unittest.main()
