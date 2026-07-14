package sharedmcp

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

const (
	beginMarker = "# >>> codexm shared MCP servers (managed; do not edit)"
	endMarker   = "# <<< codexm shared MCP servers"
)

type Result struct {
	Changed        bool
	SharedServers  []string
	Inherited      []string
	LocalOverrides []string
	Excluded       []string
}

// EnsureSharedHome creates the isolated CODEX_HOME used by `codexm mcp`
// commands. Only its mcp_servers table is synchronized to account profiles.
func EnsureSharedHome(home string) error {
	if err := os.MkdirAll(home, 0o700); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(home, 0o700)
	}
	return nil
}

func ServerNames(sharedConfigPath string) ([]string, error) {
	servers, err := readServers(sharedConfigPath)
	if err != nil {
		return nil, err
	}
	return sortedKeys(servers), nil
}

// Sync copies shared MCP server definitions into a marked block in a profile's
// config.toml. Settings and comments outside that block are preserved. A
// profile-local server with the same name wins over the shared definition.
func Sync(sharedConfigPath, profileConfigPath string, excluded []string, write bool) (Result, error) {
	shared, err := readServers(sharedConfigPath)
	if err != nil {
		return Result{}, err
	}
	result := Result{SharedServers: sortedKeys(shared)}

	data, err := os.ReadFile(profileConfigPath)
	if err != nil {
		return result, err
	}
	base, hadManagedBlock, err := stripManagedBlock(data)
	if err != nil {
		return result, fmt.Errorf("invalid managed MCP block in %s: %w", profileConfigPath, err)
	}
	local, err := parseDocument(base, profileConfigPath)
	if err != nil {
		return result, err
	}
	localServers := table(local, "mcp_servers")
	excludedSet := make(map[string]bool, len(excluded))
	for _, name := range excluded {
		excludedSet[name] = true
	}

	inherited := make(map[string]any)
	for name, value := range shared {
		switch {
		case excludedSet[name]:
			result.Excluded = append(result.Excluded, name)
		case localServers != nil:
			if _, ok := localServers[name]; ok {
				result.LocalOverrides = append(result.LocalOverrides, name)
				continue
			}
			inherited[name] = value
		default:
			inherited[name] = value
		}
	}
	result.Inherited = sortedKeys(inherited)
	sort.Strings(result.LocalOverrides)
	sort.Strings(result.Excluded)
	if !hadManagedBlock && len(inherited) == 0 {
		return result, nil
	}

	desired, err := render(base, inherited)
	if err != nil {
		return result, err
	}
	if _, err := parseDocument(desired, profileConfigPath); err != nil {
		return result, fmt.Errorf("cannot merge shared MCP servers: %w", err)
	}
	result.Changed = !bytes.Equal(data, desired)
	if write && result.Changed {
		if err := atomicWrite(profileConfigPath, desired, 0o600); err != nil {
			return result, err
		}
	}
	return result, nil
}

func readServers(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]any{}, nil
	}
	if err != nil {
		return nil, err
	}
	doc, err := parseDocument(data, path)
	if err != nil {
		return nil, err
	}
	servers := table(doc, "mcp_servers")
	if servers == nil {
		return map[string]any{}, nil
	}
	for name, value := range servers {
		if _, ok := value.(map[string]any); !ok {
			return nil, fmt.Errorf("invalid MCP server %q in %s: expected a table", name, path)
		}
	}
	return servers, nil
}

func parseDocument(data []byte, path string) (map[string]any, error) {
	doc := make(map[string]any)
	if len(bytes.TrimSpace(data)) == 0 {
		return doc, nil
	}
	if err := toml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("invalid TOML %s: %w", path, err)
	}
	return doc, nil
}

func table(doc map[string]any, key string) map[string]any {
	if raw, ok := doc[key]; ok {
		if value, ok := raw.(map[string]any); ok {
			return value
		}
	}
	return nil
}

func render(base []byte, servers map[string]any) ([]byte, error) {
	base = bytes.TrimRight(base, "\r\n")
	if len(servers) == 0 {
		if len(base) == 0 {
			return nil, nil
		}
		return append(base, '\n'), nil
	}
	body, err := toml.Marshal(map[string]any{"mcp_servers": servers})
	if err != nil {
		return nil, fmt.Errorf("encode shared MCP servers: %w", err)
	}
	out := make([]byte, 0, len(base)+len(body)+len(beginMarker)+len(endMarker)+8)
	out = append(out, base...)
	if len(out) > 0 {
		out = append(out, '\n', '\n')
	}
	out = append(out, beginMarker...)
	out = append(out, '\n')
	out = append(out, body...)
	if len(body) > 0 && body[len(body)-1] != '\n' {
		out = append(out, '\n')
	}
	out = append(out, endMarker...)
	out = append(out, '\n')
	return out, nil
}

func stripManagedBlock(data []byte) ([]byte, bool, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")
	begin, end := -1, -1
	for i, line := range lines {
		switch strings.TrimSpace(line) {
		case beginMarker:
			if begin != -1 {
				return nil, false, errors.New("multiple begin markers")
			}
			begin = i
		case endMarker:
			if end != -1 {
				return nil, false, errors.New("multiple end markers")
			}
			end = i
		}
	}
	if begin == -1 && end == -1 {
		return data, false, nil
	}
	if begin == -1 || end == -1 || end < begin {
		return nil, false, errors.New("unmatched marker")
	}
	lines = append(lines[:begin], lines[end+1:]...)
	return []byte(strings.TrimRight(strings.Join(lines, "\n"), "\n") + "\n"), true, nil
}

func sortedKeys(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func atomicWrite(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmp, perm)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
