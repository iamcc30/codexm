package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEnsureProfileHomeUsesFileCredentialStore(t *testing.T) {
	home := filepath.Join(t.TempDir(), "profile")
	if err := EnsureProfileHome(home, "file"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `cli_auth_credentials_store = "file"`) {
		t.Fatalf("missing file credential store: %s", data)
	}
}

func TestEnsureProfileHomePreservesOtherConfig(t *testing.T) {
	home := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	original := "model = \"gpt-test\"\n\n[features]\nshell_snapshot = true\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProfileHome(home, "file"); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, `model = "gpt-test"`) || !strings.Contains(text, "[features]") {
		t.Fatalf("existing settings were not preserved: %s", text)
	}
	if !strings.Contains(text, `cli_auth_credentials_store = "file"`) {
		t.Fatalf("credential store was not added: %s", text)
	}
}

func TestEnvWithCodexHomeReplacesExistingValue(t *testing.T) {
	env := EnvWithCodexHome([]string{"A=1", "CODEX_HOME=/old"}, "/new")
	joined := strings.Join(env, "\n")
	if strings.Contains(joined, "CODEX_HOME=/old") {
		t.Fatalf("old CODEX_HOME was retained: %v", env)
	}
	if !strings.Contains(joined, "CODEX_HOME=/new") {
		t.Fatalf("new CODEX_HOME missing: %v", env)
	}
}
