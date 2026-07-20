package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/gofrs/flock"
)

const (
	FormatVersion = 1

	projectDirName  = ".codexm"
	projectFileName = "project.json"

	attributesBegin = "# BEGIN codexm session management"
	attributesEnd   = "# END codexm session management"
)

var ErrNotInitialized = errors.New("project session management is not initialized")

type Project struct {
	FormatVersion int    `json:"format_version"`
	ProjectID     string `json:"project_id"`
	InitializedAt string `json:"initialized_at"`
}

type InitOptions struct {
	ProjectRoot    string
	ProfileHome    string
	ManagerHome    string
	ImportExisting bool
	Now            time.Time
}

type InitResult struct {
	Project Project
	Sync    Result
	Created bool
}

func Init(opts InitOptions) (InitResult, error) {
	root, err := normalizeExistingDir(opts.ProjectRoot)
	if err != nil {
		return InitResult{}, err
	}
	if opts.ManagerHome == "" {
		return InitResult{}, errors.New("manager home is required")
	}
	unlock, err := acquireLock(opts.ManagerHome, "init-"+pathKey(root))
	if err != nil {
		return InitResult{}, err
	}
	defer unlock()

	dotDir := filepath.Join(root, projectDirName)
	if err := rejectSymlink(dotDir); err != nil {
		return InitResult{}, err
	}
	if err := os.MkdirAll(dotDir, 0o755); err != nil {
		return InitResult{}, fmt.Errorf("create %s: %w", dotDir, err)
	}

	projectPath := filepath.Join(dotDir, projectFileName)
	if err := rejectSymlink(projectPath); err != nil {
		return InitResult{}, err
	}
	var project Project
	created := false
	data, err := os.ReadFile(projectPath)
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &project); err != nil {
			return InitResult{}, fmt.Errorf("invalid project file %s: %w", projectPath, err)
		}
		if err := validateProject(project, projectPath); err != nil {
			return InitResult{}, err
		}
	case errors.Is(err, os.ErrNotExist):
		now := opts.Now
		if now.IsZero() {
			now = time.Now()
		}
		id, err := newUUID()
		if err != nil {
			return InitResult{}, err
		}
		project = Project{
			FormatVersion: FormatVersion,
			ProjectID:     id,
			InitializedAt: now.UTC().Format(time.RFC3339Nano),
		}
		if err := atomicWriteJSON(projectPath, project, 0o644); err != nil {
			return InitResult{}, err
		}
		created = true
	default:
		return InitResult{}, fmt.Errorf("read %s: %w", projectPath, err)
	}

	for _, dir := range []string{"sessions", "archived_sessions", "metadata", "tombstones"} {
		path := filepath.Join(dotDir, dir)
		if err := rejectSymlink(path); err != nil {
			return InitResult{}, err
		}
		if err := os.MkdirAll(path, 0o755); err != nil {
			return InitResult{}, fmt.Errorf("create %s: %w", path, err)
		}
	}
	if err := ensureAttributes(filepath.Join(dotDir, ".gitattributes")); err != nil {
		return InitResult{}, err
	}

	result := InitResult{Project: project, Created: created}
	if opts.ImportExisting {
		if opts.ProfileHome == "" {
			return InitResult{}, errors.New("profile home is required with --import-existing")
		}
		result.Sync, err = Sync(Options{
			ProjectRoot:    root,
			ProfileHome:    opts.ProfileHome,
			ManagerHome:    opts.ManagerHome,
			ImportExisting: true,
		})
		if err != nil {
			return result, err
		}
	}
	return result, nil
}

