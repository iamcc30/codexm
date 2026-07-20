package monitor

import (
	"testing"
	"time"

	"github.com/iamcc30/codexm/internal/config"
	"github.com/iamcc30/codexm/internal/session"
)

func TestSnapshotCacheSeparatesHotStateFromColdInspection(t *testing.T) {
	cfg := config.New()
	cfg.Profiles["work"] = config.NewProfile(t.TempDir(), "")
	cache := newSnapshotCache()
	cache.ttl = time.Hour
	processCalls, gitCalls, mirrorCalls := 0, 0, 0
	cache.processScanner = func(*config.Config) []Task {
		processCalls++
		return []Task{{ID: "pid-42", PID: 42, Profile: "work"}}
	}
	cache.gitInspector = func(string) (string, string) {
		gitCalls++
		return "/repo", "main"
	}
	cache.mirrorInspector = func(root, _, _ string) session.Inspection {
		mirrorCalls++
		return session.Inspection{Enabled: true, ProjectRoot: root}
	}

	cache.prepare(cfg)
	if root, source := cache.attributeProject(cfg, t.TempDir()); root != "/repo" || source != "git" {
		t.Fatalf("unexpected attribution: %q %q", root, source)
	}
	cache.attributeProject(cfg, t.TempDir()) // A different cwd legitimately misses.
	cwd := t.TempDir()
	cache.attributeProject(cfg, cwd)
	cache.attributeProject(cfg, cwd)
	if gitCalls != 3 {
		t.Fatalf("attribution cache missed: git calls=%d, want 3", gitCalls)
	}

	first := cache.project("/repo", cfg.Profiles["work"].CodexHome, t.TempDir())
	second := cache.project("/repo", cfg.Profiles["work"].CodexHome, t.TempDir())
	if !first.mirror.Enabled || !second.mirror.Enabled {
		t.Fatalf("project facts missing: %+v %+v", first, second)
	}
	// Different manager homes are different inspection registries.
	if mirrorCalls != 2 {
		t.Fatalf("project cache key did not include manager home: calls=%d", mirrorCalls)
	}
	manager := t.TempDir()
	cache.project("/repo", cfg.Profiles["work"].CodexHome, manager)
	cache.project("/repo", cfg.Profiles["work"].CodexHome, manager)
	if mirrorCalls != 3 {
		t.Fatalf("project facts were not cached: calls=%d", mirrorCalls)
	}

	if got := cache.unmanaged(cfg, nil); len(got) != 1 {
		t.Fatalf("unexpected process snapshot: %+v", got)
	}
	if got := cache.unmanaged(cfg, map[int]bool{42: true}); len(got) != 0 {
		t.Fatalf("managed pid was not filtered from cached scan: %+v", got)
	}
	if processCalls != 1 {
		t.Fatalf("process table was rescanned: calls=%d", processCalls)
	}
}

func TestSnapshotCacheExpires(t *testing.T) {
	cfg := config.New()
	cache := newSnapshotCache()
	cache.ttl = -time.Nanosecond
	calls := 0
	cache.processScanner = func(*config.Config) []Task {
		calls++
		return nil
	}
	cache.unmanaged(cfg, nil)
	cache.unmanaged(cfg, nil)
	if calls != 2 {
		t.Fatalf("expired cache was reused: calls=%d", calls)
	}
}
