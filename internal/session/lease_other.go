//go:build !linux

package session

import (
	"errors"
	"github.com/gofrs/flock"
	"os"
)

type sessionLeaseLock struct{ handle *flock.Flock }

func lockSessionLease(path string) (sessionLeaseLock, error) {
	lock := flock.New(path)
	locked, err := lock.TryLock()
	if err != nil {
		return sessionLeaseLock{}, err
	}
	if !locked {
		return sessionLeaseLock{}, errors.New("another codexm run or session synchronization is using this project or profile; wait for it to exit")
	}
	return sessionLeaseLock{handle: lock}, nil
}
func (lock sessionLeaseLock) close() error   { return lock.handle.Unlock() }
func (lock sessionLeaseLock) file() *os.File { return nil }