func FindProject(start string) (string, Project, error) {
	path, err := normalizeExistingDir(start)
	if err != nil {
		return "", Project{}, err
	}
	for {
		dotDir := filepath.Join(path, projectDirName)
		if err := rejectSymlink(dotDir); err != nil {
			return "", Project{}, err
		}
		projectPath := filepath.Join(dotDir, projectFileName)
		if err := rejectSymlink(projectPath); err != nil {
			return "", Project{}, err
		}
		data, readErr := os.ReadFile(projectPath)
		if readErr == nil {
			var project Project
			if err := json.Unmarshal(data, &project); err != nil {
				return "", Project{}, fmt.Errorf("invalid project file %s: %w", projectPath, err)
			}
			if err := validateProject(project, projectPath); err != nil {
				return "", Project{}, err
			}
			return path, project, nil
		}
		if !errors.Is(readErr, os.ErrNotExist) {
			return "", Project{}, fmt.Errorf("read %s: %w", projectPath, readErr)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return "", Project{}, ErrNotInitialized
		}
		path = parent
	}
}

func validateProject(project Project, path string) error {
	if project.FormatVersion != FormatVersion {
		return fmt.Errorf("unsupported project format version %d in %s", project.FormatVersion, path)
	}
	if !validUUID(project.ProjectID) {
		return fmt.Errorf("invalid project_id %q in %s", project.ProjectID, path)
	}
	if _, err := time.Parse(time.RFC3339Nano, project.InitializedAt); err != nil {
		return fmt.Errorf("invalid initialized_at in %s: %w", path, err)
	}
	return nil
}

func ensureAttributes(path string) error {
	if err := rejectSymlink(path); err != nil {
		return err
	}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	text := string(data)
	begin := strings.Index(text, attributesBegin)
	end := strings.Index(text, attributesEnd)
	if (begin >= 0) != (end >= 0) || (begin >= 0 && end < begin) {
		return fmt.Errorf("invalid managed block in %s", path)
	}
	block := attributesBegin + "\n" +
		"sessions/** -merge\n" +
		"archived_sessions/** -merge\n" +
		"metadata/** -merge\n" +
		"tombstones/** -merge\n" +
		attributesEnd
	if begin >= 0 {
		end += len(attributesEnd)
		text = text[:begin] + block + text[end:]
	} else {
		if text != "" && !strings.HasSuffix(text, "\n") {
			text += "\n"
		}
		if text != "" {
			text += "\n"
		}
		text += block + "\n"
	}
	return atomicWrite(path, []byte(text), 0o644)
}

func acquireLock(managerHome, name string) (func(), error) {
	dir := filepath.Join(managerHome, "session-locks")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create session lock directory: %w", err)
	}
	lock := flock.New(filepath.Join(dir, name+".lock"))
	locked, err := lock.TryLock()
	if err != nil {
		return nil, fmt.Errorf("acquire session lock: %w", err)
	}
	if !locked {
		return nil, errors.New("another codexm session synchronization is already running")
	}
	return func() { _ = lock.Unlock() }, nil
}

func newUUID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate project id: %w", err)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	dst := make([]byte, 36)
	hex.Encode(dst[0:8], b[0:4])
	dst[8] = '-'
	hex.Encode(dst[9:13], b[4:6])
	dst[13] = '-'
	hex.Encode(dst[14:18], b[6:8])
	dst[18] = '-'
	hex.Encode(dst[19:23], b[8:10])
	dst[23] = '-'
	hex.Encode(dst[24:36], b[10:16])
	return string(dst), nil
}

func validUUID(id string) bool {
	if len(id) != 36 {
		return false
	}
	for i, r := range id {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if r != '-' {
				return false
			}
			continue
		}
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f')) {
			return false
		}
	}
	return true
}

func normalizeExistingDir(path string) (string, error) {
	if path == "" {
		var err error
		path, err = os.Getwd()
		if err != nil {
			return "", err
		}
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", fmt.Errorf("resolve project path %s: %w", abs, err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%s is not a directory", resolved)
	}
	if runtime.GOOS == "windows" {
		resolved = strings.ToLower(resolved)
	}
	return filepath.Clean(resolved), nil
}

func rejectSymlink(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing symbolic link %s", path)
	}
	return nil
}

func pathKey(path string) string {
	sum := checksumBytes([]byte(filepath.Clean(path)))
	return sum[:24]
}
