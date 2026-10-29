package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestWriterLeaseExcludesProjectAndProfileAcrossManagers(t *testing.T) {
	root, otherRoot, home, otherHome := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	lease, err := AcquireLease(root, home)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	for _, paths := range [][2]string{{root, otherHome}, {otherRoot, home}, {root, home}, {"", home}} {
		acquired, err := AcquireLease(paths[0], paths[1])
		if err == nil {
			acquired.Release()
			t.Fatalf("overlapping writer accepted: %v", paths)
		}
		if !strings.Contains(err.Error(), "another codexm run") {
			t.Fatal(err)
		}
	}
	independent, err := AcquireLease(otherRoot, otherHome)
	if err != nil {
		t.Fatalf("failed partial acquisition leaked lock: %v", err)
	}
	independent.Release()
	if release, err := acquireOperationLease(root, home, lease); err != nil {
		t.Fatal(err)
	} else {
		release()
	}
	if _, err := acquireOperationLease(root, otherHome, lease); err == nil {
		t.Fatal("accepted mismatched lease")
	}
	lease.Release()
	lease.Release()
	if _, err := acquireOperationLease(root, home, lease); err == nil {
		t.Fatal("accepted released lease")
	}
	replacement, err := AcquireLease(root, home)
	if err != nil {
		t.Fatal(err)
	}
	replacement.Release()
}

func TestWriterLeaseAliasesAndUnsafeFiles(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges")
	}
	base, root, home := t.TempDir(), t.TempDir(), t.TempDir()
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLease(root, home)
	if err != nil {
		t.Fatal(err)
	}
	if other, err := AcquireLease("", alias); err == nil {
		other.Release()
		t.Fatal("profile alias bypassed lease")
	}
	lease.Release()
	path := filepath.Join(home, ".codexm-session-writer.lock")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside"), path); err != nil {
		t.Fatal(err)
	}
	if other, err := AcquireLease(root, home); err == nil {
		other.Release()
		t.Fatal("accepted symlink lock")
	}
}

func TestWriterLeaseReleasedWhenOwnerProcessDies(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	ready := filepath.Join(t.TempDir(), "ready")
	cmd := exec.Command(os.Args[0], "-test.run=^TestWriterLeaseHelperProcess$")
	cmd.Env = append(os.Environ(), "CODEXM_LEASE_TEST_ROOT="+root, "CODEXM_LEASE_TEST_HOME="+home, "CODEXM_LEASE_TEST_READY="+ready)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("lease helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if acquired, err := AcquireLease(root, home); err == nil {
		acquired.Release()
		t.Fatal("cross-process lease was ignored")
	}
	if err := cmd.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = cmd.Wait()
	acquired, err := AcquireLease(root, home)
	if err != nil {
		t.Fatalf("crashed owner left stale lease: %v", err)
	}
	acquired.Release()
}

func TestWriterLeaseHelperProcess(t *testing.T) {
	root := os.Getenv("CODEXM_LEASE_TEST_ROOT")
	if root == "" {
		return
	}
	lease, err := AcquireLease(root, os.Getenv("CODEXM_LEASE_TEST_HOME"))
	if err != nil {
		os.Exit(2)
	}
	defer lease.Release()
	if err := os.WriteFile(os.Getenv("CODEXM_LEASE_TEST_READY"), []byte("ready"), 0o600); err != nil {
		os.Exit(3)
	}
	for {
		time.Sleep(time.Hour)
	}
}
