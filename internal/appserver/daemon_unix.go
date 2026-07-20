//go:build !windows

package appserver

import (
	"os/exec"
	"syscall"
)

func configureDaemonCommand(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
