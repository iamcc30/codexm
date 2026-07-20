package appserver

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gofrs/flock"
	"github.com/gorilla/websocket"
	gprocess "github.com/shirou/gopsutil/v4/process"
)

func TestManagerStartIsIdempotentUsesPrivateFilesAndStops(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper process signal behavior is platform-specific")
	}
	t.Setenv("GO_WANT_CODEXM_APP_SERVER_HELPER", "1")
	home := t.TempDir()
	profileHome := filepath.Join(home, "profile")
	if err := os.MkdirAll(profileHome, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		Home: home, Executable: os.Args[0], WaitReady: 3 * time.Second,
		argsPrefix: []string{"-test.run=TestManagerHelperProcess", "--"},
	}
	first, err := manager.Start(context.Background(), "test", profileHome)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background(), "test", true)
	second, err := manager.Start(context.Background(), "test", profileHome)
	if err != nil {
		t.Fatal(err)
	}
	if first.PID != second.PID || first.Endpoint != second.Endpoint {
		t.Fatalf("idempotent start launched a second process: first=%+v second=%+v", first, second)
	}
	for _, path := range []string{first.TokenFile, manager.statePath("test"), first.LogFile} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s permissions = %o", path, info.Mode().Perm())
		}
	}
	if err := manager.Stop(context.Background(), "test", true); err != nil {
		t.Fatal(err)
	}
	if health := manager.Status(context.Background(), "test"); health.Running || health.Error != "" {
		t.Fatalf("stopped service still has runtime state: %+v", health)
	}
}

func TestManagerRejectsConcurrentOperation(t *testing.T) {
	home := t.TempDir()
	manager := &Manager{Home: home, Executable: os.Args[0]}
	dir := manager.profileDir("test")
	if err := secureDir(dir); err != nil {
		t.Fatal(err)
	}
	lock := flock.New(filepath.Join(dir, "manager.lock"))
	if err := lock.Lock(); err != nil {
		t.Fatal(err)
	}
	defer lock.Unlock()
	_, err := manager.Start(context.Background(), "test", filepath.Join(home, "profile"))
	if err == nil || !strings.Contains(err.Error(), "already in progress") {
		t.Fatalf("concurrent operation was not rejected: %v", err)
	}
}

func TestManagerPreservesUnhealthyRunningState(t *testing.T) {
	home := t.TempDir()
	manager := &Manager{Home: home, Executable: os.Args[0]}
	proc, err := gprocess.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	createdMillis, err := proc.CreateTime()
	if err != nil {
		t.Fatal(err)
	}
	state := State{
		Profile: "test", PID: os.Getpid(), Endpoint: "ws://127.0.0.1:1",
		TokenFile: filepath.Join(home, "missing-token"), StartedAt: time.UnixMilli(createdMillis),
	}
	if err := manager.writeState(state); err != nil {
		t.Fatal(err)
	}
	health, err := manager.Start(context.Background(), "test", filepath.Join(home, "profile"))
	if err == nil || !strings.Contains(err.Error(), "running but unhealthy") {
		t.Fatalf("unhealthy running daemon was not protected: health=%+v err=%v", health, err)
	}
	if !health.Running || health.Healthy {
		t.Fatalf("unexpected health for protected daemon: %+v", health)
	}
	got, err := manager.readState("test")
	if err != nil {
		t.Fatalf("protected runtime state was removed: %v", err)
	}
	if got.PID != state.PID || got.StartedAt.UnixMilli() != state.StartedAt.UnixMilli() {
		t.Fatalf("protected runtime state changed: got=%+v want=%+v", got, state)
	}
}

func TestManagerDoesNotSignalReusedStalePID(t *testing.T) {
	home := t.TempDir()
	manager := &Manager{Home: home, Executable: os.Args[0]}
	state := State{
		Profile: "test", PID: os.Getpid(), Endpoint: "ws://127.0.0.1:1",
		TokenFile: filepath.Join(home, "token"), StartedAt: time.Now().Add(-24 * time.Hour),
	}
	if err := manager.writeState(state); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), "test", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.statePath("test")); !os.IsNotExist(err) {
		t.Fatalf("stale state was not removed: %v", err)
	}
	// Reaching this line proves Stop did not signal the current test process.
}

func TestManagerRefusesUnverifiedNonForceStop(t *testing.T) {
	home := t.TempDir()
	manager := &Manager{Home: home, Executable: os.Args[0]}
	proc, err := gprocess.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	createdMillis, err := proc.CreateTime()
	if err != nil {
		t.Fatal(err)
	}
	state := State{
		Profile: "test", PID: os.Getpid(), Endpoint: "ws://127.0.0.1:1",
		TokenFile: filepath.Join(home, "missing-token"),
		StartedAt: time.UnixMilli(createdMillis),
	}
	if err := manager.writeState(state); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), "test", false); err == nil ||
		!strings.Contains(err.Error(), "cannot verify active threads") {
		t.Fatalf("unverified stop was not refused: %v", err)
	}
	if _, err := os.Stat(manager.statePath("test")); err != nil {
		t.Fatalf("refused stop removed runtime state: %v", err)
	}
	state.StartedAt = time.Now().Add(-24 * time.Hour)
	if err := manager.writeState(state); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), "test", true); err != nil {
		t.Fatal(err)
	}
}

