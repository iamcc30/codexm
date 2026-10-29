package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/iamcc30/codexm/internal/appserver"
	"github.com/iamcc30/codexm/internal/config"
	"github.com/iamcc30/codexm/internal/session"
)

func TestSessionRunHoldsWriterLeaseUntilNativeExit(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell-script native writer is Unix-specific")
	}
	base := t.TempDir()
	root := filepath.Join(base, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEXM_HOME", filepath.Join(base, "manager"))
	t.Setenv("CODEXM_PROFILES_HOME", filepath.Join(base, "profiles"))
	ready, release := filepath.Join(base, "ready"), filepath.Join(base, "release")
	t.Setenv("LEASE_READY", ready)
	t.Setenv("LEASE_RELEASE", release)
	fake := filepath.Join(base, "codex")
	script := `#!/bin/sh
set -eu
if [ "${1-}" = "app-server" ] || [ "${1-}" = "--help" ]; then
  echo 'unexpected managed invocation' >&2
  exit 99
fi
: > "$LEASE_READY"
while [ ! -f "$LEASE_RELEASE" ]; do sleep 0.05; done
`
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEXM_CODEX_BIN", fake)
	var out, errOut bytes.Buffer
	app := New("test")
	app.Out, app.Err = &out, &errOut
	for _, args := range [][]string{{"add", "first"}, {"add", "second"}, {"session", "init", "--project", root}} {
		if code := app.Run(args); code != 0 {
			t.Fatalf("setup %v: %s", args, errOut.String())
		}
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	manager, err := config.ManagerConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() { done <- app.Run([]string{"run", "--project", root, "first"}) }()
	defer func() {
		_ = os.WriteFile(release, nil, 0o600)
		select {
		case <-done:
		case <-time.After(10 * time.Second):
		}
	}()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case code := <-done:
			t.Fatalf("native run exited before ready: %d %s", code, errOut.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatal("native writer did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	for _, name := range []string{"first", "second"} {
		_, err := session.Sync(session.Options{ProjectRoot: root, ProfileHome: cfg.Profiles[name].CodexHome, ManagerHome: manager})
		if err == nil || !strings.Contains(err.Error(), "another codexm run") {
			t.Fatalf("sync %s was not excluded: %v", name, err)
		}
	}
	var daemonOut, daemonErr bytes.Buffer
	daemonApp := New("test")
	daemonApp.Out, daemonApp.Err = &daemonOut, &daemonErr
	if code := daemonApp.Run([]string{"daemon", "start", "first"}); code == 0 || !strings.Contains(daemonErr.String(), "another codexm run") {
		t.Fatalf("daemon start raced active native writer: code=%d error=%s", code, daemonErr.String())
	}
	if lease, err := session.AcquireLease("", cfg.Profiles["first"].CodexHome); err == nil {
		lease.Release()
		t.Fatal("native profile lease was released early")
	}
	if err := os.WriteFile(release, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("native run failed: %d %s", code, errOut.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("native run did not finish")
	}
	// The defer must not wait for a second result after this successful receive.
	done <- 0
	if !strings.Contains(out.String(), "direct native task") {
		t.Fatalf("session run used persistent daemon: %s", out.String())
	}
	if _, err := session.Sync(session.Options{ProjectRoot: root, ProfileHome: cfg.Profiles["first"].CodexHome, ManagerHome: manager}); err != nil {
		t.Fatalf("lease remained after native exit: %v", err)
	}
}

func TestSessionDaemonGuardFailsClosedOnMalformedState(t *testing.T) {
	home := t.TempDir()
	if err := rejectSessionDaemon(home, "test"); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "runtime", "app-server", "test", "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectSessionDaemon(home, "test"); err == nil {
		t.Fatal("malformed daemon state accepted")
	}
}

func TestSessionDaemonGuardRejectsRunningUnhealthyDaemon(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, "runtime", "app-server", "test", "state.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Use the test process as a live PID, with an unreachable health endpoint.
	// The guard must reject it without sending a termination signal.
	data, err := json.Marshal(appserver.State{Profile: "test", PID: os.Getpid(), Endpoint: "ws://127.0.0.1:1", TokenFile: "unused"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := rejectSessionDaemon(home, "test"); err == nil || !strings.Contains(err.Error(), "running managed app-server") {
		t.Fatalf("live unhealthy daemon accepted: %v", err)
	}
}
