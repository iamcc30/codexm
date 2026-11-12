//go:build !linux

package historycompat

import "os"

func singleLink(os.FileInfo) bool { return false }
