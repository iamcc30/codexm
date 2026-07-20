package appserver

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gofrs/flock"
	"github.com/iamcc30/codexm/internal/codex"
	gprocess "github.com/shirou/gopsutil/v4/process"
)

type Manager struct {
	Home              string
	Executable        string
	WaitReady         time.Duration
	argsPrefix        []string
	endpointAllocator func() (string, error)
}

func (m *Manager) Supported(profileHome string) bool {
	cmd := m.command("app-server", "--help")
	cmd.Env = setEnvironment(codex.EnvWithCodexHome(os.Environ(), profileHome), "RUST_LOG", "error")
	if err := cmd.Run(); err != nil {
		return false
	}
	cmd = m.command("--help")
	cmd.Env = setEnvironment(codex.EnvWithCodexHome(os.Environ(), profileHome), "RUST_LOG", "error")
	output, err := cmd.Output()
	return err == nil && strings.Contains(string(output), "--remote") &&
		strings.Contains(string(output), "--remote-auth-token-env")
}

func NewManager(home string) (*Manager, error) {
	runner, err := codex.Find()
	if err != nil {
		return nil, err
	}
	return &Manager{Home: home, Executable: runner.Executable, WaitReady: 10 * time.Second}, nil
}

func (m *Manager) profileDir(profile string) string {
	return filepath.Join(m.Home, "runtime", "app-server", profile)
}

func (m *Manager) statePath(profile string) string {
	return filepath.Join(m.profileDir(profile), "state.json")
}

func (m *Manager) Start(ctx context.Context, profile, profileHome string) (Health, error) {
	dir := m.profileDir(profile)
	if err := secureDir(dir); err != nil {
		return Health{}, err
	}
	lock := flock.New(filepath.Join(dir, "manager.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return Health{}, fmt.Errorf("lock app-server runtime: %w", err)
	}
	if !locked {
		return Health{}, fmt.Errorf("app-server operation already in progress for profile %q", profile)
	}
	defer func() { _ = lock.Unlock() }()

	health := m.Status(ctx, profile)
	if health.Healthy {
		return health, nil
	}
	if health.Running {
		return health, fmt.Errorf("app-server for profile %q is running but unhealthy; retry or stop it with --force before starting a replacement", profile)
	}
	_ = os.Remove(m.statePath(profile))

	allocator := m.endpointAllocator
	if allocator == nil {
		allocator = reserveEndpoint
	}
	endpoint, err := allocator()
	if err != nil {
		return Health{}, err
	}
	token, err := randomToken()
	if err != nil {
		return Health{}, err
	}
	tokenPath := filepath.Join(dir, "capability.token")
	if err := secureWrite(tokenPath, []byte(token)); err != nil {
		return Health{}, err
	}
	logPath := filepath.Join(dir, "app-server.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return Health{}, fmt.Errorf("open app-server log: %w", err)
	}
	cmd := m.command(
		"app-server",
		"--listen", endpoint,
		"--ws-auth", "capability-token",
		"--ws-token-file", tokenPath,
	)
	cmd.Env = setEnvironment(codex.EnvWithCodexHome(os.Environ(), profileHome), "RUST_LOG", "error")
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	configureDaemonCommand(cmd)
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return Health{}, fmt.Errorf("start app-server: %w", err)
	}
	_ = logFile.Close()
	state := State{
		Profile:      profile,
		PID:          cmd.Process.Pid,
		Endpoint:     endpoint,
		TokenFile:    tokenPath,
		LogFile:      logPath,
		CodexVersion: m.codexVersion(profileHome),
		StartedAt:    time.Now().UTC(),
	}
	if err := m.writeState(state); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Process.Release()
		return Health{}, err
	}
	wait := m.WaitReady
	if wait <= 0 {
		wait = 10 * time.Second
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		health := m.Status(ctx, profile)
		if health.Healthy {
			_ = cmd.Process.Release()
			return health, nil
		}
		if !health.Running {
			break
		}
		select {
		case <-ctx.Done():
			_ = cmd.Process.Kill()
			return Health{}, ctx.Err()
		case <-time.After(75 * time.Millisecond):
		}
	}
	_ = cmd.Process.Kill()
	_ = cmd.Process.Release()
	_ = os.Remove(m.statePath(profile))
	return Health{}, fmt.Errorf("app-server for profile %q did not become ready; see %s", profile, logPath)
}

func (m *Manager) Status(ctx context.Context, profile string) Health {
	state, err := m.readState(profile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Health{State: State{Profile: profile}}
		}
		return Health{State: State{Profile: profile}, Error: err.Error()}
	}
	health := Health{State: state, Running: processMatchesState(state)}
	if !health.Running {
		health.Error = "process is not running"
		return health
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.Replace(state.Endpoint, "ws://", "http://", 1)+"/readyz", nil)
	if err != nil {
		health.Error = err.Error()
		return health
	}
	client := http.Client{Timeout: time.Second}
	resp, err := client.Do(req)
	if err != nil {
		health.Error = err.Error()
		return health
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	health.Healthy = resp.StatusCode == http.StatusOK
	if !health.Healthy {
		health.Error = resp.Status
	}
	return health
}

