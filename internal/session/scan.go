package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type projectState struct {
	Sessions   map[string]*rollout
	Tombstones map[string]Tombstone
}

func scanProject(root string) (projectState, error) {
	if _, err := os.Lstat(transactionPath(root)); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return projectState{}, err
		}
		return projectState{}, errors.New("pending project transaction; run session sync to recover")
	}
	dotDir := filepath.Join(root, projectDirName)
	metadataDir := filepath.Join(dotDir, "metadata")
	sessionByPath := map[string]*rollout{}
	state := projectState{
		Sessions:   map[string]*rollout{},
		Tombstones: map[string]Tombstone{},
	}

	if _, err := regularFiles(filepath.Join(dotDir, "history"), func(string) bool { return true }); err != nil {
		return state, err
	}
	metadataFiles, err := regularFiles(metadataDir, func(path string) bool {
		return strings.HasSuffix(path, ".json")
	})
	if err != nil {
		return state, err
	}
	var project Project
	if err := readJSON(filepath.Join(dotDir, projectFileName), &project); err != nil {
		return state, err
	}
	if err := validateProject(project, filepath.Join(dotDir, projectFileName)); err != nil {
		return state, err
	}
	for _, path := range metadataFiles {
		var metadata Metadata
		if err := readJSON(path, &metadata); err != nil {
			return state, err
		}
		if metadata.FormatVersion != FormatVersion && metadata.FormatVersion != HistoryFormatVersion {
			return state, fmt.Errorf("unsupported metadata format version %d in %s", metadata.FormatVersion, path)
		}
		if metadata.FormatVersion == HistoryFormatVersion && project.FormatVersion != HistoryFormatVersion {
			return state, fmt.Errorf("history metadata requires project format version 2")
		}
		if !validUUID(metadata.SessionID) {
			return state, fmt.Errorf("invalid session id in %s", path)
		}
		expectedName := metadata.SessionID + ".json"
		if filepath.Base(path) != expectedName {
			return state, fmt.Errorf("metadata filename does not match session id in %s", path)
		}
		if !safeRelative(metadata.RolloutPath) {
			return state, fmt.Errorf("unsafe rollout path %q in %s", metadata.RolloutPath, path)
		}
		relative := filepath.Clean(filepath.FromSlash(metadata.RolloutPath))
		if !strings.HasPrefix(relative, "sessions"+string(filepath.Separator)) &&
			!strings.HasPrefix(relative, "archived_sessions"+string(filepath.Separator)) {
			return state, fmt.Errorf("rollout path is outside managed session directories in %s", path)
		}
		rolloutPath := filepath.Join(dotDir, relative)
		if _, duplicate := sessionByPath[rolloutPath]; duplicate {
			return state, fmt.Errorf("duplicate rollout path %s", rolloutPath)
		}
		storedMappings := append([]CWDMapping{}, metadata.CWDs...)
		session, err := readRollout(rolloutPath, root, storedMappings)
		if err != nil {
			return state, err
		}
		session.CWDs = append([]CWDMapping(nil), metadata.CWDs...)
		session.Relative = filepath.ToSlash(relative)
		session.Archived = strings.HasPrefix(relative, "archived_sessions"+string(filepath.Separator))
		session.Name = metadata.Name
		session.ProjectMeta = metadata
		if session.ID != metadata.SessionID {
			return state, fmt.Errorf("session id in %s does not match sidecar %s", rolloutPath, path)
		}
		if !filenameHasSessionID(rolloutPath, session.ID) {
			return state, fmt.Errorf("rollout filename does not match session id in %s", rolloutPath)
		}
		if session.Archived != metadata.Archived {
			return state, fmt.Errorf("archived state does not match sidecar for %s", session.ID)
		}
		if err := loadProjectHistory(root, session, metadata); err != nil {
			return state, err
		}
		if session.Checksum != metadata.Checksum || session.LineCount != metadata.LineCount {
			return state, fmt.Errorf("session content does not match sidecar for %s", session.ID)
		}
		if _, duplicate := state.Sessions[session.ID]; duplicate {
			return state, fmt.Errorf("duplicate session id %s", session.ID)
		}
		state.Sessions[session.ID] = session
		sessionByPath[rolloutPath] = session
	}

	for _, dir := range []string{"sessions", "archived_sessions"} {
		base := filepath.Join(dotDir, dir)
		files, err := regularFiles(base, isRolloutFile)
		if err != nil {
			return state, err
		}
		for _, path := range files {
			if _, ok := sessionByPath[path]; !ok {
				return state, fmt.Errorf("session file has no metadata sidecar: %s", path)
			}
		}
	}

	tombstoneDir := filepath.Join(dotDir, "tombstones")
	tombstoneFiles, err := regularFiles(tombstoneDir, func(path string) bool {
		return strings.HasSuffix(path, ".json")
	})
	if err != nil {
		return state, err
	}
	for _, path := range tombstoneFiles {
		var tombstone Tombstone
		if err := readJSON(path, &tombstone); err != nil {
			return state, err
		}
		if tombstone.FormatVersion != FormatVersion || !validUUID(tombstone.SessionID) {
			return state, fmt.Errorf("invalid tombstone %s", path)
		}
		if filepath.Base(path) != tombstone.SessionID+".json" {
			return state, fmt.Errorf("tombstone filename does not match session id in %s", path)
		}
		if _, exists := state.Tombstones[tombstone.SessionID]; exists {
			return state, fmt.Errorf("duplicate tombstone for %s", tombstone.SessionID)
		}
		if _, exists := state.Sessions[tombstone.SessionID]; exists {
			return state, fmt.Errorf("session %s has both a rollout and tombstone", tombstone.SessionID)
		}
		state.Tombstones[tombstone.SessionID] = tombstone
	}
	if err := validateProjectPhysicalSnapshots(state); err != nil {
		return state, err
	}
	return state, nil
}

