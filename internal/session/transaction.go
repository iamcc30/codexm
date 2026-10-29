package session

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

const transactionFile = "pending-transaction.json"
const profileTransactionFile = ".codexm-pending-transaction.json"

type transactionStore struct {
	base, projectRoot, journal string
	profile                    bool
}

func projectStore(root string) transactionStore {
	return transactionStore{base: filepath.Join(root, projectDirName), projectRoot: root, journal: transactionFile}
}
func profileStore(root, home string) transactionStore {
	return transactionStore{base: home, projectRoot: root, journal: profileTransactionFile, profile: true}
}
func (store transactionStore) journalPath() string { return filepath.Join(store.base, store.journal) }
func recoverProfileTransaction(root, home string) error {
	return recoverTransaction(profileStore(root, home))
}
func applyProfileTransaction(root, home string, ops []projectMutation) error {
	return applyTransaction(profileStore(root, home), ops)
}

type projectMutation struct {
	Path   string      `json:"path"`
	Data   []byte      `json:"data,omitempty"`
	Delete bool        `json:"delete,omitempty"`
	Mode   os.FileMode `json:"mode"`
	Before string      `json:"before,omitempty"` // empty means the path did not exist
}
type projectTransaction struct {
	Version     int               `json:"version"`
	ProjectID   string            `json:"project_id"`
	ProjectRoot string            `json:"project_root"`
	Operations  []projectMutation `json:"operations"`
}

func transactionPath(root string) string { return filepath.Join(root, projectDirName, transactionFile) }

// Every component is checked: a journal must never follow a symlink out of the mirror.
func transactionTarget(root, relative string) (string, error) {
	return projectStore(root).target(relative)
}
func (store transactionStore) target(relative string) (string, error) {
	if relative == "" || strings.Contains(relative, "\\") || filepath.ToSlash(filepath.Clean(filepath.FromSlash(relative))) != relative || !safeRelative(relative) {
		return "", fmt.Errorf("unsafe transaction path %q", relative)
	}
	parts := strings.Split(relative, "/")
	valid := len(parts) >= 2 && (parts[0] == "sessions" || parts[0] == "archived_sessions") && isRolloutFile(relative)
	if !store.profile && len(parts) == 2 && (parts[0] == "metadata" || parts[0] == "tombstones") {
		valid = strings.HasSuffix(parts[1], ".json") && validUUID(strings.TrimSuffix(parts[1], ".json"))
	}
	if store.profile && relative == "session_index.jsonl" {
		valid = true
	}
	if !valid {
		return "", fmt.Errorf("unmanaged transaction path %q", relative)
	}
	target := store.base
	if err := rejectSymlink(target); err != nil {
		return "", err
	}
	for _, part := range parts {
		target = filepath.Join(target, part)
		if err := rejectSymlink(target); err != nil {
			return "", err
		}
	}
	return target, nil
}

func transactionDigest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return checksumBytes(data), nil
}

// Directory fsync makes both the journal and completed renames durable on POSIX.
// Windows does not expose directory fsync through os.File; file contents still sync.
func (store transactionStore) syncParents(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	stop := store.base
	for dir := filepath.Dir(path); ; dir = filepath.Dir(dir) {
		f, err := os.Open(dir)
		if err != nil {
			return err
		}
		err = f.Sync()
		closeErr := f.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if dir == stop {
			return nil
		}
		if dir == filepath.Dir(dir) {
			return errors.New("transaction escaped mirror")
		}
	}
}

func applyProjectTransaction(root string, operations []projectMutation) error {
	return applyTransaction(projectStore(root), operations)
}
func applyTransaction(store transactionStore, operations []projectMutation) error {
	if _, err := os.Lstat(store.journalPath()); !errors.Is(err, os.ErrNotExist) {
		if err != nil {
			return err
		}
		return errors.New("pending project transaction; run session sync to recover first")
	}
	var project Project
	if err := readJSON(filepath.Join(store.projectRoot, projectDirName, projectFileName), &project); err != nil {
		return err
	}
	tx := projectTransaction{Version: 1, ProjectID: project.ProjectID, ProjectRoot: store.projectRoot, Operations: operations}
	for i := range tx.Operations {
		path, err := store.target(tx.Operations[i].Path)
		if err != nil {
			return err
		}
		tx.Operations[i].Before, err = transactionDigest(path)
		if err != nil {
			return err
		}
	}
	if err := validateTransaction(store, tx); err != nil {
		return err
	}
	if err := atomicWriteJSON(store.journalPath(), tx, 0o600); err != nil {
		return err
	}
	if err := store.syncParents(store.journalPath()); err != nil {
		return err
	}
	return recoverTransaction(store)
}

