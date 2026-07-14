package codex

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

type Runner struct {
	Executable string
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
	cmd := exec.Command(r.Executable, args...)
	cmd.Env = EnvWithCodexHome(os.Environ(), codexHome)
	if cwd != "" {
		cmd.Dir = cwd
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return &ExitError{Code: exitErr.ExitCode(), Err: err}
		}
		return err
	}
	return nil
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
		return strings.TrimSpace(out.String()), exitErr.ExitCode(), nil
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
	return ensureCredentialStore(configPath, credentialStore)
}

func ensureCredentialStore(path, store string) error {
	if store == "" {
		store = "file"
	}
	switch store {
	case "file", "auto", "keyring":
	default:
		return fmt.Errorf("unsupported credential store %q", store)
	}
	line := fmt.Sprintf("cli_auth_credentials_store = %q", store)
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return os.WriteFile(path, []byte("# Managed by codexm. You may add other Codex settings below.\n"+line+"\n"), 0o600)
	}
	if err != nil {
		return err
	}
	lines := strings.Split(string(data), "\n")
	replaced := false
	inTopLevel := true
	for i, raw := range lines {
		trimmed := strings.TrimSpace(raw)
		if strings.HasPrefix(trimmed, "[") {
			inTopLevel = false
		}
		if inTopLevel && strings.HasPrefix(trimmed, "cli_auth_credentials_store") {
			lines[i] = line
			replaced = true
			break
		}
	}
	if !replaced {
		insertAt := 0
		for insertAt < len(lines) {
			trimmed := strings.TrimSpace(lines[insertAt])
			if strings.HasPrefix(trimmed, "[") {
				break
			}
			insertAt++
		}
		updated := make([]string, 0, len(lines)+1)
		updated = append(updated, lines[:insertAt]...)
		updated = append(updated, line)
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

type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string { return e.Err.Error() }
func (e *ExitError) Unwrap() error { return e.Err }
