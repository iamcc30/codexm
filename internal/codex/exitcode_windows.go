//go:build windows

package codex

import "os/exec"

func normalizedExitCode(exitErr *exec.ExitError) int {
	if code := exitErr.ExitCode(); code >= 0 {
		return code
	}
	return 1
}
