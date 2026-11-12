package session

import (
	"errors"
	"os"
	"syscall"
)

type sessionLeaseLock struct{ handle *os.File }

func lockSessionLease(path string) (sessionLeaseLock, error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_CLOEXEC|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return sessionLeaseLock{}, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return sessionLeaseLock{}, err
	}
	stat := info.Sys().(*syscall.Stat_t)
	if !info.Mode().IsRegular() || stat.Nlink != 1 || stat.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 {
		file.Close()
		return sessionLeaseLock{}, errors.New("unsafe session writer lock identity or permissions")
	}
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return sessionLeaseLock{}, errors.New("another codexm run or session synchronization is using this project or profile; wait for it to exit")
		}
		return sessionLeaseLock{}, err
	}
	return sessionLeaseLock{handle: file}, nil
}

// Closing rather than LOCK_UN preserves ownership inherited by an orphan child.
func (lock sessionLeaseLock) close() error   { return lock.handle.Close() }
func (lock sessionLeaseLock) file() *os.File { return lock.handle }
