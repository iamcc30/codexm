package session

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

const historyC = "33333333-3333-4333-8333-333333333333"
const historyD = "44444444-4444-4444-8444-444444444444"

type historyFixture struct{ root, home, manager string }

func newHistoryFixture(t *testing.T) historyFixture {
	t.Helper()
	base := t.TempDir()
	f := historyFixture{filepath.Join(base, "project"), filepath.Join(base, "profile"), filepath.Join(base, "manager")}
	mustMkdir(t, f.root)
	return f
}
func historyLines(t *testing.T, id, cwd string, base *HistoryPosition, messages ...string) [][]byte {
	t.Helper()
	ordinal := uint64(0)
	if base != nil {
		ordinal = base.EndOrdinalExclusive
	}
	payload := map[string]any{"id": id, "cwd": cwd, "timestamp": "2026-01-02T03:04:05Z", "history_mode": "paginated"}
	if base != nil {
		payload["history_base"] = base
	}
	lines := [][]byte{mustJSON(t, map[string]any{"type": "session_meta", "ordinal": ordinal, "payload": payload})}
	for _, message := range messages {
		ordinal++
		lines = append(lines, mustJSON(t, map[string]any{"type": "event_msg", "ordinal": ordinal, "payload": map[string]any{"message": message}}))
	}
	return lines
}
func historyWrite(t *testing.T, home, id, physical string, lines [][]byte, archived, compressed bool) string {
	t.Helper()
	dir := "sessions"
	if archived {
		dir = "archived_sessions"
	}
	suffix := id
	if physical != id {
		suffix += "_" + physical
	}
	name := "rollout-2026-01-02T03-04-05-" + suffix + ".jsonl"
	if compressed {
		name += ".zst"
	}
	path := filepath.Join(home, dir, name)
	if err := writeRollout(path, lines, compressed); err != nil {
		t.Fatal(err)
	}
	return path
}
func historyBase(id string, lines [][]byte, ordinal uint64) *HistoryPosition {
	var size uint64
	for _, line := range lines {
		size += uint64(len(line)) + 1
	}
	return &HistoryPosition{ThreadID: id, EndByteOffset: size, EndOrdinalExclusive: ordinal}
}
func (f historyFixture) init(t *testing.T) {
	t.Helper()
	if _, err := Init(InitOptions{ProjectRoot: f.root, ProfileHome: f.home, ManagerHome: f.manager, ImportExisting: true}); err != nil {
		t.Fatal(err)
	}
}
func (f historyFixture) sync(t *testing.T, home string) Result {
	t.Helper()
	result, err := Sync(Options{ProjectRoot: f.root, ProfileHome: home, ManagerHome: f.manager})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestHistoryRecursivelyCopiesBoundedRawCompressedSources(t *testing.T) {
	f := newHistoryFixture(t)
	other := filepath.Join(t.TempDir(), "elsewhere")
	mustMkdir(t, other)
	a := historyLines(t, testSessionA, other, nil, "inherited")
	// Native byte offsets include whitespace and CRLF exactly.
	a[0] = append(append([]byte("  "), a[0]...), '\r')
	aPath := historyWrite(t, f.home, testSessionA, testSessionA, append(append([][]byte{}, a...), []byte("unrelated trailing bytes that must not be parsed")), true, true)
	b := historyLines(t, testSessionB, other, historyBase(testSessionA, a, 2))
	bPath := historyWrite(t, f.home, testSessionB, testSessionB, b, false, false)
	c := historyLines(t, historyC, f.root, historyBase(testSessionB, b, 3), "child")
	historyWrite(t, f.home, historyC, historyD, c, false, false)
	f.init(t)
	project, err := scanProject(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(project.Sessions) != 1 || len(project.Sessions[historyC].Dependencies) != 2 {
		t.Fatalf("missing closure: %+v", project)
	}
	_, meta, err := FindProject(f.root)
	if err != nil || meta.FormatVersion != 2 {
		t.Fatalf("missing project version gate: %+v %v", meta, err)
	}
	target := filepath.Join(t.TempDir(), "target")
	f.sync(t, target)
	for _, item := range []struct {
		path  string
		lines [][]byte
	}{{aPath, a}, {bPath, b}} {
		rel, _ := filepath.Rel(f.home, item.path)
		got, err := readRollout(filepath.Join(target, rel), f.root, nil)
		if err != nil {
			t.Fatal(err)
		}
		if checksumLines(got.RawLines) != checksumLines(item.lines) {
			t.Fatal("inherited raw bytes changed")
		}
	}
	f.sync(t, target)
}

func TestHistoryRejectsUnsafeGraphs(t *testing.T) {
	for _, scenario := range []string{"missing", "cycle", "branch", "offset-middle", "offset-long", "ordinal", "missing-ordinal", "unknown-mode", "legacy-base", "legacy-source"} {
		t.Run(scenario, func(t *testing.T) {
			f := newHistoryFixture(t)
			a := historyLines(t, testSessionA, f.root, nil, "first")
			base := historyBase(testSessionA, a, 2)
			switch scenario {
			case "offset-middle":
				base.EndByteOffset--
			case "offset-long":
				base.EndByteOffset++
			case "ordinal":
				base.EndOrdinalExclusive++
			}
			if scenario == "legacy-source" {
				var meta map[string]any
				json.Unmarshal(a[0], &meta)
				delete(meta["payload"].(map[string]any), "history_mode")
				a[0] = mustJSON(t, meta)
				base = historyBase(testSessionA, a, 2)
			}
			if scenario != "missing" {
				historyWrite(t, f.home, testSessionA, testSessionA, a, false, false)
			}
			child := historyLines(t, testSessionA, f.root, base, "child")
			switch scenario {
			case "cycle":
				base.ThreadID = testSessionB
				child = historyLines(t, testSessionA, f.root, base, "child")
			case "branch":
				historyWrite(t, f.home, testSessionA, historyC, child, false, false)
			case "missing-ordinal":
				var value map[string]any
				json.Unmarshal(child[1], &value)
				delete(value, "ordinal")
				child[1] = mustJSON(t, value)
			case "unknown-mode", "legacy-base":
				var value map[string]any
				json.Unmarshal(child[0], &value)
				mode := "future"
				if scenario == "legacy-base" {
					mode = "legacy"
				}
				value["payload"].(map[string]any)["history_mode"] = mode
				child[0] = mustJSON(t, value)
			}
			historyWrite(t, f.home, testSessionA, testSessionB, child, false, false)
			if _, err := scanProfile(f.home, f.root); err == nil {
				t.Fatalf("accepted %s", scenario)
			}
		})
	}
}

func TestHistoryPreservesAdvancedSourceTailAndRejectsPrefixMismatchBeforeWrites(t *testing.T) {
	for _, mismatch := range []bool{false, true} {
		t.Run(map[bool]string{false: "advanced-tail", true: "different-prefix"}[mismatch], func(t *testing.T) {
			f := newHistoryFixture(t)
			other := t.TempDir()
			a := historyLines(t, testSessionA, other, nil, "first")
			historyWrite(t, f.home, testSessionA, testSessionA, a, false, false)
			b := historyLines(t, testSessionB, f.root, historyBase(testSessionA, a, 2), "child")
			historyWrite(t, f.home, testSessionB, testSessionB, b, false, false)
			f.init(t)
			target := t.TempDir()
			extended := append(append([][]byte{}, a...), mustJSON(t, map[string]any{"type": "event_msg", "ordinal": 2, "payload": map[string]any{"message": "later source tail"}}))
			if mismatch {
				extended[1] = bytes.ReplaceAll(extended[1], []byte("first"), []byte("other"))
			}
			targetSource := historyWrite(t, target, testSessionA, testSessionA, extended, false, false)
			before := mustReadFile(t, targetSource)
			_, err := Sync(Options{ProjectRoot: f.root, ProfileHome: target, ManagerHome: f.manager})
			if mismatch {
				if err == nil || !strings.Contains(err.Error(), "prefix conflict") {
					t.Fatalf("expected raw prefix conflict: %v", err)
				}
				if _, err := os.Stat(filepath.Join(target, "sessions", "rollout-2026-01-02T03-04-05-"+testSessionB+".jsonl")); !os.IsNotExist(err) {
					t.Fatal("partial import before failed preflight")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, mustReadFile(t, targetSource)) {
				t.Fatal("existing source was modified")
			}
		})
	}
}

func TestHistoryNativeRevertCanDiscardOldTail(t *testing.T) {
	f := newHistoryFixture(t)
	full := historyLines(t, testSessionA, f.root, nil, "keep", "discard")
	historyWrite(t, f.home, testSessionA, testSessionA, full, false, false)
	f.init(t)
	target := t.TempDir()
	f.sync(t, target)
	base := historyBase(testSessionA, full[:2], 2)
	newLines := historyLines(t, testSessionA, f.root, base, "replacement")
	newPath := historyWrite(t, target, testSessionA, testSessionB, newLines, false, false)
	f.sync(t, target)
	f.sync(t, f.home)
	rel, _ := filepath.Rel(target, newPath)
	if !bytes.Equal(mustReadFile(t, newPath), mustReadFile(t, filepath.Join(f.home, rel))) {
		t.Fatal("revert tip not imported")
	}
}

func TestHistoryRelocationFailsBeforeProfileWrites(t *testing.T) {
	f := newHistoryFixture(t)
	historyWrite(t, f.home, testSessionA, testSessionA, historyLines(t, testSessionA, f.root, nil, "original"), false, false)
	f.init(t)
	targetRoot := filepath.Join(t.TempDir(), "different-checkout")
	copyProjectMirror(t, f.root, targetRoot)
	target := filepath.Join(t.TempDir(), "profile")
	_, err := Sync(Options{ProjectRoot: targetRoot, ProfileHome: target, ManagerHome: f.manager})
	if err == nil || !strings.Contains(err.Error(), "relocat") {
		t.Fatalf("unsafe relocation accepted: %v", err)
	}
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatal("profile changed during refused relocation")
	}
}

func TestHistoryTombstoneDoesNotResurrectRetainedAncestors(t *testing.T) {
	f := newHistoryFixture(t)
	a := historyLines(t, testSessionA, f.root, nil, "old")
	aPath := historyWrite(t, f.home, testSessionA, testSessionA, a, false, false)
	b := historyLines(t, testSessionA, f.root, historyBase(testSessionA, a, 2), "current")
	bPath := historyWrite(t, f.home, testSessionA, testSessionB, b, false, false)
	f.init(t)
	// Native/user deletes the selected physical tip, leaving its source behind.
	if err := os.Remove(bPath); err != nil {
		t.Fatal(err)
	}
	result := f.sync(t, f.home)
	if result.DeletedProject != 1 {
		t.Fatalf("selected tip deletion was not tombstoned: %+v", result)
	}
	state, err := scanProject(f.root)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Sessions) != 0 || len(state.Tombstones) != 1 {
		t.Fatalf("ancestor resurrected as managed session: %+v", state)
	}
	f.sync(t, f.home)
	if _, err := os.Stat(aPath); !os.IsNotExist(err) {
		t.Fatalf("unreferenced ancestor retained after logical deletion: %v", err)
	}
	f.sync(t, f.home)
}

func TestHistoryBackupIncludesBoundedClosureAndAuditScansIt(t *testing.T) {
	f := newHistoryFixture(t)
	other := t.TempDir()
	// A synthetic credential-shaped value must be detected without exposing it.
	a := historyLines(t, testSessionA, other, nil, "sk-syntheticCredentialValue123456789012345")
	historyWrite(t, f.home, testSessionA, testSessionA, a, true, true)
	b := historyLines(t, testSessionB, f.root, historyBase(testSessionA, a, 2), "child")
	historyWrite(t, f.home, testSessionB, testSessionB, b, false, false)
	f.init(t)
	state, err := scanProject(f.root)
	if err != nil {
		t.Fatal(err)
	}
	source := state.Sessions[testSessionB]
	path, err := backupSession(f.manager, "synthetic-project", "profile", source)
	if err != nil {
		t.Fatal(err)
	}
	if checksumBytes(mustReadFile(t, path)) != checksumBytes(bytes.Join(append(append([][]byte{}, b...), []byte{}), []byte{'\n'})) {
		t.Fatal("backup current bytes changed")
	}
	backupRoot := filepath.Dir(filepath.Dir(path))
	descriptor := dependencyFor(source.Dependencies[0])
	inherited, err := readRollout(filepath.Join(backupRoot, filepath.FromSlash(descriptor.SnapshotPath)), f.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if inherited.RawChecksum != checksumLines(a) {
		t.Fatal("backup dependency missing or changed")
	}
	audit, err := Audit(AuditOptions{ProjectRoot: f.root})
	if err != nil {
		t.Fatal(err)
	}
	foundSecret, foundCWD := false, false
	for _, finding := range audit.Findings {
		if strings.HasPrefix(finding.Path, "history/") && finding.Kind == "openai_api_key" {
			foundSecret = true
		}
		if strings.HasPrefix(finding.Path, "history/") && finding.Kind == "retained_absolute_cwd" && finding.Severity == "warning" {
			foundCWD = true
		}
	}
	if !foundSecret || !foundCWD {
		t.Fatalf("dependency audit incomplete: %+v", audit)
	}
}

func TestHistoryRejectsNonNativePhysicalNames(t *testing.T) {
	for _, name := range []string{
		"rollout-invalid-date-" + testSessionA + "_" + testSessionB + ".jsonl",
		"rollout-2026-01-02T03-04-05-" + testSessionA + "_" + historyC + "_" + testSessionA + "_" + testSessionB + ".jsonl",
		"rollout-2026-01-02T03-04-05-" + testSessionA + "_" + testSessionB + ".jsonl.invalid",
	} {
		if _, err := physicalRolloutID(name, testSessionA); err == nil {
			t.Fatalf("accepted non-native filename %s", name)
		}
	}
}

func TestHistorySidecarDowngradeAndUnknownVersionFailClosed(t *testing.T) {
	for _, scenario := range []string{"project-downgrade", "dependency-sidecar-downgrade", "unknown-version", "missing-dependency", "raw-checksum"} {
		t.Run(scenario, func(t *testing.T) {
			f := newHistoryFixture(t)
			other := t.TempDir()
			a := historyLines(t, testSessionA, other, nil, "original")
			historyWrite(t, f.home, testSessionA, testSessionA, a, false, false)
			historyWrite(t, f.home, testSessionB, testSessionB, historyLines(t, testSessionB, f.root, historyBase(testSessionA, a, 2), "child"), false, false)
			f.init(t)
			metaPath := filepath.Join(f.root, projectDirName, "metadata", testSessionB+".json")
			var meta Metadata
			if err := readJSON(metaPath, &meta); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "project-downgrade":
				var project Project
				path := filepath.Join(f.root, projectDirName, projectFileName)
				if err := readJSON(path, &project); err != nil {
					t.Fatal(err)
				}
				project.FormatVersion = 1
				if err := atomicWriteJSON(path, project, 0o644); err != nil {
					t.Fatal(err)
				}
			case "dependency-sidecar-downgrade":
				meta.FormatVersion = 1
				meta.Dependencies = nil
				meta.RolloutID = ""
				meta.RawChecksum = ""
			case "unknown-version":
				meta.FormatVersion = 99
			case "missing-dependency":
				meta.Dependencies = nil
			case "raw-checksum":
				meta.RawChecksum = strings.Repeat("0", 64)
			}
			if err := atomicWriteJSON(metaPath, meta, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := scanProject(f.root); err == nil {
				t.Fatalf("accepted %s", scenario)
			}
		})
	}
}

func TestHistorySameCheckoutPathAliasesPreserveRawBytes(t *testing.T) {
	for _, scenario := range []string{"symlink", "windows-case"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			actualRoot := filepath.Join(base, "ActualProject")
			mustMkdir(t, actualRoot)
			var alias string
			switch scenario {
			case "symlink":
				alias = filepath.Join(base, "different-length-project-alias")
				if err := os.Symlink(actualRoot, alias); err != nil {
					if runtime.GOOS == "windows" {
						t.Skipf("symlink privilege unavailable: %v", err)
					}
					t.Fatal(err)
				}
			case "windows-case":
				if runtime.GOOS != "windows" {
					t.Skip("Windows path case normalization")
				}
				alias = strings.ToUpper(actualRoot)
			}
			f := historyFixture{root: actualRoot, home: filepath.Join(base, "source-profile"), manager: filepath.Join(base, "manager")}
			mappedCWD := filepath.Join(actualRoot, "NestedDirectory")
			mustMkdir(t, mappedCWD)
			source := historyLines(t, testSessionA, alias, nil, "original path spelling")
			source = append(source, mustJSON(t, map[string]any{"type": "turn_context", "ordinal": 2, "payload": map[string]any{"cwd": filepath.Join(alias, "NestedDirectory")}}))
			sourcePath := historyWrite(t, f.home, testSessionA, testSessionA, source, false, false)
			child := historyLines(t, testSessionB, alias, historyBase(testSessionA, source, 3), "inherited raw prefix")
			childPath := historyWrite(t, f.home, testSessionB, testSessionB, child, false, false)
			sourceBytes, childBytes := mustReadFile(t, sourcePath), mustReadFile(t, childPath)
			f.init(t)
			target := filepath.Join(base, "target-profile")
			f.sync(t, target)
			for _, item := range []struct {
				path string
				data []byte
			}{{sourcePath, sourceBytes}, {childPath, childBytes}} {
				relative, err := filepath.Rel(f.home, item.path)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(item.data, mustReadFile(t, filepath.Join(target, relative))) {
					t.Fatal("path normalization rewrote byte-addressed history")
				}
			}
			// The imported aliases must still be discoverable on a subsequent sync.
			f.sync(t, target)
		})
	}
}

func TestHistoryCWDEqualityDoesNotConflateDistinctDirectories(t *testing.T) {
	base := t.TempDir()
	first, second := filepath.Join(base, "FirstDirectory"), filepath.Join(base, "SecondDirectory")
	mustMkdir(t, first)
	mustMkdir(t, second)
	if sameHistoryCWD(first, second) {
		t.Fatal("different existing directories were treated as an alias")
	}
	missing := filepath.Join(base, "historical-directory")
	if !sameHistoryCWD(missing, missing) {
		t.Fatal("exact historical cwd spelling was not preserved")
	}
	if sameHistoryCWD(missing, filepath.Join(base, "other-missing-directory")) {
		t.Fatal("different missing cwd paths were treated as an alias")
	}
}
