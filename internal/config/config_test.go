package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestResolveProfileUsesDeepestBinding(t *testing.T) {
	root := t.TempDir()
	child := filepath.Join(root, "team", "project")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	rootNorm, err := NormalizePath(root)
	if err != nil {
		t.Fatal(err)
	}
	teamNorm, err := NormalizePath(filepath.Join(root, "team"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := New()
	cfg.Profiles["general"] = NewProfile(filepath.Join(root, "general"), "")
	cfg.Profiles["team"] = NewProfile(filepath.Join(root, "team-home"), "")
	cfg.Bindings[rootNorm] = "general"
	cfg.Bindings[teamNorm] = "team"

	profile, source, ok := ResolveProfile(cfg, child)
	if !ok {
		t.Fatal("expected profile")
	}
	if profile != "team" {
		t.Fatalf("expected team, got %s", profile)
	}
	if source != teamNorm {
		t.Fatalf("expected source %s, got %s", teamNorm, source)
	}
}

func TestResolveProfileFallsBackToDefault(t *testing.T) {
	cfg := New()
	cfg.Profiles["account1"] = NewProfile(t.TempDir(), "")
	cfg.DefaultProfile = "account1"
	profile, source, ok := ResolveProfile(cfg, t.TempDir())
	if !ok || profile != "account1" || source != "default" {
		t.Fatalf("unexpected result: %q %q %t", profile, source, ok)
	}
}

func TestValidateProfileName(t *testing.T) {
	good := []string{"account1", "work-team", "a.b", "x_y"}
	for _, name := range good {
		if err := ValidateProfileName(name); err != nil {
			t.Fatalf("expected %q to be valid: %v", name, err)
		}
	}
	bad := []string{"", ".", "..", "work team", "../bad", "中文"}
	for _, name := range bad {
		if err := ValidateProfileName(name); err == nil {
			t.Fatalf("expected %q to be invalid", name)
		}
	}
}

func TestDefaultProfileHomeCannotEscapeProfilesDirectory(t *testing.T) {
	base := filepath.Join(t.TempDir(), "profiles")
	t.Setenv("CODEXM_PROFILES_HOME", base)
	for _, name := range []string{".", ".."} {
		if _, err := DefaultProfileHome(name); err == nil {
			t.Fatalf("expected %q to be rejected", name)
		}
	}
	home, err := DefaultProfileHome("safe.name")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(home) != base {
		t.Fatalf("profile home escaped base: %s", home)
	}
}

func TestLoadRejectsLegacyUnsafeProfileName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEXM_HOME", home)
	data := []byte(`{"version":2,"profiles":{"..":{"codex_home":"/tmp/outside","created_at":"2026-01-01T00:00:00Z"}},"bindings":{}}`)
	if err := os.WriteFile(filepath.Join(home, "config.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(); err == nil {
		t.Fatal("unsafe profile name in an existing config was accepted")
	}
}

func TestUpdateSerializesConcurrentWriters(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEXM_HOME", home)
	const writers = 24
	start := make(chan struct{})
	const readers = 8
	errs := make(chan error, writers+readers*20)
	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			<-start
			name := fmt.Sprintf("profile-%02d", index)
			errs <- Update(func(cfg *Config) error {
				cfg.Profiles[name] = NewProfile(filepath.Join(home, name), "")
				return nil
			})
		}(i)
	}
	for i := 0; i < readers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for j := 0; j < 20; j++ {
				_, err := Load()
				errs <- err
			}
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Profiles) != writers {
		t.Fatalf("lost concurrent updates: got %d profiles, want %d", len(cfg.Profiles), writers)
	}
}

func TestSetMCPExcludedIsSortedAndIdempotent(t *testing.T) {
	p := NewProfile(t.TempDir(), "")
	p = SetMCPExcluded(p, "zeta", true)
	p = SetMCPExcluded(p, "alpha", true)
	p = SetMCPExcluded(p, "zeta", true)
	if len(p.ExcludedMCPServers) != 2 || p.ExcludedMCPServers[0] != "alpha" || p.ExcludedMCPServers[1] != "zeta" {
		t.Fatalf("unexpected exclusions: %v", p.ExcludedMCPServers)
	}
	p = SetMCPExcluded(p, "alpha", false)
	if len(p.ExcludedMCPServers) != 1 || p.ExcludedMCPServers[0] != "zeta" {
		t.Fatalf("unexpected exclusions after include: %v", p.ExcludedMCPServers)
	}
}
