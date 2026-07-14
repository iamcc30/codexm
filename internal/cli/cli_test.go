package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codexm/internal/config"
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
}
