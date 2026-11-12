package session

import (
	"errors"
	"os"
	"path/filepath"
)

// These pins are supplied by the release build, never configuration or a journal.
var NativeHistoryBinarySHA256 string
var NativeHistoryHelperSHA256 string

const nativeManagedFile = ".codexm-native-history"

func NativeHistoryManaged(home string) (bool, error) {
	path := filepath.Join(home, nativeManagedFile)
	if err := rejectSymlink(path); err != nil {
		return false, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if string(data) != "codexm offline native history v1\n" {
		return false, errors.New("invalid native-history managed-profile marker")
	}
	return true, nil
}

func CheckNativePending(home string) error {
	if home == "" {
		return nil
	}
	path := filepath.Join(home, ".codexm-sync-v2", "pending.json")
	if err := rejectSymlink(filepath.Dir(path)); err != nil {
		return err
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("native-history synchronization is pending; recover with session sync --native-history --native-writers-stopped before launching or modifying this profile")
	}
	return nil
}

type NativeHistoryOptions struct {
	Options
	NativeExecutable string
	StoppedWriters   bool
	SessionID        string
}

func ValidateNativeHistoryLaunch(home, project, executable string, args []string) error {
	managed, err := NativeHistoryManaged(home)
	if err != nil || !managed {
		return err
	}
	if len(args) != 0 {
		return errors.New("native-history profiles initially support only a foreground interactive run without native flags or subcommands")
	}
	return validateNativeHistoryLaunch(home, project, executable)
}
