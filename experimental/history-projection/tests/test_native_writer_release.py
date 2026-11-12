"""The native fixture waits only for its orderly-exited writer's lease tail."""
import importlib.util
import io
from pathlib import Path
import subprocess
import unittest
from unittest import mock


spec = importlib.util.spec_from_file_location(
    "native_writer_acceptance", Path(__file__).resolve().parents[3] / "scripts/native-history-acceptance.py")
native = importlib.util.module_from_spec(spec)
spec.loader.exec_module(native)


class WriterLeaseRelease(unittest.TestCase):
    def test_expected_contention_is_polled_read_only_until_release(self):
        busy = "error: acquire session writer lease: another codexm run or session synchronization is using this project or profile; wait for it to exit\n"
        replies = [subprocess.CompletedProcess([], 1, busy), subprocess.CompletedProcess([], 0, "ready\n")]
        with mock.patch.object(native.subprocess, "run", side_effect=replies) as run:
            native.wait_for_writer_lease_release("codexm", Path("/project"), Path("/profile"), {}, io.StringIO())
        self.assertEqual(2, run.call_count)
        self.assertTrue(all(call.args[0][1:3] == ["session", "status"] for call in run.call_args_list))

    def test_unexpected_errors_are_not_retried(self):
        with mock.patch.object(native.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, "error: corrupt input\n")) as run:
            with self.assertRaisesRegex(AssertionError, "Unexpected lease-release probe failure"):
                native.wait_for_writer_lease_release("codexm", Path("/project"), Path("/profile"), {}, io.StringIO())
        self.assertEqual(1, run.call_count)

    def test_permanently_held_lease_fails_with_bounded_diagnostics(self):
        busy = "error: acquire session writer lease: another codexm run or session synchronization is using this project or profile; wait for it to exit\n"
        with mock.patch.object(native.subprocess, "run", return_value=subprocess.CompletedProcess([], 1, busy)):
            with self.assertRaisesRegex(TimeoutError, "project=/project, profile=/profile"):
                native.wait_for_writer_lease_release("codexm", Path("/project"), Path("/profile"), {}, io.StringIO(), timeout=0.01)


if __name__ == "__main__":
    unittest.main()
