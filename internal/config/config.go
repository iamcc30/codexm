package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const CurrentVersion = 2

const configLockTimeout = 10 * time.Second

type Profile struct {
	CodexHome          string   `json:"codex_home"`
	Description        string   `json:"description,omitempty"`
	CreatedAt          string   `json:"created_at"`
	ExcludedMCPServers []string `json:"excluded_mcp_servers,omitempty"`
}

type Config struct {
	Version        int                `json:"version"`
	DefaultProfile string             `json:"default_profile,omitempty"`
	Profiles       map[string]Profile `json:"profiles"`
	Bindings       map[string]string  `json:"bindings"`
}

func New() *Config {
	return &Config{
		Version:  CurrentVersion,
		Profiles: map[string]Profile{},
		Bindings: map[string]string{},
	}
}

func ManagerConfigDir() (string, error) {
	if custom := strings.TrimSpace(os.Getenv("CODEXM_HOME")); custom != "" {
		return filepath.Abs(expandHome(custom))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "windows":
		if appData := os.Getenv("APPDATA"); appData != "" {
			return filepath.Join(appData, "codexm"), nil
		}
		return filepath.Join(home, "AppData", "Roaming", "codexm"), nil
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "codexm"), nil
	default:
		if xdg := os.Getenv("XDG_CONFIG_HOME"); xdg != "" {
			return filepath.Join(xdg, "codexm"), nil
		}
		return filepath.Join(home, ".config", "codexm"), nil
	}
}

func ProfilesDataDir() (string, error) {
	if custom := strings.TrimSpace(os.Getenv("CODEXM_PROFILES_HOME")); custom != "" {
		return filepath.Abs(expandHome(custom))
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	switch runtime.GOOS {
	case "windows":
		if local := os.Getenv("LOCALAPPDATA"); local != "" {
			return filepath.Join(local, "codexm", "profiles"), nil
		}
		return filepath.Join(home, "AppData", "Local", "codexm", "profiles"), nil
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "codexm", "profiles"), nil
	default:
		if xdg := os.Getenv("XDG_DATA_HOME"); xdg != "" {
			return filepath.Join(xdg, "codexm", "profiles"), nil
		}
		return filepath.Join(home, ".local", "share", "codexm", "profiles"), nil
	}
}

func ConfigPath() (string, error) {
	dir, err := ManagerConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "config.json"), nil
}

func SharedCodexHome() (string, error) {
	dir, err := ManagerConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "shared"), nil
}

func SharedMCPConfigPath() (string, error) {
	home, err := SharedCodexHome()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "config.toml"), nil
}

func Load() (*Config, error) {
	path, err := ConfigPath()
	if err != nil {
		return nil, err
	}
	unlock, err := lockConfigShared(path)
	if err != nil {
		return nil, err
	}
	defer unlock()
	return loadPath(path)
}

func loadPath(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return New(), nil
	}
	if err != nil {
		return nil, err
	}
	cfg := New()
	if err := json.Unmarshal(b, cfg); err != nil {
		return nil, fmt.Errorf("invalid config %s: %w", path, err)
	}
	if cfg.Version == 0 {
		cfg.Version = CurrentVersion
	}
	if cfg.Version > CurrentVersion {
		return nil, fmt.Errorf("config version %d is newer than supported version %d", cfg.Version, CurrentVersion)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = map[string]Profile{}
	}
	if cfg.Bindings == nil {
		cfg.Bindings = map[string]string{}
	}
	for name := range cfg.Profiles {
		if err := ValidateProfileName(name); err != nil {
			return nil, fmt.Errorf("invalid profile in config: %w", err)
		}
	}
	return cfg, nil
}

func Save(cfg *Config) error {
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	unlock, err := lockConfig(path)
	if err != nil {
		return err
	}
	defer unlock()
	return savePath(path, cfg)
}

// Update serializes a complete load-modify-save transaction across codexm
// processes. Callers should use Update instead of a separate Load and Save
// whenever they mutate configuration, otherwise concurrent commands can lose
// each other's changes even when each individual rename is atomic.
func Update(mutate func(*Config) error) error {
	if mutate == nil {
		return errors.New("config update function is required")
	}
	path, err := ConfigPath()
	if err != nil {
		return err
	}
	unlock, err := lockConfig(path)
	if err != nil {
		return err
	}
	defer unlock()
	cfg, err := loadPath(path)
	if err != nil {
		return err
	}
	if err := mutate(cfg); err != nil {
		return err
	}
	return savePath(path, cfg)
}

func lockConfig(path string) (func(), error) {
	return lockConfigMode(path, false)
}

