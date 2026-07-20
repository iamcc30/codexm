package session

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/klauspost/compress/zstd"
)

const DefaultAuditMaxFileSize int64 = 25 * 1024 * 1024

type AuditOptions struct {
	ProjectRoot string
	MaxFileSize int64
}

type AuditFinding struct {
	Severity string `json:"severity"`
	Kind     string `json:"kind"`
	Path     string `json:"path"`
	Line     int    `json:"line,omitempty"`
	Message  string `json:"message"`
}

type AuditResult struct {
	ProjectRoot string         `json:"project_root"`
	Files       int            `json:"files"`
	Bytes       int64          `json:"bytes"`
	Errors      int            `json:"errors"`
	Warnings    int            `json:"warnings"`
	Findings    []AuditFinding `json:"findings,omitempty"`
}

func (r AuditResult) HasFailures(strict bool) bool {
	return r.Errors > 0 || (strict && r.Warnings > 0)
}

type secretPattern struct {
	kind string
	re   *regexp.Regexp
}

var auditSecretPatterns = []secretPattern{
	{kind: "openai_api_key", re: regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{20,}\b`)},
	{kind: "github_token", re: regexp.MustCompile(`\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}\b`)},
	{kind: "github_token", re: regexp.MustCompile(`\bgithub_pat_[A-Za-z0-9_]{20,}\b`)},
	{kind: "aws_access_key", re: regexp.MustCompile(`\b(?:AKIA|ASIA)[A-Z0-9]{16}\b`)},
	{kind: "slack_token", re: regexp.MustCompile(`\bxox[baprs]-[A-Za-z0-9-]{16,}\b`)},
	{kind: "private_key", re: regexp.MustCompile(`-----BEGIN (?:RSA |EC |OPENSSH |DSA )?PRIVATE KEY-----`)},
	{kind: "bearer_token", re: regexp.MustCompile(`(?i)\bBearer[ \t]+[A-Za-z0-9._~+/=-]{12,}`)},
	{kind: "credential_assignment", re: regexp.MustCompile(`(?i)(?:api[_-]?key|client[_-]?secret|access[_-]?token|password|passwd|authorization)["' \t]*[:=]["' \t]*[A-Za-z0-9_./+=:-]{8,}`)},
}

var entropyCandidate = regexp.MustCompile(`[A-Za-z0-9_+/=-]{32,}`)

func Audit(opts AuditOptions) (AuditResult, error) {
	root, _, err := FindProject(opts.ProjectRoot)
	if err != nil {
		return AuditResult{}, err
	}
	maxFileSize := opts.MaxFileSize
	if maxFileSize <= 0 {
		maxFileSize = DefaultAuditMaxFileSize
	}
	dotDir := filepath.Join(root, projectDirName)
	mappings, metadataFiles, err := auditMappings(dotDir)
	if err != nil {
		return AuditResult{}, err
	}
	result := AuditResult{ProjectRoot: root}
	for _, path := range metadataFiles {
		relative, err := filepath.Rel(dotDir, path)
		if err != nil || !safeRelative(relative) {
			return result, fmt.Errorf("unsafe audit path %s", path)
		}
		result.Files++
		if err := auditFileSecrets(path, filepath.ToSlash(relative), &result); err != nil {
			return result, err
		}
	}
	for _, dir := range []string{"sessions", "archived_sessions"} {
		files, err := regularFiles(filepath.Join(dotDir, dir), isRolloutFile)
		if err != nil {
			return result, err
		}
		for _, path := range files {
			relative, err := filepath.Rel(dotDir, path)
			if err != nil || !safeRelative(relative) {
				return result, fmt.Errorf("unsafe audit path %s", path)
			}
			relative = filepath.ToSlash(relative)
			result.Files++
			if err := auditRollout(path, relative, mappings[relative], maxFileSize, &result); err != nil {
				return result, err
			}
		}
	}
	if _, err := scanProject(root); err != nil {
		addAuditFinding(&result, AuditFinding{
			Severity: "error", Kind: "mirror_validation", Path: projectDirName,
			Message: "session mirror structure or sidecar validation failed: " + err.Error(),
		})
	}
	sort.Slice(result.Findings, func(i, j int) bool {
		left, right := result.Findings[i], result.Findings[j]
		if left.Path != right.Path {
			return left.Path < right.Path
		}
		if left.Line != right.Line {
			return left.Line < right.Line
		}
		return left.Kind < right.Kind
	})
	return result, nil
}

func auditMappings(dotDir string) (map[string]map[int]bool, []string, error) {
	result := map[string]map[int]bool{}
	files, err := regularFiles(filepath.Join(dotDir, "metadata"), func(path string) bool {
		return strings.HasSuffix(path, ".json")
	})
	if err != nil {
		return nil, nil, err
	}
	for _, path := range files {
		var metadata Metadata
		if err := readJSON(path, &metadata); err != nil {
			return nil, nil, err
		}
		if metadata.FormatVersion != FormatVersion || !validUUID(metadata.SessionID) || !safeRelative(metadata.RolloutPath) {
			return nil, nil, fmt.Errorf("invalid session metadata %s", path)
		}
		rolloutPath := filepath.ToSlash(filepath.Clean(filepath.FromSlash(metadata.RolloutPath)))
		lines := map[int]bool{}
		for _, mapping := range metadata.CWDs {
			if mapping.Line < 1 || !safeRelative(mapping.Path) {
				return nil, nil, fmt.Errorf("invalid cwd mapping in %s", path)
			}
			lines[mapping.Line] = true
		}
		result[rolloutPath] = lines
	}
	return result, files, nil
}

func auditFileSecrets(path, relative string, result *AuditResult) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	lineNo := 0
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			result.Bytes += int64(len(line))
			auditLineSecrets(relative, lineNo, bytes.TrimSuffix(line, []byte{'\n'}), result)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil
			}
			return fmt.Errorf("read %s: %w", path, readErr)
		}
	}
}

