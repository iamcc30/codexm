package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testSessionA = "11111111-1111-4111-8111-111111111111"
	testSessionB = "22222222-2222-4222-8222-222222222222"
)

func TestInitIsIdempotentFindsNearestProjectAndPreservesAttributes(t *testing.T) {
	base := t.TempDir()
	outer := filepath.Join(base, "outer")
	inner := filepath.Join(outer, "packages", "inner")
	mustMkdir(t, inner)
	mustWriteFile(t, filepath.Join(outer, ".codexm", ".gitattributes"), "*.bin binary\n")
	manager := filepath.Join(base, "manager")

	first, err := Init(InitOptions{ProjectRoot: outer, ManagerHome: manager})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Init(InitOptions{ProjectRoot: outer, ManagerHome: manager})
	if err != nil {
		t.Fatal(err)
	}
	if !first.Created || second.Created || first.Project.ProjectID != second.Project.ProjectID {
		t.Fatalf("init is not idempotent: first=%+v second=%+v", first, second)
	}
	attrs := string(mustReadFile(t, filepath.Join(outer, ".codexm", ".gitattributes")))
	if !strings.Contains(attrs, "*.bin binary") || strings.Count(attrs, attributesBegin) != 1 {
		t.Fatalf("attributes were not preserved: %s", attrs)
	}
	root, project, err := FindProject(inner)
	normalizedOuter, _ := normalizeExistingDir(outer)
	if err != nil || root != normalizedOuter || project.ProjectID != first.Project.ProjectID {
		t.Fatalf("nearest project lookup failed: %s %+v %v", root, project, err)
	}

	nested, err := Init(InitOptions{ProjectRoot: inner, ManagerHome: manager})
	if err != nil {
		t.Fatal(err)
	}
	root, project, err = FindProject(inner)
	normalizedInner, _ := normalizeExistingDir(inner)
	if err != nil || root != normalizedInner || project.ProjectID != nested.Project.ProjectID {
		t.Fatalf("nested project did not win: %s %+v %v", root, project, err)
	}
}

func TestInitDefaultsEmptyAndImportExistingIsExplicit(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	profile := filepath.Join(base, "profile")
	manager := filepath.Join(base, "manager")
	mustMkdir(t, root)
	writeProfileSession(t, profile, root, testSessionA, time.Now().Add(-time.Hour), false, false, "old")

	result, err := Init(InitOptions{ProjectRoot: root, ManagerHome: manager})
	if err != nil {
		t.Fatal(err)
	}
	if files, _ := filepath.Glob(filepath.Join(root, ".codexm", "metadata", "*.json")); len(files) != 0 {
		t.Fatalf("default init imported existing sessions: %v", files)
	}
	if _, err := Init(InitOptions{
		ProjectRoot:    root,
		ProfileHome:    profile,
		ManagerHome:    manager,
		ImportExisting: true,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".codexm", "metadata", testSessionA+".json")); err != nil {
		t.Fatalf("explicit import did not create metadata: %v (project=%+v)", err, result.Project)
	}
}

func TestInspectIsReadOnly(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	profile := filepath.Join(base, "profile")
	manager := filepath.Join(base, "manager-does-not-exist")
	mustMkdir(t, root)
	mustMkdir(t, profile)
	if _, err := Init(InitOptions{ProjectRoot: root, ManagerHome: filepath.Join(base, "init-manager")}); err != nil {
		t.Fatal(err)
	}
	inspection := Inspect(root, profile, manager)
	if !inspection.Enabled || inspection.ValidationError != "" {
		t.Fatalf("unexpected inspection: %+v", inspection)
	}
	if _, err := os.Stat(manager); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Inspect wrote manager state: %v", err)
	}
}