func lockConfigShared(path string) (func(), error) {
	return lockConfigMode(path, true)
}

func lockConfigMode(path string, shared bool) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	lock := flock.New(path + ".lock")
	ctx, cancel := context.WithTimeout(context.Background(), configLockTimeout)
	var locked bool
	var err error
	if shared {
		locked, err = lock.TryRLockContext(ctx, 25*time.Millisecond)
	} else {
		locked, err = lock.TryLockContext(ctx, 25*time.Millisecond)
	}
	cancel()
	if err != nil {
		return nil, fmt.Errorf("lock config: %w", err)
	}
	if !locked {
		return nil, fmt.Errorf("lock config: timed out after %s", configLockTimeout)
	}
	return func() { _ = lock.Unlock() }, nil
}

func savePath(path string, cfg *Config) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	cfg.Version = CurrentVersion
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(path), ".config.json.tmp-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpPath)
		}
	}()
	if runtime.GOOS != "windows" {
		if err := tmp.Chmod(0o600); err != nil {
			return err
		}
	}
	if _, err := tmp.Write(b); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return err
	}
	ok = true
	return nil
}

func ValidateProfileName(name string) error {
	if name == "" {
		return errors.New("profile name cannot be empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("invalid profile name %q: dot path segments are not allowed", name)
	}
	for _, r := range name {
		ok := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.'
		if !ok {
			return fmt.Errorf("invalid profile name %q: use letters, numbers, dot, dash or underscore", name)
		}
	}
	return nil
}

func DefaultProfileHome(name string) (string, error) {
	if err := ValidateProfileName(name); err != nil {
		return "", err
	}
	base, err := ProfilesDataDir()
	if err != nil {
		return "", err
	}
	home := filepath.Join(base, name)
	rel, err := filepath.Rel(base, home)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("profile home for %q escapes profiles directory", name)
	}
	return home, nil
}

func NormalizePath(path string) (string, error) {
	if path == "" {
		return "", errors.New("path cannot be empty")
	}
	path = expandHome(path)
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	if resolved, err := filepath.EvalSymlinks(abs); err == nil {
		abs = resolved
	}
	if runtime.GOOS == "windows" {
		abs = strings.ToLower(abs)
	}
	return abs, nil
}

func ResolveProfile(cfg *Config, path string) (string, string, bool) {
	normalized, err := NormalizePath(path)
	if err != nil {
		return "", "", false
	}
	type candidate struct {
		root    string
		profile string
	}
	var candidates []candidate
	for root, profile := range cfg.Bindings {
		normalizedRoot, err := NormalizePath(root)
		if err != nil {
			continue
		}
		if normalized == normalizedRoot || isWithin(normalized, normalizedRoot) {
			candidates = append(candidates, candidate{root: normalizedRoot, profile: profile})
		}
	}
	sort.Slice(candidates, func(i, j int) bool { return len(candidates[i].root) > len(candidates[j].root) })
	if len(candidates) > 0 {
		return candidates[0].profile, candidates[0].root, true
	}
	if cfg.DefaultProfile != "" {
		if _, ok := cfg.Profiles[cfg.DefaultProfile]; ok {
			return cfg.DefaultProfile, "default", true
		}
	}
	return "", "", false
}

func NewProfile(home, description string) Profile {
	return Profile{
		CodexHome:   home,
		Description: strings.TrimSpace(description),
		CreatedAt:   time.Now().UTC().Format(time.RFC3339),
	}
}

func RemoveBindingsForProfile(cfg *Config, name string) {
	for path, profile := range cfg.Bindings {
		if profile == name {
			delete(cfg.Bindings, path)
		}
	}
}

func SortedProfileNames(cfg *Config) []string {
	names := make([]string, 0, len(cfg.Profiles))
	for name := range cfg.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func SetMCPExcluded(profile Profile, server string, excluded bool) Profile {
	set := make(map[string]bool, len(profile.ExcludedMCPServers)+1)
	for _, name := range profile.ExcludedMCPServers {
		if name != "" {
			set[name] = true
		}
	}
	if excluded {
		set[server] = true
	} else {
		delete(set, server)
	}
	profile.ExcludedMCPServers = profile.ExcludedMCPServers[:0]
	for name := range set {
		profile.ExcludedMCPServers = append(profile.ExcludedMCPServers, name)
	}
	sort.Strings(profile.ExcludedMCPServers)
	return profile
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, `~\`) {
		if home, err := os.UserHomeDir(); err == nil {
			if path == "~" {
				return home
			}
			return filepath.Join(home, path[2:])
		}
	}
	return path
}

func isWithin(path, root string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
