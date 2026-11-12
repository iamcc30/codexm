//go:build linux && nativehistorytest

package session

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Fault injection is absent from ordinary builds, including release builds.
func init() {
	frozenRecoveryTestHook = func(point string) {
		if point == os.Getenv("CODEXM_NATIVE_HISTORY_TEST_FAULT") {
			_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
		}
	}
	nativeHistoryTestChildSetup = func(command *exec.Cmd) {
		point := os.Getenv("CODEXM_NATIVE_HISTORY_TEST_PAUSE_AT")
		switch point {
		case "history_committed", "state_before_commit", "state_after_commit":
			command.Env = append(command.Env, "CODEXM_NATIVE_HISTORY_TEST_PAUSE_AT="+point)
			command.Stderr = os.Stderr
		}
	}
	nativeHistoryTestChildStarted = func(pid int) {
		fmt.Fprintf(os.Stderr, "CODEXM_NATIVE_HISTORY_TEST_HELPER_PID:%d\n", pid)
	}
}