func (m *Manager) Stop(ctx context.Context, profile string, force bool) error {
	dir := m.profileDir(profile)
	if err := secureDir(dir); err != nil {
		return err
	}
	lock := flock.New(filepath.Join(dir, "manager.lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return err
	}
	if !locked {
		return fmt.Errorf("app-server operation already in progress for profile %q", profile)
	}
	defer func() { _ = lock.Unlock() }()
	state, err := m.readState(profile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if !processMatchesState(state) {
		_ = os.Remove(m.statePath(profile))
		return nil
	}
	if !force {
		inspectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		client, dialErr := DialState(inspectCtx, state)
		if dialErr != nil {
			return fmt.Errorf("cannot verify active threads for profile %q: %w; use --force to stop", profile, dialErr)
		}
		loaded, listErr := client.LoadedThreads(inspectCtx)
		active := 0
		if threads, threadErr := client.Threads(inspectCtx, false); threadErr == nil {
			for _, thread := range threads {
				var status struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(thread.Status, &status) == nil && status.Type == "active" {
					active++
				}
			}
		} else if listErr == nil {
			// Older servers may expose loaded ids without runtime status.
			// Conservatively protect every loaded thread in that case.
			active = len(loaded)
		} else {
			_ = client.Close()
			return fmt.Errorf("cannot verify active threads for profile %q: thread/list and thread/loaded/list unavailable; use --force to stop", profile)
		}
		_ = client.Close()
		if active > 0 {
			return fmt.Errorf("profile %q has %d active thread(s); use --force to stop", profile, active)
		}
	}
	process, err := os.FindProcess(state.PID)
	if err != nil {
		return err
	}
	if runtime.GOOS == "windows" {
		err = process.Kill()
	} else {
		err = process.Signal(syscall.SIGTERM)
	}
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("stop app-server: %w", err)
	}
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) && processRunning(state.PID) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if processRunning(state.PID) {
		if !force {
			return fmt.Errorf("app-server did not stop; retry with --force")
		}
		_ = process.Kill()
	}
	_ = os.Remove(m.statePath(profile))
	return nil
}

func (m *Manager) readState(profile string) (State, error) {
	var state State
	data, err := os.ReadFile(m.statePath(profile))
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return state, fmt.Errorf("invalid runtime state for profile %q: %w", profile, err)
	}
	if state.Profile != profile || state.PID <= 0 || state.Endpoint == "" || state.TokenFile == "" {
		return state, fmt.Errorf("incomplete runtime state for profile %q", profile)
	}
	return state, nil
}

func (m *Manager) writeState(state State) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return secureWrite(m.statePath(state.Profile), append(data, '\n'))
}

func (m *Manager) codexVersion(profileHome string) string {
	cmd := m.command("--version")
	cmd.Env = codex.EnvWithCodexHome(os.Environ(), profileHome)
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func (m *Manager) command(args ...string) *exec.Cmd {
	all := append([]string(nil), m.argsPrefix...)
	all = append(all, args...)
	return exec.Command(m.Executable, all...)
}

func reserveEndpoint() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("reserve app-server port: %w", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", err
	}
	return "ws://" + addr, nil
}

func randomToken() (string, error) {
	data := make([]byte, 32)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func secureDir(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		return os.Chmod(path, 0o700)
	}
	return nil
}

func secureWrite(path string, data []byte) error {
	if err := secureDir(filepath.Dir(path)); err != nil {
		return err
	}
	tmp := path + ".tmp-" + strconv.Itoa(os.Getpid())
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmp, 0o600)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func setEnvironment(env []string, key, value string) []string {
	prefix := key + "="
	result := make([]string, 0, len(env)+1)
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			continue
		}
		result = append(result, item)
	}
	return append(result, prefix+value)
}

func processMatchesState(state State) bool {
	if !processRunning(state.PID) {
		return false
	}
	proc, err := gprocess.NewProcess(int32(state.PID))
	if err != nil {
		return runtime.GOOS == "windows"
	}
	createdMillis, err := proc.CreateTime()
	if err != nil || state.StartedAt.IsZero() {
		return true
	}
	created := time.UnixMilli(createdMillis)
	delta := state.StartedAt.Sub(created)
	if delta < 0 {
		delta = -delta
	}
	return delta <= 30*time.Second
}

func processRunning(pid int) bool {
	if pid <= 0 {
		return false
	}
	if exists, err := gprocess.PidExists(int32(pid)); err == nil {
		if !exists {
			return false
		}
		if proc, processErr := gprocess.NewProcess(int32(pid)); processErr == nil {
			if statuses, statusErr := proc.Status(); statusErr == nil {
				for _, status := range statuses {
					if status == gprocess.Zombie {
						return false
					}
				}
			}
		}
		return true
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if runtime.GOOS == "windows" {
		// FindProcess always succeeds on Windows; health probes remain the
		// fallback when the process table cannot be queried.
		return true
	}
	return process.Signal(syscall.Signal(0)) == nil
}
