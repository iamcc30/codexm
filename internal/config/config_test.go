package config

import (
	"os"
	"path/filepath"
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
	bad := []string{"", "work team", "../bad", "中文"}
	for _, name := range bad {
		if err := ValidateProfileName(name); err == nil {
			t.Fatalf("expected %q to be invalid", name)
		}
	}
}