func validateProjectTransaction(root string, tx projectTransaction) error {
	return validateTransaction(projectStore(root), tx)
}
func validateTransaction(store transactionStore, tx projectTransaction) error {
	var project Project
	if err := readJSON(filepath.Join(store.projectRoot, projectDirName, projectFileName), &project); err != nil {
		return err
	}
	if err := validateProject(project, filepath.Join(store.projectRoot, projectDirName, projectFileName)); err != nil {
		return err
	}
	if tx.Version != 1 || tx.ProjectID != project.ProjectID || !sameHistoryCWD(store.projectRoot, tx.ProjectRoot) || len(tx.Operations) == 0 {
		return errors.New("invalid project transaction identity or version")
	}
	seen := map[string]bool{}
	// Check every operation before applying any, including operations completed before a crash.
	for _, op := range tx.Operations {
		if seen[strings.ToLower(op.Path)] || (op.Mode != 0o600 && op.Mode != 0o644) || (op.Delete && len(op.Data) != 0) {
			return errors.New("invalid or duplicate project transaction operation")
		}
		seen[strings.ToLower(op.Path)] = true
		if op.Before != "" {
			digest, err := hex.DecodeString(op.Before)
			if err != nil || len(digest) != 32 {
				return errors.New("invalid transaction checksum")
			}
		}
		path, err := store.target(op.Path)
		if err != nil {
			return err
		}
		if err := store.validatePayload(op); err != nil {
			return err
		}
		current, err := transactionDigest(path)
		if err != nil {
			return err
		}
		desired := ""
		if !op.Delete {
			desired = checksumBytes(op.Data)
		}
		if current != op.Before && current != desired {
			return fmt.Errorf("transaction target changed independently: %s; preserve the journal and resolve manually", op.Path)
		}
	}
	return nil
}

func recoverProjectTransaction(root string) error { return recoverTransaction(projectStore(root)) }
func recoverTransaction(store transactionStore) error {
	path := store.journalPath()
	if err := rejectSymlink(store.base); err != nil {
		return err
	}
	if err := rejectSymlink(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var tx projectTransaction
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&tx); err != nil {
		return fmt.Errorf("invalid pending project transaction: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing project transaction data")
	}
	if err := validateTransaction(store, tx); err != nil {
		return err
	}
	for _, op := range tx.Operations {
		target, err := store.target(op.Path)
		if err != nil {
			return err
		}
		current, err := transactionDigest(target)
		if err != nil {
			return err
		}
		desired := ""
		if !op.Delete {
			desired = checksumBytes(op.Data)
		}
		if current != desired {
			if current != op.Before {
				return fmt.Errorf("transaction target changed independently: %s", op.Path)
			}
			if op.Delete {
				err = os.Remove(target)
			} else {
				err = atomicWrite(target, op.Data, op.Mode)
			}
			if err != nil {
				return err
			}
		}
		if err := store.syncParents(target); err != nil {
			return err
		}
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return store.syncParents(path)
}

func projectJSONMutation(path string, value any) (projectMutation, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	return projectMutation{Path: path, Data: append(data, '\n'), Mode: 0o644}, err
}

// Journals may arrive from an untrusted checkout. Never replay arbitrary payloads
// merely because their paths and old checksums happen to be valid.
func (store transactionStore) validatePayload(op projectMutation) error {
	if op.Delete {
		return nil
	}
	parts := strings.Split(op.Path, "/")
	switch parts[0] {
	case "session_index.jsonl":
		return validateSessionIndexData(op.Data)
	case "metadata":
		var metadata Metadata
		if err := json.Unmarshal(op.Data, &metadata); err != nil {
			return err
		}
		if !validUUID(metadata.SessionID) || parts[1] != metadata.SessionID+".json" || (metadata.FormatVersion != FormatVersion && metadata.FormatVersion != HistoryFormatVersion) || !managedRolloutPath(metadata.RolloutPath) {
			return fmt.Errorf("invalid transaction metadata %s", op.Path)
		}
	case "tombstones":
		var tombstone Tombstone
		if err := json.Unmarshal(op.Data, &tombstone); err != nil {
			return err
		}
		if !validUUID(tombstone.SessionID) || parts[1] != tombstone.SessionID+".json" || tombstone.FormatVersion != FormatVersion {
			return fmt.Errorf("invalid transaction tombstone %s", op.Path)
		}
	default:
		dir, err := os.MkdirTemp(store.base, ".codexm-transaction-validate-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, filepath.Base(op.Path))
		if err := os.WriteFile(path, op.Data, 0o600); err != nil {
			return err
		}
		if _, err := readRollout(path, store.projectRoot, nil); err != nil {
			return fmt.Errorf("invalid transaction rollout %s: %w", op.Path, err)
		}
	}
	return nil
}