func scanProfile(home, projectRoot string) (map[string]*rollout, error) {
	if _, err := os.Lstat(filepath.Join(home, profileTransactionFile)); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return nil, err
		}
		return nil, errors.New("pending profile transaction; run session sync in the originating project to recover")
	}
	names, err := readSessionNames(home)
	if err != nil {
		return nil, err
	}
	paths, headers, err := profileCatalog(home)
	if err != nil {
		return nil, err
	}
	groups := map[string]map[string]bool{}
	for physical, header := range headers {
		if _, within := relativeWithin(projectRoot, header.InitialCWD); within {
			if header.ValidationError != nil {
				return nil, header.ValidationError
			}
			if groups[header.ID] == nil {
				groups[header.ID] = map[string]bool{}
			}
			groups[header.ID][physical] = true
		}
	}
	result := map[string]*rollout{}
	for _, logical := range sortedIDs(groups) {
		members := groups[logical]
		referenced := map[string]bool{}
		for physical := range members {
			if base := headers[physical].HistoryBase; base != nil {
				referenced[base.ThreadID] = true
			}
		}
		var tips []string
		for physical := range members {
			if !referenced[physical] {
				tips = append(tips, physical)
			}
		}
		if len(tips) != 1 {
			return nil, fmt.Errorf("ambiguous or cyclic physical rollouts for session %s; cannot select a current rollout", logical)
		}
		tip, err := readRollout(paths[tips[0]], projectRoot, nil)
		if err != nil {
			return nil, err
		}
		if err := attachHistory(tip, projectRoot, paths); err != nil {
			return nil, err
		}
		reached := map[string]bool{tip.RolloutID: true}
		for _, dependency := range tip.Dependencies {
			relative, err := filepath.Rel(home, dependency.Path)
			if err != nil || !managedRolloutPath(relative) {
				return nil, fmt.Errorf("unsafe profile dependency path %s", dependency.Path)
			}
			dependency.Relative = filepath.ToSlash(relative)
			reached[dependency.RolloutID] = true
		}
		for physical := range members {
			if !reached[physical] {
				return nil, fmt.Errorf("disconnected or cyclic physical rollouts for session %s", logical)
			}
		}
		relative, err := filepath.Rel(home, tip.Path)
		if err != nil || !managedRolloutPath(relative) {
			return nil, fmt.Errorf("unsafe profile rollout path %s", tip.Path)
		}
		tip.Relative = filepath.ToSlash(relative)
		tip.Archived = strings.HasPrefix(tip.Relative, "archived_sessions/")
		tip.Name = names[logical]
		result[logical] = tip
	}
	return result, nil
}