func TestSyncRewritesOnlyStructuredCWDFieldsAcrossCheckouts(t *testing.T) {
	base := t.TempDir()
	rootA := filepath.Join(base, "checkout-a")
	rootB := filepath.Join(base, "checkout-b")
	profileA := filepath.Join(base, "profile-a")
	profileB := filepath.Join(base, "profile-b")
	managerA := filepath.Join(base, "manager-a")
	managerB := filepath.Join(base, "manager-b")
	mustMkdir(t, filepath.Join(rootA, "sub"))
	writeProfileSession(t, profileA, rootA, testSessionA, time.Now(), false, false, "portable")
	if _, err := Init(InitOptions{
		ProjectRoot: rootA, ProfileHome: profileA, ManagerHome: managerA, ImportExisting: true,
	}); err != nil {
		t.Fatal(err)
	}
	copyProjectMirror(t, rootA, rootB)

	result, err := Sync(Options{ProjectRoot: rootB, ProfileHome: profileB, ManagerHome: managerB})
	if err != nil {
		t.Fatal(err)
	}
	if result.Imported != 1 {
		t.Fatalf("expected one import, got %+v", result)
	}
	sessionPath := findSingleRollout(t, filepath.Join(profileB, "sessions"))
	lines := readPlainLines(t, sessionPath)
	var meta, context map[string]any
	mustUnmarshal(t, lines[0], &meta)
	mustUnmarshal(t, lines[1], &context)
	normalizedRootB, _ := normalizeExistingDir(rootB)
	if got := meta["payload"].(map[string]any)["cwd"]; got != normalizedRootB {
		t.Fatalf("session_meta cwd not rewritten: %v", got)
	}
	if got := context["payload"].(map[string]any)["cwd"]; got != filepath.Join(normalizedRootB, "sub") {
		t.Fatalf("turn_context cwd not rewritten: %v", got)
	}
	var response map[string]any
	mustUnmarshal(t, lines[2], &response)
	output := response["payload"].(map[string]any)["output"].(string)
	if !strings.Contains(output, rootA) {
		t.Fatalf("non-structured string was unexpectedly rewritten: %s", lines[2])
	}
}

