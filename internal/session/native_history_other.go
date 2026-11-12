//go:build !linux

package session

import "errors"

func SyncNativeHistory(opts NativeHistoryOptions) (Result, error) {
	return Result{}, errors.New("opt-in native-history synchronization is supported only on Linux")
}

func validateNativeHistoryLaunch(home, project, executable string) error {
	return errors.New("native-history managed profiles are supported only on Linux")
}
