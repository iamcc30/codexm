"""Keep the known JSONL-only blocker separate from native acceptance and regressions."""
import importlib.util
from pathlib import Path
import unittest


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
