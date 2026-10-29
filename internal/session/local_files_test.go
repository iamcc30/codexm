package session

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLocalIgnorePreservesRulesAndIsIdempotent(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, projectDirName, ".gitignore")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("custom-private-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ensureLocalIgnores(root); err != nil {
		t.Fatal(err)
	}
	first, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(first), "custom-private-file\n") || !strings.Contains(string(first), "/pending-transaction.json\n") {
		t.Fatalf("unexpected ignore rules: %s", first)
	}
	if err := ensureLocalIgnores(root); err != nil {
		t.Fatal(err)
	}
	second, _ := os.ReadFile(path)
	if string(first) != string(second) {
		t.Fatal("local ignore rules are not idempotent")
	}
}
