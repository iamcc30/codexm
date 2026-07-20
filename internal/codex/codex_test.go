package codex

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunnerReturnsInterruptedChildExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix signal behavior")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-codex")
	content := "#!/bin/sh\ntrap 'exit 130' INT\nkill -INT $$\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	runner := &Runner{Executable: script}
	err := runner.Run(dir, dir, nil)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 130 {
		t.Fatalf("unexpected interrupt result: %v", err)
	}
}

func TestRunnerNormalizesDirectSignalExitCode(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix signal behavior")
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-codex")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nkill -TERM $$\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	err := (&Runner{Executable: script}).Run(dir, dir, nil)
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Code != 143 {
		t.Fatalf("direct signal exit was not normalized: %v", err)
	}
}

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
	if !strings.Contains(string(data), `mcp_oauth_credentials_store = "file"`) {
		t.Fatalf("missing MCP OAuth file credential store: %s", data)
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
	if !strings.Contains(text, `mcp_oauth_credentials_store = "file"`) {
		t.Fatalf("MCP OAuth credential store was not added: %s", text)
	}
}

func TestEnsureProfileHomeUpdatesBothCredentialStores(t *testing.T) {
	home := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	original := "cli_auth_credentials_store = \"auto\"\nmcp_oauth_credentials_store = \"keyring\"\n\n[features]\nshell_snapshot = true\n"
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
	if strings.Count(text, `cli_auth_credentials_store = "file"`) != 1 || strings.Count(text, `mcp_oauth_credentials_store = "file"`) != 1 {
		t.Fatalf("credential stores were not updated once each: %s", text)
	}
}

func TestEnsureMCPOAuthCredentialStoreMigratesExistingProfile(t *testing.T) {
	home := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	if err := os.WriteFile(path, []byte("cli_auth_credentials_store = \"keyring\" # explicit choice\n\n[features]\nshell_snapshot = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMCPOAuthCredentialStore(home); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `mcp_oauth_credentials_store = "keyring"`) {
		t.Fatalf("MCP OAuth store did not follow the existing CLI store: %s", data)
	}
}

func TestEnsureMCPOAuthCredentialStoreMigratesQuotedCLIKey(t *testing.T) {
	home := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	original := `"cli_auth_credentials_store" = "keyring"` + "\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMCPOAuthCredentialStore(home); err != nil {
		t.Fatal(err)
	}
	stores, err := ReadCredentialStores(path)
	if err != nil {
		t.Fatalf("migration produced invalid TOML: %v\n%s", err, mustReadFile(t, path))
	}
	if stores.CLI != "keyring" || stores.MCPOAuth != "keyring" {
		t.Fatalf("unexpected credential stores: %+v\n%s", stores, mustReadFile(t, path))
	}
}

func TestEnsureMCPOAuthCredentialStoreRecognizesQuotedMCPKey(t *testing.T) {
	home := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	original := `'cli_auth_credentials_store' = "keyring"` + "\n" + `"mcp_oauth_credentials_store" = "file"` + "\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMCPOAuthCredentialStore(home); err != nil {
		t.Fatal(err)
	}
	if got := mustReadFile(t, path); got != original {
		t.Fatalf("explicit quoted MCP setting was changed:\n%s", got)
	}
}

func TestEnsureProfileHomeUpdatesQuotedCredentialKeys(t *testing.T) {
	home := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	original := `"cli_auth_credentials_store" = "auto"` + "\n" + `'mcp_oauth_credentials_store' = "keyring"` + "\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureProfileHome(home, "file"); err != nil {
		t.Fatal(err)
	}
	stores, err := ReadCredentialStores(path)
	if err != nil {
		t.Fatalf("update produced invalid TOML: %v\n%s", err, mustReadFile(t, path))
	}
	if stores.CLI != "file" || stores.MCPOAuth != "file" {
		t.Fatalf("unexpected credential stores: %+v\n%s", stores, mustReadFile(t, path))
	}
}

func TestEnsureMCPOAuthCredentialStorePreservesExplicitOverride(t *testing.T) {
	home := filepath.Join(t.TempDir(), "profile")
	if err := os.MkdirAll(home, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "config.toml")
	original := "cli_auth_credentials_store = \"file\"\nmcp_oauth_credentials_store = \"auto\"\n"
	if err := os.WriteFile(path, []byte(original), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureMCPOAuthCredentialStore(home); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != original {
		t.Fatalf("explicit MCP OAuth store was changed:\n%s", data)
	}
}

func TestReadCredentialStoresIgnoresCommentsAndStrings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	content := `mcp_oauth_credentials_store = "auto"
model_instructions_file = 'mcp_oauth_credentials_store = "file"'
# mcp_oauth_credentials_store = "file"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	stores, err := ReadCredentialStores(path)
	if err != nil {
		t.Fatal(err)
	}
	if stores.MCPOAuth != "auto" {
		t.Fatalf("comment or string caused a false result: %+v", stores)
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

func mustReadFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}
