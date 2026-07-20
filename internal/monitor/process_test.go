package monitor

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/iamcc30/codexm/internal/appserver"
	"github.com/iamcc30/codexm/internal/config"
)

func TestManagedRemoteProcessIsNotClassifiedAsUnmanaged(t *testing.T) {
	if !managedRemoteProcess([]string{"PATH=/bin", appserver.ManagedRemoteEnv + "=1"}) {
		t.Fatal("managed remote marker was not recognized")
	}
	if managedRemoteProcess([]string{appserver.ManagedRemoteEnv + "=0"}) {
		t.Fatal("disabled managed marker was accepted")
	}
}

func TestScannerFindsLegacyProcessAndExcludesManagedRemoteClient(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("executable-name and process environment inspection is platform-specific")
	}
	executable := filepath.Join(t.TempDir(), "codex")
	data, err := os.ReadFile(os.Args[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, data, 0o700); err != nil {
		t.Fatal(err)
	}
	profileHome := t.TempDir()
	start := func(managed bool) *exec.Cmd {
		cmd := exec.Command(executable, "-test.run=TestUnmanagedScannerHelperProcess", "--")
		cmd.Env = append(os.Environ(),
			"GO_WANT_CODEXM_SCANNER_HELPER=1",
			"CODEX_HOME="+profileHome,
		)
		if managed {
			cmd.Env = append(cmd.Env, appserver.ManagedRemoteEnv+"=1")
		}
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		})
		return cmd
	}
	legacy := start(false)
	managed := start(true)
	cfg := config.New()
	cfg.Profiles["test"] = config.Profile{CodexHome: profileHome}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tasks := scanUnmanaged(cfg, nil)
		foundLegacy, foundManaged := false, false
		for _, task := range tasks {
			foundLegacy = foundLegacy || task.PID == legacy.Process.Pid
			foundManaged = foundManaged || task.PID == managed.Process.Pid
		}
		if foundLegacy {
			if foundManaged {
				t.Fatal("managed remote client was reported as unmanaged")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("legacy Codex process was not discovered")
}

func TestUnmanagedScannerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CODEXM_SCANNER_HELPER") != "1" {
		return
	}
	select {}
}

func TestEnvironmentAssignmentPositionHandlesSpacesAndBoundaries(t *testing.T) {
	raw := "/tmp/codex prompt CODEX_HOME=/wrong CODEX_HOME=/Users/test/Application Support/codex profile=1"
	if got := environmentAssignmentPosition(raw, "CODEX_HOME", "/Users/test/Application Support/codex"); got < 0 {
		t.Fatal("path containing spaces was not matched")
	}
	if got := environmentAssignmentPosition(raw, "CODEX_HOME", "/Users/test/Application Support/codex-other"); got >= 0 {
		t.Fatal("partial environment value was matched")
	}
}
