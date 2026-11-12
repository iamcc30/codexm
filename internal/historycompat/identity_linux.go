//go:build linux

package historycompat

import (
	"os"
	"syscall"
)

func singleLink(i os.FileInfo) bool { s, ok := i.Sys().(*syscall.Stat_t); return ok && s.Nlink == 1 }