func TestManagerReportsPortConflictWithoutLeavingState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper process signal behavior is platform-specific")
	}
	t.Setenv("GO_WANT_CODEXM_APP_SERVER_HELPER", "1")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	home := t.TempDir()
	profileHome := filepath.Join(home, "profile")
	if err := os.MkdirAll(profileHome, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		Home: home, Executable: os.Args[0], WaitReady: 300 * time.Millisecond,
		argsPrefix:        []string{"-test.run=TestManagerHelperProcess", "--"},
		endpointAllocator: func() (string, error) { return "ws://" + listener.Addr().String(), nil },
	}
	if _, err := manager.Start(context.Background(), "test", profileHome); err == nil {
		t.Fatal("port conflict was not reported")
	}
	if _, err := os.Stat(manager.statePath("test")); !os.IsNotExist(err) {
		t.Fatalf("failed start left runtime state: %v", err)
	}
}

func TestManagerProtectsActiveThreadFromStop(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper process signal behavior is platform-specific")
	}
	t.Setenv("GO_WANT_CODEXM_APP_SERVER_HELPER", "1")
	t.Setenv("CODEXM_HELPER_ACTIVE_THREAD", "1")
	home := t.TempDir()
	profileHome := filepath.Join(home, "profile")
	if err := os.MkdirAll(profileHome, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		Home: home, Executable: os.Args[0], WaitReady: 3 * time.Second,
		argsPrefix: []string{"-test.run=TestManagerHelperProcess", "--"},
	}
	if _, err := manager.Start(context.Background(), "test", profileHome); err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background(), "test", true)
	if err := manager.Stop(context.Background(), "test", false); err == nil ||
		!strings.Contains(err.Error(), "active thread") {
		t.Fatalf("active thread did not protect stop: %v", err)
	}
	if health := manager.Status(context.Background(), "test"); !health.Healthy {
		t.Fatalf("protected daemon stopped unexpectedly: %+v", health)
	}
}

func TestManagerRecoversFromCrashedStaleState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("helper process signal behavior is platform-specific")
	}
	t.Setenv("GO_WANT_CODEXM_APP_SERVER_HELPER", "1")
	home := t.TempDir()
	profileHome := filepath.Join(home, "profile")
	if err := os.MkdirAll(profileHome, 0o700); err != nil {
		t.Fatal(err)
	}
	manager := &Manager{
		Home: home, Executable: os.Args[0], WaitReady: 3 * time.Second,
		argsPrefix: []string{"-test.run=TestManagerHelperProcess", "--"},
	}
	if err := manager.writeState(State{
		Profile: "test", PID: 99999999, Endpoint: "ws://127.0.0.1:1",
		TokenFile: filepath.Join(home, "missing-token"), StartedAt: time.Now().Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	health, err := manager.Start(context.Background(), "test", profileHome)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Stop(context.Background(), "test", true)
	if !health.Healthy || health.PID == 99999999 {
		t.Fatalf("crash recovery did not replace stale state: %+v", health)
	}
}

func TestManagerHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_CODEXM_APP_SERVER_HELPER") != "1" {
		return
	}
	args := os.Args
	listen := ""
	for i := range args {
		if args[i] == "--listen" && i+1 < len(args) {
			listen = strings.TrimPrefix(args[i+1], "ws://")
			break
		}
	}
	if listen == "" {
		// --version calls made by the manager only need deterministic output.
		_, _ = os.Stdout.WriteString("codex-cli helper\n")
		os.Exit(0)
	}
	upgrader := websocket.Upgrader{}
	server := &http.Server{Addr: listen, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		if websocket.IsWebSocketUpgrade(r) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			for {
				var request struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params json.RawMessage `json:"params"`
				}
				if err := conn.ReadJSON(&request); err != nil {
					return
				}
				if len(request.ID) == 0 {
					continue
				}
				result := any(map[string]any{})
				switch request.Method {
				case "thread/loaded/list":
					result = map[string]any{"data": []string{"active-thread"}}
				case "thread/list":
					var params struct {
						Archived bool `json:"archived"`
					}
					_ = json.Unmarshal(request.Params, &params)
					data := []any{}
					if !params.Archived && os.Getenv("CODEXM_HELPER_ACTIVE_THREAD") == "1" {
						data = append(data, map[string]any{
							"id": "active-thread", "cwd": os.TempDir(),
							"status": map[string]any{"type": "active", "activeFlags": []string{}},
						})
					}
					result = map[string]any{"data": data}
				}
				var id any
				_ = json.Unmarshal(request.ID, &id)
				_ = conn.WriteJSON(map[string]any{"id": id, "result": result})
			}
		}
		http.NotFound(w, r)
	})}
	if err := server.ListenAndServe(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}
