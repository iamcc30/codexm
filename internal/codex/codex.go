package codex

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

type Runner struct {
	Executable string
}

type CredentialStores struct {
	CLI      string `toml:"cli_auth_credentials_store"`
	MCPOAuth string `toml:"mcp_oauth_credentials_store"`
}

func Find() (*Runner, error) {
	if override := strings.TrimSpace(os.Getenv("CODEXM_CODEX_BIN")); override != "" {
		path, err := exec.LookPath(override)
		if err != nil {
			return nil, fmt.Errorf("CODEXM_CODEX_BIN %q not found: %w", override, err)
		}
		return &Runner{Executable: path}, nil
	}
	path, err := exec.LookPath("codex")
	if err != nil {
		return nil, errors.New("codex executable not found in PATH; install the OpenAI Codex CLI first")
	}
	return &Runner{Executable: path}, nil
}

func (r *Runner) Run(codexHome, cwd string, args []string) error {
	return r.RunWithEnv(codexHome, cwd, args, nil)
}

func (r *Runner) RunWithEnv(codexHome, cwd string, args []string, extraEnv map[string]string) error {
	cmd := exec.Command(r.Executable, args...)
	cmd.Env = EnvWithCodexHome(os.Environ(), codexHome)
	for key, value := range extraEnv {
		cmd.Env = setEnv(cmd.Env, key, value)
	}
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	interrupts := make(chan os.Signal, 2)
	signal.Notify(interrupts, os.Interrupt)
	defer signal.Stop(interrupts)
	if err := cmd.Start(); err != nil {
		return err
	}
	waited := make(chan error, 1)
	go func() {
		waited <- cmd.Wait()
	}()
	var err error
	for err == nil {
		select {
		case err = <-waited:
			if err == nil {
				return nil
			}
		case <-interrupts:
			// Codex receives terminal interrupts from the same foreground process
			// group. Keep codexm alive long enough to wait and run post-sync, but
			// do not forward a duplicate interrupt to the child.
		}
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &ExitError{Code: normalizedExitCode(exitErr), Err: err}
		}
		return err
	}
	return nil
}

func setEnv(env []string, key, value string) []string {
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

func (r *Runner) Capture(codexHome, cwd string, args []string) (string, int, error) {
	cmd := exec.Command(r.Executable, args...)
	cmd.Env = EnvWithCodexHome(os.Environ(), codexHome)
	if cwd != "" {
		cmd.Dir = cwd
	}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	if err == nil {
		return strings.TrimSpace(out.String()), 0, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return strings.TrimSpace(out.String()), normalizedExitCode(exitErr), nil
	}
	return strings.TrimSpace(out.String()), -1, err
}

func EnvWithCodexHome(env []string, codexHome string) []string {
	key := "CODEX_HOME"
	prefix := key + "="
	out := make([]string, 0, len(env)+2)
	for _, item := range env {
		match := strings.HasPrefix(item, prefix)
		if runtime.GOOS == "windows" {
			match = strings.EqualFold(strings.SplitN(item, "=", 2)[0], key)
		}
		if !match {
			out = append(out, item)
		}
	}
	out = append(out, prefix+codexHome)
	out = append(out, "CODEXM_ACTIVE_HOME="+codexHome)
	return out
}

func EnsureProfileHome(home, credentialStore string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(home, 0o700)
	}
	configPath := filepath.Join(home, "config.toml")
	return ensureCredentialStores(configPath, credentialStore)
}

// EnsureMCPOAuthCredentialStore migrates profiles created by older codexm
// versions. It adds the MCP OAuth store only when it is absent and follows the
// existing CLI credential-store choice, preserving explicit MCP overrides.
func EnsureMCPOAuthCredentialStore(home string) error {
	path := filepath.Join(home, "config.toml")
	stores, err := ReadCredentialStores(path)
	if err != nil {
		return err
	}
	if stores.MCPOAuth != "" {
		return nil
	}
	store := stores.CLI
	if store == "" {
		store = "file"
	}
	return ensureCredentialStores(path, store)
}

func ReadCredentialStores(path string) (CredentialStores, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return CredentialStores{}, err
	}
	var stores CredentialStores
	if err := toml.Unmarshal(data, &stores); err != nil {
		return CredentialStores{}, fmt.Errorf("invalid TOML %s: %w", path, err)
	}
	return stores, nil
}

func ensureCredentialStores(path, store string) error {
	if store == "" {
		store = "file"
	}
	switch store {
	case "file", "auto", "keyring":
	default:
		return fmt.Errorf("unsupported credential store %q", store)
	}
	settings := map[string]string{
		"cli_auth_credentials_store":  fmt.Sprintf("cli_auth_credentials_store = %q", store),
		"mcp_oauth_credentials_store": fmt.Sprintf("mcp_oauth_credentials_store = %q", store),
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		content := "# Managed by codexm. You may add other Codex settings below.\n" +
			settings["cli_auth_credentials_store"] + "\n" +
			settings["mcp_oauth_credentials_store"] + "\n"
		return os.WriteFile(path, []byte(content), 0o600)
	}
	if err != nil {
		return err
	}
	var existing CredentialStores
	if err := toml.Unmarshal(data, &existing); err != nil {
		return fmt.Errorf("invalid TOML %s: %w", path, err)
	}
	lines := strings.Split(string(data), "\n")
	inTopLevel := true
	for i, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "[") {
			inTopLevel = false
		}
		if !inTopLevel {
			continue
		}
		for key, line := range settings {
			if isAssignment(trimmed, key) {
				lines[i] = line
				delete(settings, key)
				break
			}
		}
	}
	if len(settings) > 0 {
		insertAt := 0
		for insertAt < len(lines) {
			trimmed := strings.TrimSpace(lines[insertAt])
			if strings.HasPrefix(trimmed, "[") {
				break
			}
			insertAt++
		}
		updated := make([]string, 0, len(lines)+len(settings))
		updated = append(updated, lines[:insertAt]...)
		if line, ok := settings["cli_auth_credentials_store"]; ok {
			updated = append(updated, line)
		}
		if line, ok := settings["mcp_oauth_credentials_store"]; ok {
			updated = append(updated, line)
		}
		updated = append(updated, lines[insertAt:]...)
		lines = updated
	}
	content := strings.Join(lines, "\n")
	if !strings.HasSuffix(content, "\n") {
		content += "\n"
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(content), 0o600); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmp, 0o600)
	}
	return os.Rename(tmp, path)
}

func isAssignment(line, key string) bool {
	lhs, _, ok := strings.Cut(line, "=")
	if !ok {
		return false
	}
	lhs = strings.TrimSpace(lhs)
	return lhs == key || lhs == `"`+key+`"` || lhs == `'`+key+`'`
}

type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }
