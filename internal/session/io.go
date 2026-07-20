package session

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	pathpkg "path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/klauspost/compress/zstd"
)

type CWDMapping struct {
	Line int    `json:"line"`
	Path string `json:"path"`
}

type Metadata struct {
	FormatVersion int          `json:"format_version"`
	SessionID     string       `json:"session_id"`
	RolloutPath   string       `json:"rollout_path"`
	Checksum      string       `json:"checksum"`
	LineCount     int          `json:"line_count"`
	Archived      bool         `json:"archived"`
	Name          string       `json:"name,omitempty"`
	CWDs          []CWDMapping `json:"cwd_mappings,omitempty"`
}

type Tombstone struct {
	FormatVersion int    `json:"format_version"`
	SessionID     string `json:"session_id"`
	DeletedAt     string `json:"deleted_at"`
	Checksum      string `json:"checksum,omitempty"`
}

type rollout struct {
	ID          string
	Path        string
	Relative    string
	Archived    bool
	Name        string
	Timestamp   string
	InitialCWD  string
	RawLines    [][]byte
	Canonical   [][]byte
	CWDs        []CWDMapping
	Checksum    string
	LineCount   int
	Compressed  bool
	ProjectMeta Metadata
}

type rolloutHeader struct {
	ID         string
	Timestamp  string
	InitialCWD string
}

// readRolloutHeader reads only the first JSONL record. Profile discovery uses
// it to reject sessions belonging to other projects before allocating and
// canonicalizing the entire transcript.
func readRolloutHeader(path string) (rolloutHeader, error) {
	if err := rejectSymlink(path); err != nil {
		return rolloutHeader{}, err
	}
	file, err := os.Open(path)
	if err != nil {
		return rolloutHeader{}, err
	}
	defer file.Close()
	var reader io.Reader = file
	var decoder *zstd.Decoder
	if strings.HasSuffix(path, ".zst") {
		decoder, err = zstd.NewReader(file)
		if err != nil {
			return rolloutHeader{}, fmt.Errorf("open zstandard session %s: %w", path, err)
		}
		defer decoder.Close()
		reader = decoder
	}
	line, err := bufio.NewReader(reader).ReadBytes('\n')
	if err != nil {
		if errors.Is(err, io.EOF) && len(line) > 0 {
			return rolloutHeader{}, fmt.Errorf("truncated JSONL session %s at line 1", path)
		}
		return rolloutHeader{}, fmt.Errorf("read session header %s: %w", path, err)
	}
	if len(bytes.TrimSpace(line)) == 0 {
		return rolloutHeader{}, fmt.Errorf("blank JSONL line in %s at line 1", path)
	}
	value, err := decodeObject(line[:len(line)-1])
	if err != nil {
		return rolloutHeader{}, fmt.Errorf("invalid JSONL session %s at line 1: %w", path, err)
	}
	id, timestamp, cwd, err := parseSessionMeta(value)
	if err != nil {
		return rolloutHeader{}, fmt.Errorf("%s: %w", path, err)
	}
	return rolloutHeader{ID: id, Timestamp: timestamp, InitialCWD: cwd}, nil
}

