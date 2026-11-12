package session

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNativePendingBlocksOrdinaryProfileOperations(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	directory := filepath.Join(home, ".codexm-sync-v2")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "pending.json"), []byte("untrusted pending intent"), 0600); err != nil {
		t.Fatal(err)
	}
	if lease, err := AcquireLease(root, home); err == nil {
		lease.Release()
		t.Fatal("pending native intent admitted a launcher")
	}
	if err := os.Remove(filepath.Join(directory, "pending.json")); err != nil {
		t.Fatal(err)
	}
	lease, err := AcquireLease(root, home)
	if err != nil {
		t.Fatal("rejected lease leaked locks:", err)
	}
	lease.Release()
}

func TestNativeHistoryOptInIsFailClosed(t *testing.T) {
	home := t.TempDir()
	if managed, err := NativeHistoryManaged(home); err != nil || managed {
		t.Fatal(managed, err)
	}
	if err := os.WriteFile(filepath.Join(home, nativeManagedFile), []byte("unrecognized policy"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NativeHistoryManaged(home); err == nil {
		t.Fatal("unknown marker accepted")
	}
	if err := os.WriteFile(filepath.Join(home, nativeManagedFile), []byte("codexm offline native history v1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateNativeHistoryLaunch(home, t.TempDir(), "unused", []string{"-c", "sqlite_home='elsewhere'"}); err == nil {
		t.Fatal("native override accepted")
	}
	if runtime.GOOS != "linux" {
		if _, err := SyncNativeHistory(NativeHistoryOptions{}); err == nil || !strings.Contains(err.Error(), "Linux") {
			t.Fatal(err)
		}
	}
}
