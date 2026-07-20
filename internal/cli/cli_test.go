package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/iamcc30/codexm/internal/appserver"
	"github.com/iamcc30/codexm/internal/config"
)

const cliTestSessionID = "33333333-3333-4333-8333-333333333333"

func TestAddBindAndResolve(t *testing.T) {
	managerHome := filepath.Join(t.TempDir(), "manager")
	profilesHome := filepath.Join(t.TempDir(), "profiles")
	project := filepath.Join(t.TempDir(), "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEXM_HOME", managerHome)
	t.Setenv("CODEXM_PROFILES_HOME", profilesHome)

	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("test")
	app.Out = &out
	app.Err = &errOut

	if code := app.Run([]string{"add", "--bind", project, "account1"}); code != 0 {
		t.Fatalf("add failed (%d): %s", code, errOut.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Profiles["account1"]; !ok {
		t.Fatal("profile not saved")
	}
	profile, _, ok := config.ResolveProfile(cfg, project)
	if !ok || profile != "account1" {
		t.Fatalf("binding not resolved: %q %t", profile, ok)
	}
	configToml := filepath.Join(cfg.Profiles["account1"].CodexHome, "config.toml")
	data, err := os.ReadFile(configToml)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `cli_auth_credentials_store = "file"`) {
		t.Fatalf("profile is not isolated: %s", data)
	}
	if !strings.Contains(string(data), `mcp_oauth_credentials_store = "file"`) {
		t.Fatalf("MCP OAuth credentials are not isolated: %s", data)
	}
}

func TestMCPSyncAndExclusion(t *testing.T) {
	managerHome := filepath.Join(t.TempDir(), "manager")
	profilesHome := filepath.Join(t.TempDir(), "profiles")
	t.Setenv("CODEXM_HOME", managerHome)
	t.Setenv("CODEXM_PROFILES_HOME", profilesHome)

	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("test")
	app.Out = &out
	app.Err = &errOut
	for _, name := range []string{"work", "personal"} {
		if code := app.Run([]string{"add", name}); code != 0 {
			t.Fatalf("add %s failed (%d): %s", name, code, errOut.String())
		}
	}
	sharedPath, err := config.SharedMCPConfigPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(sharedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	shared := "[mcp_servers.docs]\nurl = \"https://docs.example/mcp\"\n\n[mcp_servers.prod]\ncommand = \"prod-mcp\"\n"
	if err := os.WriteFile(sharedPath, []byte(shared), 0o600); err != nil {
		t.Fatal(err)
	}

	if code := app.Run([]string{"mcp", "sync", "--all"}); code != 0 {
		t.Fatalf("sync failed (%d): %s", code, errOut.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"work", "personal"} {
		data, err := os.ReadFile(filepath.Join(cfg.Profiles[name].CodexHome, "config.toml"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(data), "[mcp_servers.docs]") || !strings.Contains(string(data), "[mcp_servers.prod]") {
			t.Fatalf("%s was not synchronized: %s", name, data)
		}
	}

	if code := app.Run([]string{"mcp", "exclude", "personal", "prod"}); code != 0 {
		t.Fatalf("exclude failed (%d): %s", code, errOut.String())
	}
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	personalData, err := os.ReadFile(filepath.Join(cfg.Profiles["personal"].CodexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(personalData), "[mcp_servers.prod]") {
		t.Fatalf("excluded server remains in profile: %s", personalData)
	}
	if len(cfg.Profiles["personal"].ExcludedMCPServers) != 1 || cfg.Profiles["personal"].ExcludedMCPServers[0] != "prod" {
		t.Fatalf("exclusion was not persisted: %+v", cfg.Profiles["personal"])
	}

	if code := app.Run([]string{"mcp", "include", "personal", "prod"}); code != 0 {
		t.Fatalf("include failed (%d): %s", code, errOut.String())
	}
	cfg, err = config.Load()
	if err != nil {
		t.Fatal(err)
	}
	personalData, err = os.ReadFile(filepath.Join(cfg.Profiles["personal"].CodexHome, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(personalData), "[mcp_servers.prod]") {
		t.Fatalf("included server was not restored: %s", personalData)
	}
	if len(cfg.Profiles["personal"].ExcludedMCPServers) != 0 {
		t.Fatalf("exclusion was not removed: %+v", cfg.Profiles["personal"])
	}
}

func TestSessionRunSyncsAfterNonzeroExitAndConflictBlocksLaunch(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake Codex is Unix-specific")
	}
	base := t.TempDir()
	managerHome := filepath.Join(base, "manager")
	profilesHome := filepath.Join(base, "profiles")
	project := filepath.Join(base, "project")
	marker := filepath.Join(base, "codex-ran")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEXM_HOME", managerHome)
	t.Setenv("CODEXM_PROFILES_HOME", profilesHome)
	t.Setenv("FAKE_CODEX_MARKER", marker)

	fakeCodex := filepath.Join(base, "fake-codex")
	script := fmt.Sprintf(`#!/bin/sh
set -eu
mkdir -p "$CODEX_HOME/sessions/2026/07/18"
cwd=$(pwd)
session="$CODEX_HOME/sessions/2026/07/18/rollout-2026-07-18T12-00-00-%s.jsonl"
printf '{"timestamp":"2026-07-18T12:00:00Z","type":"session_meta","payload":{"id":"%s","session_id":"%s","timestamp":"2026-07-18T12:00:00Z","cwd":"%%s"}}\n' "$cwd" > "$session"
: > "$FAKE_CODEX_MARKER"
exit "${FAKE_CODEX_EXIT:-0}"
`, cliTestSessionID, cliTestSessionID, cliTestSessionID)
	if err := os.WriteFile(fakeCodex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEXM_CODEX_BIN", fakeCodex)

	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("test")
	app.Out = &out
	app.Err = &errOut
	if code := app.Run([]string{"add", "--bind", project, "team"}); code != 0 {
		t.Fatalf("add failed (%d): %s", code, errOut.String())
	}
	if code := app.Run([]string{"session", "init", "--project", project}); code != 0 {
		t.Fatalf("session init failed (%d): %s", code, errOut.String())
	}

	t.Setenv("FAKE_CODEX_EXIT", "7")
	if code := app.Run([]string{"run", "--project", project}); code != 7 {
		t.Fatalf("Codex exit code was not preserved: %d, stderr=%s", code, errOut.String())
	}
	metadata := filepath.Join(project, ".codexm", "metadata", cliTestSessionID+".json")
	if _, err := os.Stat(metadata); err != nil {
		t.Fatalf("post-run sync did not export session after nonzero exit: %v", err)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	profilePath := filepath.Join(cfg.Profiles["team"].CodexHome, "sessions", "2026", "07", "18",
		"rollout-2026-07-18T12-00-00-"+cliTestSessionID+".jsonl")
	baseline, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendCLIJSONLine(profilePath, `{"type":"event_msg","payload":{"message":"project branch"}}`); err != nil {
		t.Fatal(err)
	}
	if code := app.Run([]string{"session", "sync", "--project", project}); code != 0 {
		t.Fatalf("syncing first append failed (%d): %s", code, errOut.String())
	}
	diverged := append(append([]byte(nil), baseline...), []byte("{\"type\":\"event_msg\",\"payload\":{\"message\":\"profile branch\"}}\n")...)
	if err := os.WriteFile(profilePath, diverged, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_CODEX_EXIT", "0")
	if code := app.Run([]string{"run", "--project", project}); code == 0 {
		t.Fatalf("conflicted run unexpectedly succeeded: stderr=%s", errOut.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("Codex launched despite session conflict: %v", err)
	}
	if !strings.Contains(errOut.String(), "session conflict") {
		t.Fatalf("conflict was not explained: %s", errOut.String())
	}
}

func TestManagedRunInjectsRemoteEnvironmentPreservesExitAndUnmanagedEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake Codex is Unix-specific")
	}
	base := t.TempDir()
	managerHome := filepath.Join(base, "manager")
	profilesHome := filepath.Join(base, "profiles")
	project := filepath.Join(base, "project")
	record := filepath.Join(base, "record")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	fakeCodex := filepath.Join(base, "codex")
	script := `#!/bin/sh
set -eu
if [ "${1-}" = "app-server" ] && [ "${2-}" = "--help" ]; then
  printf '%s\n' 'app-server help'
  exit 0
fi
if [ "${1-}" = "--help" ]; then
  printf '%s\n' '--remote ENDPOINT --remote-auth-token-env ENV'
  exit 0
fi
if [ "${1-}" = "--version" ]; then
  printf '%s\n' 'codex-cli 999.0-test'
  exit 0
fi
if [ "${1-}" = "app-server" ]; then
  exec "$FAKE_CODEX_TEST_BINARY" -test.run=TestCLIManagedAppServerHelperProcess -- "$@"
fi
{
  printf 'args='
  printf '<%s>' "$@"
  printf '\nmanaged=%s\ntoken_set=%s\n' "${CODEXM_MANAGED_REMOTE-}" "${CODEXM_APP_SERVER_TOKEN:+yes}"
} > "$FAKE_CODEX_RECORD"
exit "${FAKE_CODEX_EXIT:-0}"
`
	if err := os.WriteFile(fakeCodex, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEXM_HOME", managerHome)
	t.Setenv("CODEXM_PROFILES_HOME", profilesHome)
	t.Setenv("CODEXM_CODEX_BIN", fakeCodex)
	t.Setenv("FAKE_CODEX_TEST_BINARY", os.Args[0])
	t.Setenv("FAKE_CODEX_RECORD", record)
	t.Setenv("GO_WANT_CODEXM_CLI_APP_HELPER", "1")

	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("test")
	app.Out, app.Err = &out, &errOut
	if code := app.Run([]string{"add", "--bind", project, "team"}); code != 0 {
		t.Fatalf("add failed (%d): %s", code, errOut.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := appserver.NewManager(managerHome)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Stop(context.Background(), "team", true) })

	t.Setenv("FAKE_CODEX_EXIT", "7")
	if code := app.Run([]string{"run", "--project", project, "team", "--", "resume", "--last"}); code != 7 {
		t.Fatalf("managed run exit=%d stderr=%s", code, errOut.String())
	}
	managedRecord, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	managedText := string(managedRecord)
	for _, expected := range []string{
		"<--remote>", "<--remote-auth-token-env>", "<CODEXM_APP_SERVER_TOKEN>",
		"<resume><--last>", "managed=1", "token_set=yes",
	} {
		if !strings.Contains(managedText, expected) {
			t.Fatalf("managed child record is missing %q:\n%s", expected, managedText)
		}
	}
	if strings.Contains(managedText, "Bearer ") {
		t.Fatalf("capability token leaked into recorded arguments:\n%s", managedText)
	}

	t.Setenv("FAKE_CODEX_EXIT", "0")
	if code := app.Run([]string{"run", "--unmanaged", "--project", project, "team", "--", "resume", "--last"}); code != 0 {
		t.Fatalf("unmanaged escape failed (%d): %s", code, errOut.String())
	}
	unmanagedRecord, err := os.ReadFile(record)
	if err != nil {
		t.Fatal(err)
	}
	unmanagedText := string(unmanagedRecord)
	if strings.Contains(unmanagedText, "<--remote>") || !strings.Contains(unmanagedText, "managed=") ||
		strings.Contains(unmanagedText, "managed=1") || !strings.Contains(unmanagedText, "token_set=") {
		t.Fatalf("unmanaged child inherited managed state:\n%s", unmanagedText)
	}
	if cfg.Profiles["team"].CodexHome == "" {
		t.Fatal("test profile was not configured")
	}
}

func TestCLIManagedAppServerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CODEXM_CLI_APP_HELPER") != "1" {
		return
	}
	listen := ""
	for i, arg := range os.Args {
		if arg == "--listen" && i+1 < len(os.Args) {
			listen = strings.TrimPrefix(os.Args[i+1], "ws://")
			break
		}
	}
	if listen == "" {
		os.Exit(2)
	}
	server := &http.Server{Addr: listen, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	})}
	if err := server.ListenAndServe(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func TestSessionShellWarnsAndUninitializedRunRemainsCompatible(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script fake executables are Unix-specific")
	}
	base := t.TempDir()
	managerHome := filepath.Join(base, "manager")
	profilesHome := filepath.Join(base, "profiles")
	project := filepath.Join(base, "plain-project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEXM_HOME", managerHome)
	t.Setenv("CODEXM_PROFILES_HOME", profilesHome)
	exitZero := filepath.Join(base, "exit-zero")
	if err := os.WriteFile(exitZero, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEXM_CODEX_BIN", exitZero)

	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("test")
	app.Out = &out
	app.Err = &errOut
	if code := app.Run([]string{"add", "--bind", project, "team"}); code != 0 {
		t.Fatal(errOut.String())
	}
	if code := app.Run([]string{"run", "--project", project}); code != 0 {
		t.Fatalf("uninitialized project changed existing run behavior: %d %s", code, errOut.String())
	}
	t.Setenv("SHELL", exitZero)
	if code := app.Run([]string{"shell", "team"}); code != 0 {
		t.Fatalf("shell failed: %d %s", code, errOut.String())
	}
	if !strings.Contains(out.String(), "bypasses automatic project session synchronization") {
		t.Fatalf("shell bypass warning missing: %s", out.String())
	}
}

func TestSessionStatusDoesNotClaimSuccessAfterScanError(t *testing.T) {
	base := t.TempDir()
	managerHome := filepath.Join(base, "manager")
	profilesHome := filepath.Join(base, "profiles")
	project := filepath.Join(base, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEXM_HOME", managerHome)
	t.Setenv("CODEXM_PROFILES_HOME", profilesHome)

	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("test")
	app.Out = &out
	app.Err = &errOut
	if code := app.Run([]string{"add", "--bind", project, "team"}); code != 0 {
		t.Fatal(errOut.String())
	}
	if code := app.Run([]string{"session", "init", "--project", project}); code != 0 {
		t.Fatal(errOut.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	broken := filepath.Join(cfg.Profiles["team"].CodexHome, "sessions", "2026", "07", "18",
		"rollout-2026-07-18T12-00-00-"+cliTestSessionID+".jsonl")
	if err := os.MkdirAll(filepath.Dir(broken), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(broken, []byte(`{"type":"session_meta"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	errOut.Reset()
	if code := app.Run([]string{"session", "status", "--project", project}); code == 0 {
		t.Fatal("status unexpectedly succeeded")
	}
	if strings.Contains(out.String(), "no changes pending") {
		t.Fatalf("status claimed success after an error: %s", out.String())
	}
}

func TestSessionAuditJSONForCleanMirror(t *testing.T) {
	project := t.TempDir()
	t.Setenv("CODEXM_HOME", t.TempDir())
	var out bytes.Buffer
	var errOut bytes.Buffer
	app := New("test")
	app.Out, app.Err = &out, &errOut
	if code := app.Run([]string{"session", "init", "--project", project}); code != 0 {
		t.Fatalf("init failed: %d %s", code, errOut.String())
	}
	out.Reset()
	if code := app.Run([]string{"session", "audit", "--project", project, "--json"}); code != 0 {
		t.Fatalf("audit failed: %d %s", code, errOut.String())
	}
	var result struct {
		Errors   int `json:"errors"`
		Warnings int `json:"warnings"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("invalid audit JSON: %v\n%s", err, out.String())
	}
	if result.Errors != 0 || result.Warnings != 0 {
		t.Fatalf("unexpected audit findings: %+v", result)
	}
}

func appendCLIJSONLine(path, line string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = file.WriteString(line + "\n")
	return err
}
