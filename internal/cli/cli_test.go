package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamcc30/codexm/internal/config"
)

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
