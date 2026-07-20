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
	dotDir := filepath.Join(root, projectDirName)
	metadataDir := filepath.Join(dotDir, "metadata")
	sessionByPath := map[string]*rollout{}
	state := projectState{
		Sessions:   map[string]*rollout{},
		Tombstones: map[string]Tombstone{},
	}

	metadataFiles, err := regularFiles(metadataDir, func(path string) bool {
		return strings.HasSuffix(path, ".json")
	})
	if err != nil {
		return state, err
	}
	for _, path := range metadataFiles {
		var metadata Metadata
		if err := readJSON(path, &metadata); err != nil {
			return state, err
		}
		if metadata.FormatVersion != FormatVersion {
			return state, fmt.Errorf("unsupported metadata format version %d in %s", metadata.FormatVersion, path)
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
	return state, nil
}

func scanProfile(home, projectRoot string) (map[string]*rollout, error) {
	names, err := readSessionNames(home)
	if err != nil {
		return nil, err
	}
	result := map[string]*rollout{}
	for _, collection := range []struct {
		dir      string
		archived bool
	}{
		{dir: "sessions"},
		{dir: "archived_sessions", archived: true},
	} {
		base := filepath.Join(home, collection.dir)
		files, err := regularFiles(base, isRolloutFile)
		if err != nil {
			return nil, err
		}
		for _, path := range files {
			header, err := readRolloutHeader(path)
			if err != nil {
				return nil, err
			}
			if !filenameHasSessionID(path, header.ID) {
				return nil, fmt.Errorf("rollout filename does not match session id in %s", path)
			}
			if _, within := relativeWithin(projectRoot, header.InitialCWD); !within {
				continue
			}
			session, err := readRollout(path, projectRoot, nil)
			if err != nil {
				return nil, err
			}
			if !filenameHasSessionID(path, session.ID) {
				return nil, fmt.Errorf("rollout filename does not match session id in %s", path)
			}
			if _, duplicate := result[session.ID]; duplicate {
				return nil, fmt.Errorf("duplicate session id %s in profile", session.ID)
			}
			relative, err := filepath.Rel(home, path)
			if err != nil || !safeRelative(relative) {
				return nil, fmt.Errorf("unsafe profile rollout path %s", path)
			}
			session.Relative = filepath.ToSlash(relative)
			session.Archived = collection.archived
			session.Name = names[session.ID]
			result[session.ID] = session
		}
	}
	return result, nil
}

// scanProfileIDs returns only sessions whose initial working directory belongs
// to projectRoot. It intentionally avoids loading complete transcripts and is
// used to detect sessions created during a codexm run.
func scanProfileIDs(home, projectRoot string) (map[string]bool, error) {
	result := map[string]bool{}
	for _, dir := range []string{"sessions", "archived_sessions"} {
		files, err := regularFiles(filepath.Join(home, dir), isRolloutFile)
		if err != nil {
			return nil, err
		}
		for _, path := range files {
			header, err := readRolloutHeader(path)
			if err != nil {
				return nil, err
			}
			if !filenameHasSessionID(path, header.ID) {
				return nil, fmt.Errorf("rollout filename does not match session id in %s", path)
			}
			if _, within := relativeWithin(projectRoot, header.InitialCWD); !within {
				continue
			}
			if result[header.ID] {
				return nil, fmt.Errorf("duplicate session id %s in profile", header.ID)
			}
			result[header.ID] = true
		}
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
	base := filepath.Base(path)
	return strings.HasSuffix(base, "-"+id+".jsonl") ||
		strings.HasSuffix(base, "-"+id+".jsonl.zst")
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
