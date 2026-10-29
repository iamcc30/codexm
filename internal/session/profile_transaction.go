package session

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Prepare all selected tips and their shared sources as one unit. Publishing
// just dependencies first can leave an ambiguous physical lineage after a crash.
func planProfileImportTransaction(root, home string, sources []*rollout, required map[string]*rollout) ([]projectMutation, error) {
	paths, headers, err := profileCatalog(home)
	if err != nil {
		return nil, err
	}
	selected := map[string]*rollout{}
	for _, source := range sources {
		selected[source.RolloutID] = source
	}
	writes := map[string]projectMutation{}
	deletes := map[string]projectMutation{}
	protected := map[string]bool{}
	for _, physical := range sortedIDs(required) {
		dependency := required[physical]
		if selected[physical] != nil {
			continue
		} // selected tip includes its inherited prefix
		target := filepath.Join(home, filepath.FromSlash(dependency.Relative))
		compressed := dependency.Compressed
		if path, exists := paths[physical]; exists {
			current, err := readRolloutBounded(path, "", nil, rolloutBytes(dependency), true)
			if err != nil {
				return nil, err
			}
			if !sameRawPrefix(current.RawLines, dependency.RawLines) {
				return nil, fmt.Errorf("history source prefix conflict for rollout %s", physical)
			}
			protected[path] = true
			if len(current.RawLines) >= len(dependency.RawLines) {
				continue
			}
			target = path
			compressed = strings.HasSuffix(path, ".zst")
		}
		protected[target] = true
		relative, err := filepath.Rel(home, target)
		if err != nil {
			return nil, err
		}
		data, err := encodeRollout(dependency.RawLines, compressed)
		if err != nil {
			return nil, err
		}
		key := filepath.ToSlash(relative)
		writes[key] = projectMutation{Path: key, Data: data, Mode: 0o600}
	}
	for _, source := range sources {
		if _, err := preflightHistoryImport(root, home, source); err != nil {
			return nil, err
		}
		retiring, err := profileRetirementPlan(home, source, paths, headers)
		if err != nil {
			return nil, err
		}
		target := filepath.Join(home, filepath.FromSlash(source.Relative))
		if path, exists := paths[source.RolloutID]; exists && path != target {
			retiring[source.RolloutID] = path
		}
		lines, err := materializeLines(source, root)
		if err != nil {
			return nil, err
		}
		data, err := encodeRollout(lines, source.Compressed)
		if err != nil {
			return nil, err
		}
		key := filepath.ToSlash(source.Relative)
		if _, exists := writes[key]; exists {
			return nil, fmt.Errorf("duplicate planned rollout target %s", key)
		}
		writes[key] = projectMutation{Path: key, Data: data, Mode: 0o600}
		for physical, path := range retiring {
			if protected[path] && selected[physical] == nil {
				continue
			}
			relative, err := filepath.Rel(home, path)
			if err != nil {
				return nil, err
			}
			key := filepath.ToSlash(relative)
			deletes[key] = projectMutation{Path: key, Delete: true, Mode: 0o600}
		}
	}
	nameMutation, err := prepareSessionNameMutation(home, sources)
	if err != nil {
		return nil, err
	}
	if nameMutation != nil {
		writes[nameMutation.Path] = *nameMutation
	}
	var operations []projectMutation
	for _, path := range sortedIDs(writes) {
		operations = append(operations, writes[path])
	}
	for _, path := range sortedIDs(deletes) {
		if _, written := writes[path]; !written {
			operations = append(operations, deletes[path])
		}
	}
	return operations, nil
}

func selectedProfileImports(projects projectState, profiles map[string]*rollout, reg *registry) []*rollout {
	var sources []*rollout
	for _, id := range sortedIDs(projects.Sessions) {
		source := projects.Sessions[id]
		previous := profiles[id]
		base, owned := reg.Sessions[id]
		selected := previous == nil && !owned
		if previous != nil {
			conflict, winner := compareSessions(source, previous, base, owned)
			selected = conflict == "" && winner == "project"
		}
		if selected {
			sources = append(sources, source)
		}
	}
	return sources
}

func validateSessionIndexData(data []byte) error {
	for _, line := range bytes.Split(data, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if _, err := decodeObject(line); err != nil {
			return fmt.Errorf("invalid session index; refusing to replace it: %w", err)
		}
	}
	return nil
}

func prepareSessionNameMutation(home string, sources []*rollout) (*projectMutation, error) {
	names, err := readSessionNames(home)
	if err != nil {
		return nil, err
	}
	var additions []byte
	for _, source := range sources {
		current, exists := names[source.ID]
		if (exists && current == source.Name) || (!exists && source.Name == "") {
			continue
		}
		entry := sessionIndexEntry{ID: source.ID, ThreadName: source.Name, UpdatedAt: nowRFC3339()}
		data, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		additions = append(additions, data...)
		additions = append(additions, '\n')
		names[source.ID] = source.Name
	}
	if len(additions) == 0 {
		return nil, nil
	}
	path := filepath.Join(home, "session_index.jsonl")
	if err := rejectSymlink(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := validateSessionIndexData(data); err != nil {
		return nil, err
	}
	if len(data) > 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	data = append(data, additions...)
	return &projectMutation{Path: "session_index.jsonl", Data: data, Mode: 0o600}, nil
}
