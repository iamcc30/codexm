//go:build windows

package appserver

import (
	"os/exec"
	"syscall"
)

func configureDaemonCommand(cmd *exec.Cmd) {
	const createNewProcessGroup = 0x00000200
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
}
