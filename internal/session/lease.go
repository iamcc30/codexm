package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
)

// Lease excludes cooperating native runs and synchronization operations. Locks
// live beside the data, not in ManagerHome, so distinct manager configurations
// cannot concurrently replace the same rollout. It does not lock external Codex
// processes; stop those writers before synchronizing.
//
// Keep lock files in place: unlinking a locked file would let another process
// lock a different inode at the same path.
type Lease struct {
	mu       sync.Mutex
	project  string
	profile  string
	locks    []*flock.Flock
	released bool
}

// AcquireLease acquires a nonblocking project/profile lease. Either path can be
// empty for an operation that uses only the other resource.
func AcquireLease(projectRoot, profileHome string) (*Lease, error) {
	project, profile, err := leaseRoots(projectRoot, profileHome)
	if err != nil {
		return nil, err
	}
	if project == "" && profile == "" {
		return nil, errors.New("session lease requires a project or profile")
	}
	lease := &Lease{project: project, profile: profile}
	paths := []string{}
	if project != "" {
		paths = append(paths, filepath.Join(project, projectDirName, "session-writer.lock"))
	}
	if profile != "" {
		paths = append(paths, filepath.Join(profile, ".codexm-session-writer.lock"))
	}
	for _, path := range paths {
		if err := rejectSymlink(filepath.Dir(path)); err != nil {
			lease.Release()
			return nil, err
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			lease.Release()
			return nil, err
		}
		if err := rejectSymlink(path); err != nil {
			lease.Release()
			return nil, err
		}
		if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
			lease.Release()
			return nil, fmt.Errorf("refusing non-regular session lease %s", path)
		} else if err != nil && !os.IsNotExist(err) {
			lease.Release()
			return nil, err
		}
		lock := flock.New(path)
		locked, err := lock.TryLock()
		if err != nil {
			lease.Release()
			return nil, fmt.Errorf("acquire session writer lease: %w", err)
		}
		if !locked {
			lease.Release()
			return nil, errors.New("another codexm run or session synchronization is using this project or profile; wait for it to exit")
		}
		lease.locks = append(lease.locks, lock)
	}
	return lease, nil
}

func leaseRoots(projectRoot, profileHome string) (string, string, error) {
	project, profile := "", ""
	var err error
	if projectRoot != "" {
		project, err = normalizeExistingDir(projectRoot)
		if err != nil {
			return "", "", err
		}
	}
	if profileHome != "" {
		if err := os.MkdirAll(profileHome, 0o700); err != nil {
			return "", "", err
		}
		profile, err = normalizeExistingDir(profileHome)
		if err != nil {
			return "", "", err
		}
	}
	return project, profile, nil
}

// Release is idempotent. The operating system also releases the locks if the
// process dies; no stale PID-file removal is needed.
func (lease *Lease) Release() {
	if lease == nil {
		return
	}
	lease.mu.Lock()
	defer lease.mu.Unlock()
	if lease.released {
		return
	}
	for i := len(lease.locks) - 1; i >= 0; i-- {
		_ = lease.locks[i].Unlock()
	}
	lease.released = true
}

func acquireOperationLease(projectRoot, profileHome string, held *Lease) (func(), error) {
	if held == nil {
		lease, err := AcquireLease(projectRoot, profileHome)
		if err != nil {
			return nil, err
		}
		return lease.Release, nil
	}
	project, profile, err := leaseRoots(projectRoot, profileHome)
	if err != nil {
		return nil, err
	}
	held.mu.Lock()
	defer held.mu.Unlock()
	if held.released || held.project != project || held.profile != profile {
		return nil, errors.New("session writer lease is released or belongs to a different project/profile")
	}
	return func() {}, nil
}