func TestSyncPrefixConflictResolveArchiveNameAndTombstone(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	profile := filepath.Join(base, "profile")
	manager := filepath.Join(base, "manager")
	mustMkdir(t, root)
	writeProfileSession(t, profile, root, testSessionA, time.Now(), false, false, "base")
	if _, err := Init(InitOptions{
		ProjectRoot: root, ProfileHome: profile, ManagerHome: manager, ImportExisting: true,
	}); err != nil {
		t.Fatal(err)
	}

	profilePath := findSingleRollout(t, filepath.Join(profile, "sessions"))
	appendJSONLine(t, profilePath, map[string]any{"type": "event_msg", "payload": map[string]any{"message": "profile append"}})
	result, err := Sync(Options{ProjectRoot: root, ProfileHome: profile, ManagerHome: manager})
	if err != nil || result.UpdatedProject != 1 {
		t.Fatalf("profile append did not win: %+v %v", result, err)
	}

	projectPath := findSingleRollout(t, filepath.Join(root, ".codexm", "sessions"))
	projectLines := readPlainLines(t, projectPath)
	profileLines := append([][]byte(nil), projectLines[:len(projectLines)-1]...)
	profileLines = append(profileLines, mustJSON(t, map[string]any{"type": "event_msg", "payload": map[string]any{"message": "other branch"}}))
	if err := writeRollout(profilePath, profileLines, false); err != nil {
		t.Fatal(err)
	}
	result, err = Sync(Options{ProjectRoot: root, ProfileHome: profile, ManagerHome: manager})
	var conflictErr *ConflictError
	if !errors.As(err, &conflictErr) || len(result.Conflicts) != 1 {
		t.Fatalf("divergence was not reported: %+v %v", result, err)
	}
	resolved, err := Resolve(ResolveOptions{
		ProjectRoot: root, ProfileHome: profile, ManagerHome: manager,
		SessionID: testSessionA, Use: "project",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(resolved.Backup); err != nil {
		t.Fatalf("conflict loser was not backed up: %v", err)
	}

	active := findSingleRollout(t, filepath.Join(profile, "sessions"))
	archived := filepath.Join(profile, "archived_sessions", filepath.Base(active))
	mustMkdir(t, filepath.Dir(archived))
	if err := os.Rename(active, archived); err != nil {
		t.Fatal(err)
	}
	appendIndexName(t, profile, testSessionA, "renamed")
	result, err = Sync(Options{ProjectRoot: root, ProfileHome: profile, ManagerHome: manager})
	if err != nil || result.UpdatedProject != 1 {
		t.Fatalf("archive/name did not round trip: %+v %v", result, err)
	}
	var metadata Metadata
	if err := readJSON(filepath.Join(root, ".codexm", "metadata", testSessionA+".json"), &metadata); err != nil {
		t.Fatal(err)
	}
	if !metadata.Archived || metadata.Name != "renamed" {
		t.Fatalf("archive/name metadata missing: %+v", metadata)
	}

	if err := os.Remove(archived); err != nil {
		t.Fatal(err)
	}
	result, err = Sync(Options{ProjectRoot: root, ProfileHome: profile, ManagerHome: manager})
	if err != nil || result.DeletedProject != 1 {
		t.Fatalf("profile deletion did not create tombstone: %+v %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(root, ".codexm", "tombstones", testSessionA+".json")); err != nil {
		t.Fatalf("tombstone missing: %v", err)
	}

	staleRoot := filepath.Join(base, "stale-checkout")
	staleProfile := filepath.Join(base, "stale-profile")
	staleManager := filepath.Join(base, "stale-manager")
	copyProjectMirror(t, root, staleRoot)
	stalePath := writeProfileSession(t, staleProfile, staleRoot, testSessionA, time.Now(), true, false, "stale")
	for i := 0; i < 2; i++ {
		if _, err := Sync(Options{ProjectRoot: staleRoot, ProfileHome: staleProfile, ManagerHome: staleManager}); err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(stalePath); err != nil {
			t.Fatalf("unowned stale profile copy was deleted on sync %d: %v", i+1, err)
		}
	}
}

func TestZstandardRoundTripAndValidationFailures(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	profile := filepath.Join(base, "profile")
	manager := filepath.Join(base, "manager")
	mustMkdir(t, root)
	writeProfileSession(t, profile, root, testSessionA, time.Now(), false, true, "compressed")
	if _, err := Init(InitOptions{
		ProjectRoot: root, ProfileHome: profile, ManagerHome: manager, ImportExisting: true,
	}); err != nil {
		t.Fatal(err)
	}
	if path := findSingleRollout(t, filepath.Join(root, ".codexm", "sessions")); !strings.HasSuffix(path, ".zst") {
		t.Fatalf("compressed extension was not preserved: %s", path)
	}

	t.Run("truncated", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "rollout-"+testSessionB+".jsonl")
		mustWriteFile(t, path, `{"type":"session_meta"}`)
		if _, err := readRollout(path, root, nil); err == nil || !strings.Contains(err.Error(), "truncated") {
			t.Fatalf("truncated JSONL accepted: %v", err)
		}
	})

	t.Run("symlink", func(t *testing.T) {
		if os.Getenv("GOOS") == "windows" {
			t.Skip("symlink permissions vary on Windows")
		}
		target := filepath.Join(t.TempDir(), "target.jsonl")
		mustWriteFile(t, target, "{}\n")
		link := filepath.Join(t.TempDir(), "rollout-"+testSessionB+".jsonl")
		if err := os.Symlink(target, link); err != nil {
			t.Skip(err)
		}
		if _, err := readRollout(link, root, nil); err == nil || !strings.Contains(err.Error(), "symbolic link") {
			t.Fatalf("symlink accepted: %v", err)
		}
	})

	t.Run("unknown project version", func(t *testing.T) {
		other := filepath.Join(t.TempDir(), "project")
		mustMkdir(t, filepath.Join(other, ".codexm"))
		mustWriteFile(t, filepath.Join(other, ".codexm", "project.json"),
			`{"format_version":99,"project_id":"11111111-1111-4111-8111-111111111111","initialized_at":"2026-01-01T00:00:00Z"}`)
		if _, _, err := FindProject(other); err == nil || !strings.Contains(err.Error(), "unsupported") {
			t.Fatalf("unknown project version accepted: %v", err)
		}
	})

	t.Run("duplicate session id", func(t *testing.T) {
		duplicateProfile := filepath.Join(t.TempDir(), "profile")
		active := writeProfileSession(t, duplicateProfile, root, testSessionB, time.Now(), false, false, "")
		archived := filepath.Join(duplicateProfile, "archived_sessions", filepath.Base(active))
		mustMkdir(t, filepath.Dir(archived))
		if err := os.WriteFile(archived, mustReadFile(t, active), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := scanProfile(duplicateProfile, root); err == nil || !strings.Contains(err.Error(), "duplicate session id") {
			t.Fatalf("duplicate UUID accepted: %v", err)
		}
	})

	t.Run("path traversal sidecar", func(t *testing.T) {
		unsafeRoot := filepath.Join(t.TempDir(), "project")
		unsafeProfile := filepath.Join(t.TempDir(), "profile")
		unsafeManager := filepath.Join(t.TempDir(), "manager")
		mustMkdir(t, unsafeRoot)
		writeProfileSession(t, unsafeProfile, unsafeRoot, testSessionB, time.Now(), false, false, "")
		if _, err := Init(InitOptions{
			ProjectRoot: unsafeRoot, ProfileHome: unsafeProfile, ManagerHome: unsafeManager, ImportExisting: true,
		}); err != nil {
			t.Fatal(err)
		}
		metadataPath := filepath.Join(unsafeRoot, ".codexm", "metadata", testSessionB+".json")
		var metadata Metadata
		if err := readJSON(metadataPath, &metadata); err != nil {
			t.Fatal(err)
		}
		metadata.RolloutPath = "../outside.jsonl"
		if err := atomicWriteJSON(metadataPath, metadata, 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := scanProject(unsafeRoot); err == nil || !strings.Contains(err.Error(), "unsafe rollout path") {
			t.Fatalf("path traversal accepted: %v", err)
		}
	})
}

func TestSessionMetaAllowsLogicalSessionIDToDifferFromThreadID(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "rollout-2026-07-18T12-00-00-"+testSessionA+".jsonl")
	line := mustJSON(t, map[string]any{
		"type": "session_meta",
		"payload": map[string]any{
			"id":               testSessionA,
			"session_id":       testSessionB,
			"parent_thread_id": testSessionB,
			"timestamp":        "2026-07-18T12:00:00Z",
			"cwd":              root,
			"source":           map[string]any{"subagent": "review"},
		},
	})
	if err := writeRollout(path, [][]byte{line}, false); err != nil {
		t.Fatal(err)
	}
	rollout, err := readRollout(path, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rollout.ID != testSessionA {
		t.Fatalf("rollout identity should use payload.id, got %s", rollout.ID)
	}
}

func TestProfileScanSkipsUnrelatedTranscriptBodies(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	other := filepath.Join(base, "other-project")
	profile := filepath.Join(base, "profile")
	mustMkdir(t, root)
	mustMkdir(t, other)
	path := filepath.Join(profile, "sessions", "2026", "01", "02", "rollout-2026-01-02T03-04-05-"+testSessionB+".jsonl")
	header := mustJSON(t, map[string]any{
		"type": "session_meta",
		"payload": map[string]any{
			"id": testSessionB, "timestamp": time.Now().UTC().Format(time.RFC3339Nano), "cwd": other,
		},
	})
	mustWriteFile(t, path, string(header)+"\n{this unrelated body is intentionally invalid}\n")
	sessions, err := scanProfile(profile, root)
	if err != nil {
		t.Fatalf("unrelated transcript body should not be parsed: %v", err)
	}
	if len(sessions) != 0 {
		t.Fatalf("unrelated session was included: %+v", sessions)
	}
	ids, err := SnapshotProfile(profile, root)
	if err != nil || len(ids) != 0 {
		t.Fatalf("lightweight snapshot included unrelated session: %+v %v", ids, err)
	}
}

func TestAuditDetectsSecretsLargeFilesAndUnmappedCWDWithoutLeakingValues(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	profile := filepath.Join(base, "profile")
	manager := filepath.Join(base, "manager")
	mustMkdir(t, root)
	profilePath := writeProfileSession(t, profile, root, testSessionA, time.Now(), false, false, "audit")
	secret := "sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"
	highEntropy := "aB3dEf5hIj7lMn9pQr2tUv4xYz6A8cD0eF2gH4j"
	appendJSONLine(t, profilePath, map[string]any{"type": "event_msg", "payload": map[string]any{"message": secret}})
	appendJSONLine(t, profilePath, map[string]any{"type": "event_msg", "payload": map[string]any{"message": highEntropy}})
	if _, err := Init(InitOptions{ProjectRoot: root, ProfileHome: profile, ManagerHome: manager, ImportExisting: true}); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(root, ".codexm", "metadata", testSessionA+".json")
	var metadata Metadata
	if err := readJSON(metadataPath, &metadata); err != nil {
		t.Fatal(err)
	}
	metadata.CWDs = metadata.CWDs[1:]
	if err := atomicWriteJSON(metadataPath, metadata, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Audit(AuditOptions{ProjectRoot: root, MaxFileSize: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Errors < 2 || result.Warnings < 2 || !result.HasFailures(false) {
		t.Fatalf("audit missed expected findings: %+v", result)
	}
	kinds := map[string]bool{}
	for _, finding := range result.Findings {
		kinds[finding.Kind] = true
		if strings.Contains(finding.Message, secret) || strings.Contains(finding.Message, highEntropy) {
			t.Fatalf("audit leaked a matched value: %+v", finding)
		}
	}
	for _, kind := range []string{"openai_api_key", "high_entropy_value", "large_transcript", "unmapped_absolute_cwd"} {
		if !kinds[kind] {
			t.Errorf("missing %s finding: %+v", kind, result.Findings)
		}
	}
}

func TestAuditCleanEmptyMirror(t *testing.T) {
	root := t.TempDir()
	if _, err := Init(InitOptions{ProjectRoot: root, ManagerHome: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	result, err := Audit(AuditOptions{ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if result.Files != 0 || len(result.Findings) != 0 || result.HasFailures(true) {
		t.Fatalf("empty mirror should be clean: %+v", result)
	}
}

func TestAuditScansSessionMetadataWithoutLeakingSecret(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "project")
	profile := filepath.Join(base, "profile")
	mustMkdir(t, root)
	writeProfileSession(t, profile, root, testSessionA, time.Now(), false, false, "audit")
	if _, err := Init(InitOptions{ProjectRoot: root, ProfileHome: profile, ManagerHome: filepath.Join(base, "manager"), ImportExisting: true}); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(root, ".codexm", "metadata", testSessionA+".json")
	var metadata Metadata
	if err := readJSON(metadataPath, &metadata); err != nil {
		t.Fatal(err)
	}
	secret := "sk-ABCDEFGHIJKLMNOPQRSTUVWXYZ1234567890"
	metadata.Name = secret
	if err := atomicWriteJSON(metadataPath, metadata, 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := Audit(AuditOptions{ProjectRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, finding := range result.Findings {
		if strings.Contains(finding.Message, secret) {
			t.Fatalf("audit leaked metadata secret: %+v", finding)
		}
		if finding.Kind == "openai_api_key" && strings.HasPrefix(finding.Path, "metadata/") {
			found = true
		}
	}
	if !found {
		t.Fatalf("metadata-only secret was not detected: %+v", result.Findings)
	}
}

func BenchmarkScanProfileSkipsUnrelatedTranscriptBodies(b *testing.B) {
	base := b.TempDir()
	root := filepath.Join(base, "project")
	other := filepath.Join(base, "other-project")
	profile := filepath.Join(base, "profile")
	for _, path := range []string{root, other, profile} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			b.Fatal(err)
		}
	}
	payload := strings.Repeat("x", 32*1024)
	for i := 0; i < 500; i++ {
		id := fmt.Sprintf("%08x-0000-4000-8000-%012x", i+1, i+1)
		path := filepath.Join(profile, "sessions", "2026", "01", "02", "rollout-2026-01-02T03-04-05-"+id+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			b.Fatal(err)
		}
		header, _ := json.Marshal(map[string]any{
			"type":    "session_meta",
			"payload": map[string]any{"id": id, "timestamp": "2026-01-02T03:04:05Z", "cwd": other},
		})
		body, _ := json.Marshal(map[string]any{"type": "event_msg", "payload": map[string]any{"message": payload}})
		if err := os.WriteFile(path, append(append(header, '\n'), append(body, '\n')...), 0o600); err != nil {
			b.Fatal(err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if sessions, err := scanProfile(profile, root); err != nil || len(sessions) != 0 {
			b.Fatalf("scanProfile() = %d sessions, %v", len(sessions), err)
		}
	}
}

func writeProfileSession(t *testing.T, profile, root, id string, created time.Time, archived, compressed bool, name string) string {
	t.Helper()
	filename := "rollout-2026-01-02T03-04-05-" + id + ".jsonl"
	relative := filepath.Join("sessions", "2026", "01", "02", filename)
	if archived {
		relative = filepath.Join("archived_sessions", filename)
	}
	if compressed {
		relative += ".zst"
	}
	path := filepath.Join(profile, relative)
	lines := [][]byte{
		mustJSON(t, map[string]any{
			"timestamp": created.UTC().Format(time.RFC3339Nano),
			"type":      "session_meta",
			"payload": map[string]any{
				"id": id, "session_id": id, "timestamp": created.UTC().Format(time.RFC3339Nano), "cwd": root,
			},
		}),
		mustJSON(t, map[string]any{
			"type": "turn_context", "payload": map[string]any{"cwd": filepath.Join(root, "sub")},
		}),
		mustJSON(t, map[string]any{
			"type": "response_item", "payload": map[string]any{"output": "do not rewrite " + root},
		}),
	}
	if err := writeRollout(path, lines, compressed); err != nil {
		t.Fatal(err)
	}
	if name != "" {
		appendIndexName(t, profile, id, name)
	}
	return path
}

func appendIndexName(t *testing.T, profile, id, name string) {
	t.Helper()
	if err := appendSessionName(profile, id, name); err != nil {
		t.Fatal(err)
	}
}

func appendJSONLine(t *testing.T, path string, value any) {
	t.Helper()
	lines := readPlainLines(t, path)
	lines = append(lines, mustJSON(t, value))
	if err := writeRollout(path, lines, strings.HasSuffix(path, ".zst")); err != nil {
		t.Fatal(err)
	}
}

func copyProjectMirror(t *testing.T, source, target string) {
	t.Helper()
	err := filepath.Walk(filepath.Join(source, ".codexm"), func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		dst := filepath.Join(target, rel)
		if info.IsDir() {
			return os.MkdirAll(dst, info.Mode())
		}
		return os.WriteFile(dst, mustReadFile(t, path), info.Mode())
	})
	if err != nil {
		t.Fatal(err)
	}
}

func findSingleRollout(t *testing.T, root string) string {
	t.Helper()
	var found []string
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && isRolloutFile(path) {
			found = append(found, path)
		}
		return nil
	})
	if len(found) != 1 {
		t.Fatalf("expected one rollout under %s, found %v", root, found)
	}
	return found[0]
}

func readPlainLines(t *testing.T, path string) [][]byte {
	t.Helper()
	session, err := readRollout(path, t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	return session.RawLines
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustUnmarshal(t *testing.T, data []byte, value any) {
	t.Helper()
	if err := json.Unmarshal(data, value); err != nil {
		t.Fatal(err)
	}
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	mustMkdir(t, filepath.Dir(path))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func mustMkdir(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatal(err)
	}
}
