package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/iamcc30/codexm/internal/appserver"
	"github.com/iamcc30/codexm/internal/codex"
	"github.com/iamcc30/codexm/internal/config"
	"github.com/iamcc30/codexm/internal/session"
)

func TestNativeMaintenanceSerializesFirstOptInWithLaunchRoutes(t *testing.T) {
	base := t.TempDir()
	t.Setenv("CODEXM_HOME", filepath.Join(base, "manager"))
	t.Setenv("CODEXM_PROFILES_HOME", filepath.Join(base, "profiles"))
	var out, errOut bytes.Buffer
	app := New("test")
	app.Out = &out
	app.Err = &errOut
	if code := app.Run([]string{"add", "first"}); code != 0 {
		t.Fatal(errOut.String())
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	home := cfg.Profiles["first"].CodexHome
	lease, err := session.AcquireLease("", home)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	// The marker has not yet been installed: the shared lease must already exclude
	// every route, so none can pass a policy check just before first opt-in.
	if code := app.Run([]string{"shell", "first"}); code == 0 || !strings.Contains(errOut.String(), "another codexm") {
		t.Fatal(code, errOut.String())
	}
	errOut.Reset()
	if code := app.runAndCode(&codex.Runner{Executable: filepath.Join(base, "never-execute")}, home, "", []string{"login"}); code == 0 || !strings.Contains(errOut.String(), "another codexm") {
		t.Fatal(code, errOut.String())
	}
	manager := &appserver.Manager{Home: filepath.Join(base, "manager"), Executable: filepath.Join(base, "never-execute")}
	if _, err := manager.Start(context.Background(), "first", home); err == nil || !strings.Contains(err.Error(), "another codexm") {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(base, "manager", "runtime")); !os.IsNotExist(err) {
		t.Fatal("start mutated runtime before obtaining profile lease", err)
	}
}