// Snapshot discovery stays header-only. Full synchronization validates all
// referenced byte and ordinal boundaries before any transcript is used.
func scanProfileIDs(home, projectRoot string) (map[string]bool, error) {
	_, headers, err := profileCatalog(home)
	if err != nil {
		return nil, err
	}
	groups := map[string]map[string]bool{}
	for physical, header := range headers {
		if _, within := relativeWithin(projectRoot, header.InitialCWD); !within {
			continue
		}
		if header.ValidationError != nil {
			return nil, header.ValidationError
		}
		if groups[header.ID] == nil {
			groups[header.ID] = map[string]bool{}
		}
		groups[header.ID][physical] = true
	}
	result := map[string]bool{}
	for logical, members := range groups {
		referenced := map[string]bool{}
		for physical := range members {
			if base := headers[physical].HistoryBase; base != nil {
				referenced[base.ThreadID] = true
			}
		}
		var tip string
		for physical := range members {
			if !referenced[physical] {
				if tip != "" {
					return nil, fmt.Errorf("ambiguous physical rollouts for session %s", logical)
				}
				tip = physical
			}
		}
		if tip == "" {
			return nil, fmt.Errorf("cyclic physical rollouts for session %s", logical)
		}
		seen := map[string]bool{}
		for current := tip; current != ""; {
			if seen[current] {
				return nil, fmt.Errorf("history dependency cycle at rollout %s", current)
			}
			seen[current] = true
			header, exists := headers[current]
			if !exists {
				return nil, fmt.Errorf("missing history source rollout %s", current)
			}
			if header.ValidationError != nil {
				return nil, header.ValidationError
			}
			current = ""
			if header.HistoryBase != nil {
				current = header.HistoryBase.ThreadID
			}
		}
		for physical := range members {
			if !seen[physical] {
				return nil, fmt.Errorf("disconnected physical rollouts for session %s", logical)
			}
		}
		result[logical] = true
	}
	return result, nil
}

func regularFiles(root string, include func(string) bool) ([]string, error) {
	if err := rejectSymlink(root); err != nil {
		return nil, err
	}
	if _, err := os.Stat(root); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var files []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("refusing symbolic link %s", path)
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return fmt.Errorf("refusing non-regular file %s", path)
		}
		if include(path) {
			files = append(files, path)
		}
		return nil
	})
	sort.Strings(files)
	return files, err
}

func isRolloutFile(path string) bool {
	base := filepath.Base(path)
	return strings.HasPrefix(base, "rollout-") &&
		(strings.HasSuffix(base, ".jsonl") || strings.HasSuffix(base, ".jsonl.zst"))
}

func filenameHasSessionID(path, id string) bool {
	_, err := physicalRolloutID(path, id)
	return err == nil
}

type sessionIndexEntry struct {
	ID         string `json:"id"`
	ThreadName string `json:"thread_name"`
	UpdatedAt  string `json:"updated_at"`
}

func readSessionNames(home string) (map[string]string, error) {
	path := filepath.Join(home, "session_index.jsonl")
	if err := rejectSymlink(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close()
	names := map[string]string{}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		var entry sessionIndexEntry
		if json.Unmarshal(scanner.Bytes(), &entry) == nil && validUUID(entry.ID) {
			names[entry.ID] = entry.ThreadName
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return names, nil
}

func appendSessionName(home, id, name string) error {
	names, err := readSessionNames(home)
	if err != nil {
		return err
	}
	if current, exists := names[id]; (exists && current == name) || (!exists && name == "") {
		return nil
	}
	entry := sessionIndexEntry{ID: id, ThreadName: name, UpdatedAt: nowRFC3339()}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	path := filepath.Join(home, "session_index.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(append(data, '\n')); err != nil {
		return err
	}
	return file.Sync()
}

func readJSON(path string, value any) error {
	if err := rejectSymlink(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("invalid JSON %s: %w", path, err)
	}
	return nil
}