func readRollout(path, projectRoot string, mappings []CWDMapping) (*rollout, error) {
	if err := rejectSymlink(path); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var reader io.Reader = file
	compressed := strings.HasSuffix(path, ".zst")
	var decoder *zstd.Decoder
	if compressed {
		decoder, err = zstd.NewReader(file)
		if err != nil {
			return nil, fmt.Errorf("open zstandard session %s: %w", path, err)
		}
		defer decoder.Close()
		reader = decoder
	}

	mappingByLine := make(map[int]string, len(mappings))
	useStoredMappings := mappings != nil
	for _, mapping := range mappings {
		if mapping.Line < 1 || !safeRelative(mapping.Path) {
			return nil, fmt.Errorf("invalid cwd mapping in %s", path)
		}
		if _, exists := mappingByLine[mapping.Line]; exists {
			return nil, fmt.Errorf("duplicate cwd mapping line %d in %s", mapping.Line, path)
		}
		mappingByLine[mapping.Line] = mapping.Path
	}

	result := &rollout{Path: path, Compressed: compressed}
	buffered := bufio.NewReader(reader)
	lineNo := 0
	usedMappings := 0
	for {
		line, readErr := buffered.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			if line[len(line)-1] != '\n' {
				return nil, fmt.Errorf("truncated JSONL session %s at line %d", path, lineNo)
			}
			raw := append([]byte(nil), line[:len(line)-1]...)
			if len(bytes.TrimSpace(raw)) == 0 {
				return nil, fmt.Errorf("blank JSONL line in %s at line %d", path, lineNo)
			}
			value, err := decodeObject(raw)
			if err != nil {
				return nil, fmt.Errorf("invalid JSONL session %s at line %d: %w", path, lineNo, err)
			}
			canonicalValue := cloneObject(value)
			if lineNo == 1 {
				id, timestamp, cwd, err := parseSessionMeta(value)
				if err != nil {
					return nil, fmt.Errorf("%s: %w", path, err)
				}
				result.ID = id
				result.Timestamp = timestamp
				result.InitialCWD = cwd
			}
			if mapped, ok := mappingByLine[lineNo]; ok {
				if !rewriteCWD(canonicalValue, projectRoot, mapped, true) {
					return nil, fmt.Errorf("cwd mapping line %d does not reference session_meta or turn_context in %s", lineNo, path)
				}
				usedMappings++
			} else if !useStoredMappings {
				if mapped, ok := portableCWD(canonicalValue, projectRoot); ok {
					result.CWDs = append(result.CWDs, CWDMapping{Line: lineNo, Path: mapped})
				}
			}
			canonical, err := json.Marshal(canonicalValue)
			if err != nil {
				return nil, err
			}
			result.RawLines = append(result.RawLines, raw)
			result.Canonical = append(result.Canonical, canonical)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return nil, fmt.Errorf("read session %s: %w", path, readErr)
		}
	}
	if lineNo == 0 {
		return nil, fmt.Errorf("empty JSONL session %s", path)
	}
	if usedMappings != len(mappings) {
		return nil, fmt.Errorf("invalid cwd mappings in %s", path)
	}
	result.LineCount = lineNo
	result.Checksum = checksumLines(result.Canonical)
	return result, nil
}

func parseSessionMeta(value map[string]any) (string, string, string, error) {
	if stringValue(value["type"]) != "session_meta" {
		return "", "", "", errors.New("first JSONL record is not session_meta")
	}
	payload, ok := value["payload"].(map[string]any)
	if !ok {
		return "", "", "", errors.New("session_meta payload is missing")
	}
	id := stringValue(payload["id"])
	if id == "" {
		id = stringValue(payload["session_id"])
	}
	if !validUUID(id) {
		return "", "", "", fmt.Errorf("invalid session id %q", id)
	}
	return id, stringValue(payload["timestamp"]), stringValue(payload["cwd"]), nil
}

func portableCWD(value map[string]any, root string) (string, bool) {
	recordType := stringValue(value["type"])
	if recordType != "session_meta" && recordType != "turn_context" {
		return "", false
	}
	payload, ok := value["payload"].(map[string]any)
	if !ok {
		return "", false
	}
	cwd, ok := payload["cwd"].(string)
	if !ok || cwd == "" {
		return "", false
	}
	rel, ok := relativeWithin(root, cwd)
	if !ok {
		return "", false
	}
	payload["cwd"] = portableRoot(rel)
	return filepath.ToSlash(rel), true
}

func rewriteCWD(value map[string]any, root, rel string, canonical bool) bool {
	recordType := stringValue(value["type"])
	if recordType != "session_meta" && recordType != "turn_context" {
		return false
	}
	payload, ok := value["payload"].(map[string]any)
	if !ok {
		return false
	}
	if _, ok := payload["cwd"].(string); !ok {
		return false
	}
	if canonical {
		payload["cwd"] = portableRoot(filepath.FromSlash(rel))
	} else {
		payload["cwd"] = filepath.Join(root, filepath.FromSlash(rel))
	}
	return true
}

