package monitor

import (
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iamcc30/codexm/internal/config"
	"github.com/iamcc30/codexm/internal/session"
)

const defaultSnapshotCacheTTL = 5 * time.Second

type attribution struct {
	root   string
	source string
}

type attributionCacheEntry struct {
	value attribution
	at    time.Time
}

type projectFacts struct {
	mirror    session.Inspection
	gitRoot   string
	gitBranch string
}

type projectFactsCacheEntry struct {
	value projectFacts
	at    time.Time
}

// snapshotCache keeps filesystem, process-table, Git, and mirror inspection
// work out of the notification hot path. Thread/account state remains live;
// cold facts are refreshed on a short bounded interval.
type snapshotCache struct {
	mu sync.Mutex

	ttl             time.Duration
	configSignature string
	processes       []Task
	processesAt     time.Time
	attributions    map[string]attributionCacheEntry
	projects        map[string]projectFactsCacheEntry

	processScanner  func(*config.Config) []Task
	gitInspector    func(string) (string, string)
	mirrorInspector func(string, string, string) session.Inspection
}

func newSnapshotCache() *snapshotCache {
	return &snapshotCache{
		ttl:             defaultSnapshotCacheTTL,
		attributions:    map[string]attributionCacheEntry{},
		projects:        map[string]projectFactsCacheEntry{},
		processScanner:  scanProcessTasks,
		gitInspector:    gitInfo,
		mirrorInspector: session.Inspect,
	}
}

func (c *snapshotCache) prepare(cfg *config.Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	signature := snapshotConfigSignature(cfg)
	if signature != c.configSignature {
		c.configSignature = signature
		c.processesAt = time.Time{}
		c.attributions = map[string]attributionCacheEntry{}
		c.projects = map[string]projectFactsCacheEntry{}
	}
}

func (c *snapshotCache) unmanaged(cfg *config.Config, managed map[int]bool) []Task {
	c.prepare(cfg)
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	if c.processesAt.IsZero() || now.Sub(c.processesAt) >= c.ttl {
		c.processes = c.processScanner(cfg)
		c.processesAt = now
	}
	return filterManagedProcesses(c.processes, managed)
}

func (c *snapshotCache) attributeProject(cfg *config.Config, cwd string) (string, string) {
	if cwd == "" {
		return "(unknown)", "cwd"
	}
	key := filepath.Clean(cwd)
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.attributions[key]; ok && time.Since(entry.at) < c.ttl {
		return entry.value.root, entry.value.source
	}
	value := attribution{root: filepath.Clean(cwd), source: "cwd"}
	if root, _, err := session.FindProject(cwd); err == nil {
		value = attribution{root: root, source: "mirror"}
	} else if _, source, ok := config.ResolveProfile(cfg, cwd); ok && source != "default" {
		value = attribution{root: source, source: "binding"}
	} else if root, _ := c.gitInspector(cwd); root != "" {
		value = attribution{root: root, source: "git"}
	}
	c.attributions[key] = attributionCacheEntry{value: value, at: time.Now()}
	return value.root, value.source
}

func (c *snapshotCache) project(root, profileHome, managerHome string) projectFacts {
	key := root + "\x00" + profileHome + "\x00" + managerHome
	c.mu.Lock()
	defer c.mu.Unlock()
	if entry, ok := c.projects[key]; ok && time.Since(entry.at) < c.ttl {
		return entry.value
	}
	value := projectFacts{}
	if root != "" && root != "(unknown)" {
		value.mirror = c.mirrorInspector(root, profileHome, managerHome)
		value.gitRoot, value.gitBranch = c.gitInspector(root)
	}
	c.projects[key] = projectFactsCacheEntry{value: value, at: time.Now()}
	return value
}

func snapshotConfigSignature(cfg *config.Config) string {
	var values []string
	for name, profile := range cfg.Profiles {
		values = append(values, "p:"+name+"="+profile.CodexHome)
	}
	for root, profile := range cfg.Bindings {
		values = append(values, "b:"+root+"="+profile)
	}
	sort.Strings(values)
	return strings.Join(values, "\n")
}