func auditRollout(path, relative string, mappedLines map[int]bool, maxFileSize int64, result *AuditResult) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	var reader io.Reader = file
	var decoder *zstd.Decoder
	if strings.HasSuffix(path, ".zst") {
		decoder, err = zstd.NewReader(file)
		if err != nil {
			return fmt.Errorf("open zstandard session %s: %w", path, err)
		}
		defer decoder.Close()
		reader = decoder
	}
	buffered := bufio.NewReader(reader)
	lineNo := 0
	fileBytes := int64(0)
	largeReported := false
	physicalLarge := false
	if info, statErr := file.Stat(); statErr == nil {
		physicalLarge = info.Size() > maxFileSize
	}
	usedMappings := map[int]bool{}
	for {
		line, readErr := buffered.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			fileBytes += int64(len(line))
			result.Bytes += int64(len(line))
			if !largeReported && (physicalLarge || fileBytes > maxFileSize) {
				addAuditFinding(result, AuditFinding{
					Severity: "warning", Kind: "large_transcript", Path: relative,
					Message: fmt.Sprintf("decompressed transcript exceeds %d MiB", maxFileSize/(1024*1024)),
				})
				largeReported = true
			}
			auditLineSecrets(relative, lineNo, bytes.TrimSuffix(line, []byte{'\n'}), result)
			auditStructuredCWD(relative, lineNo, bytes.TrimSuffix(line, []byte{'\n'}), mappedLines, usedMappings, result)
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				if len(line) > 0 && line[len(line)-1] != '\n' {
					addAuditFinding(result, AuditFinding{Severity: "error", Kind: "invalid_jsonl", Path: relative, Line: lineNo, Message: "truncated JSONL line"})
				}
				break
			}
			return fmt.Errorf("read %s: %w", path, readErr)
		}
	}
	for line := range mappedLines {
		if !usedMappings[line] {
			addAuditFinding(result, AuditFinding{Severity: "error", Kind: "invalid_cwd_mapping", Path: relative, Line: line, Message: "metadata mapping does not reference a structured cwd"})
		}
	}
	return nil
}

func auditLineSecrets(path string, lineNo int, line []byte, result *AuditResult) {
	foundSecret := false
	for _, pattern := range auditSecretPatterns {
		if pattern.re.Match(line) {
			addAuditFinding(result, AuditFinding{Severity: "error", Kind: pattern.kind, Path: path, Line: lineNo, Message: "possible credential detected; matched value is intentionally hidden"})
			foundSecret = true
		}
	}
	if foundSecret {
		return
	}
	for _, candidate := range entropyCandidate.FindAll(line, -1) {
		if likelyHighEntropy(candidate) {
			addAuditFinding(result, AuditFinding{Severity: "warning", Kind: "high_entropy_value", Path: path, Line: lineNo, Message: "possible encoded secret detected; matched value is intentionally hidden"})
			return
		}
	}
}

func auditStructuredCWD(path string, lineNo int, line []byte, mappedLines, usedMappings map[int]bool, result *AuditResult) {
	value, err := decodeObject(line)
	if err != nil {
		addAuditFinding(result, AuditFinding{Severity: "error", Kind: "invalid_jsonl", Path: path, Line: lineNo, Message: "line is not a valid JSON object"})
		return
	}
	recordType := stringValue(value["type"])
	if recordType != "session_meta" && recordType != "turn_context" {
		return
	}
	payload, ok := value["payload"].(map[string]any)
	if !ok {
		return
	}
	cwd, _ := payload["cwd"].(string)
	if cwd == "" {
		return
	}
	if mappedLines[lineNo] {
		usedMappings[lineNo] = true
		return
	}
	if portableAbsolutePath(cwd) {
		addAuditFinding(result, AuditFinding{Severity: "error", Kind: "unmapped_absolute_cwd", Path: path, Line: lineNo, Message: "structured cwd is absolute but has no portable metadata mapping"})
	}
}

func portableAbsolutePath(value string) bool {
	return filepath.IsAbs(value) || windowsDriveAbsolute(value) || strings.HasPrefix(value, `\\`)
}

func likelyHighEntropy(value []byte) bool {
	if len(value) < 32 {
		return false
	}
	categories := 0
	var lower, upper, digit, symbol bool
	counts := map[byte]int{}
	for _, char := range value {
		counts[char]++
		switch {
		case char >= 'a' && char <= 'z':
			lower = true
		case char >= 'A' && char <= 'Z':
			upper = true
		case char >= '0' && char <= '9':
			digit = true
		default:
			symbol = true
		}
	}
	for _, present := range []bool{lower, upper, digit, symbol} {
		if present {
			categories++
		}
	}
	if categories < 3 {
		return false
	}
	entropy := 0.0
	length := float64(len(value))
	for _, count := range counts {
		probability := float64(count) / length
		entropy -= probability * math.Log2(probability)
	}
	return entropy >= 4.3
}

func addAuditFinding(result *AuditResult, finding AuditFinding) {
	result.Findings = append(result.Findings, finding)
	if finding.Severity == "error" {
		result.Errors++
	} else {
		result.Warnings++
	}
}