func portableRoot(rel string) string {
	if rel == "." || rel == "" {
		return "$CODEXM_PROJECT_ROOT"
	}
	return "$CODEXM_PROJECT_ROOT/" + filepath.ToSlash(rel)
}

func relativeWithin(root, path string) (string, bool) {
	if !filepath.IsAbs(path) {
		return "", false
	}
	cleanRoot := filepath.Clean(root)
	cleanPath := filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(cleanRoot); err == nil {
		cleanRoot = resolved
	}
	if resolved, err := filepath.EvalSymlinks(cleanPath); err == nil {
		cleanPath = resolved
	}
	if runtime.GOOS == "windows" {
		cleanRoot = strings.ToLower(cleanRoot)
		cleanPath = strings.ToLower(cleanPath)
	}
	rel, err := filepath.Rel(cleanRoot, cleanPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", false
	}
	if rel == "" {
		rel = "."
	}
	return rel, true
}

func safeRelative(path string) bool {
	if path == "" || filepath.IsAbs(path) {
		return false
	}
	clean := filepath.Clean(filepath.FromSlash(path))
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return false
	}
	portable := strings.ReplaceAll(path, `\`, "/")
	if strings.HasPrefix(portable, "/") || windowsDriveAbsolute(portable) {
		return false
	}
	portable = pathpkg.Clean(portable)
	return portable != ".." && !strings.HasPrefix(portable, "../")
}

func windowsDriveAbsolute(path string) bool {
	if len(path) < 3 || path[1] != ':' || (path[2] != '/' && path[2] != '\\') {
		return false
	}
	letter := path[0]
	return letter >= 'a' && letter <= 'z' || letter >= 'A' && letter <= 'Z'
}

func cloneObject(value map[string]any) map[string]any {
	data, _ := json.Marshal(value)
	cloned, _ := decodeObject(data)
	return cloned
}

func materializeLines(source *rollout, root string) ([][]byte, error) {
	mapped := make(map[int]string, len(source.CWDs))
	for _, item := range source.CWDs {
		mapped[item.Line] = item.Path
	}
	lines := make([][]byte, 0, len(source.RawLines))
	for index, raw := range source.RawLines {
		lineNo := index + 1
		rel, ok := mapped[lineNo]
		if !ok {
			lines = append(lines, append([]byte(nil), raw...))
			continue
		}
		value, err := decodeObject(raw)
		if err != nil {
			return nil, err
		}
		if !rewriteCWD(value, root, rel, false) {
			return nil, fmt.Errorf("invalid cwd mapping at line %d", lineNo)
		}
		line, err := json.Marshal(value)
		if err != nil {
			return nil, err
		}
		lines = append(lines, line)
	}
	return lines, nil
}

func decodeObject(data []byte) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return nil, errors.New("multiple JSON values on one JSONL line")
		}
		return nil, err
	}
	return value, nil
}

func writeRollout(path string, lines [][]byte, compressed bool) error {
	var plain bytes.Buffer
	for _, line := range lines {
		plain.Write(line)
		plain.WriteByte('\n')
	}
	data := plain.Bytes()
	if compressed {
		var encoded bytes.Buffer
		writer, err := zstd.NewWriter(&encoded)
		if err != nil {
			return err
		}
		if _, err := writer.Write(data); err != nil {
			writer.Close()
			return err
		}
		if err := writer.Close(); err != nil {
			return err
		}
		data = encoded.Bytes()
	}
	return atomicWrite(path, data, 0o600)
}

func atomicWriteJSON(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(path, append(data, '\n'), mode)
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create parent directory for %s: %w", path, err)
	}
	if err := rejectSymlink(path); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary file for %s: %w", path, err)
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()
	if runtime.GOOS != "windows" {
		if err := temp.Chmod(mode); err != nil {
			return err
		}
	}
	if _, err := temp.Write(data); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
	ok = true
	return nil
}

func checksumLines(lines [][]byte) string {
	hash := sha256.New()
	for _, line := range lines {
		hash.Write(line)
		hash.Write([]byte{'\n'})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func checksumBytes(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}
