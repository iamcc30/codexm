package session

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const reviewSourceID = "11111111-aaaa-4111-8111-111111111111"
const reviewChildID = "22222222-bbbb-4222-8222-222222222222"
const reviewOtherChildID = "33333333-cccc-4333-8333-333333333333"

func reviewPaginatedLines(t *testing.T, id, cwd string, base *HistoryPosition, messages ...string) [][]byte {
	t.Helper()
	ordinal := uint64(0)
	payload := map[string]any{"id": id, "cwd": cwd, "timestamp": "2026-01-02T03:04:05Z", "history_mode": "paginated"}
	if base != nil {
		ordinal = base.EndOrdinalExclusive
		payload["history_base"] = base
	}
	lines := [][]byte{mustJSON(t, map[string]any{"type": "session_meta", "ordinal": ordinal, "payload": payload})}
	for _, message := range messages {
		ordinal++
		lines = append(lines, mustJSON(t, map[string]any{"type": "event_msg", "ordinal": ordinal, "payload": map[string]any{"message": message}}))
	}
	return lines
}

func reviewWriteRollout(t *testing.T, home, id string, lines [][]byte) string {
	t.Helper()
	path := filepath.Join(home, "sessions", "2026", "01", "02", "rollout-2026-01-02T03-04-05-"+id+".jsonl")
	if err := writeRollout(path, lines, false); err != nil {
		t.Fatal(err)
	}
	return path
}

func reviewWritePhysicalRollout(t *testing.T, home, logical, physical string, lines [][]byte) string {
	t.Helper()
	name := logical
	if physical != logical {
		name += "_" + physical
	}
	path := filepath.Join(home, "sessions", "2026", "01", "02", "rollout-2026-01-02T03-04-05-"+name+".jsonl")
	if err := writeRollout(path, lines, false); err != nil {
		t.Fatal(err)
	}
	return path
}

func reviewCutoff(id string, lines [][]byte, endOrdinal uint64) *HistoryPosition {
	var count uint64
	for _, line := range lines {
		count += uint64(len(line)) + 1
	}
	return &HistoryPosition{ThreadID: id, EndByteOffset: count, EndOrdinalExclusive: endOrdinal}
}

func reviewSync(t *testing.T, root, home, manager string) Result {
	t.Helper()
	result, err := Sync(Options{ProjectRoot: root, ProfileHome: home, ManagerHome: manager, ImportExisting: true})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestReviewTombstoneAndNewDependentRetainFullSourceTail(t *testing.T) {
	base := t.TempDir()
	root, origin, target, manager := filepath.Join(base, "project"), filepath.Join(base, "origin"), filepath.Join(base, "target"), filepath.Join(base, "manager")
	mustMkdir(t, root)
	full := reviewPaginatedLines(t, reviewSourceID, root, nil, "inherited", "legitimate later source tail")
	originPath := reviewWriteRollout(t, origin, reviewSourceID, full)
	if _, err := Init(InitOptions{ProjectRoot: root, ProfileHome: origin, ManagerHome: manager, ImportExisting: true}); err != nil {
		t.Fatal(err)
	}
	reviewSync(t, root, target, manager)
	relative, err := filepath.Rel(origin, originPath)
	if err != nil {
		t.Fatal(err)
	}
	targetSource := filepath.Join(target, relative)
	before := mustReadFile(t, targetSource)
	child := reviewPaginatedLines(t, reviewChildID, root, reviewCutoff(reviewSourceID, full[:2], 2), "child delta")
	childOrigin := reviewWriteRollout(t, origin, reviewChildID, child)
	reviewSync(t, root, origin, manager)
	state, err := scanProject(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := tombstoneProjectSession(root, state.Sessions[reviewSourceID]); err != nil {
		t.Fatal(err)
	}

	reviewSync(t, root, target, manager)
	if !bytes.Equal(before, mustReadFile(t, targetSource)) {
		t.Fatal("tombstone discarded source tail before installing its newly imported dependent")
	}
	childRelative, _ := filepath.Rel(origin, childOrigin)
	if _, err := readRollout(filepath.Join(target, childRelative), root, nil); err != nil {
		t.Fatalf("dependent was not imported: %v", err)
	}
	reviewSync(t, root, target, manager)
	if !bytes.Equal(before, mustReadFile(t, targetSource)) {
		t.Fatal("repeat synchronization changed retained source bytes")
	}
}

func TestReviewReferencedPhysicalRolloutArchiveMoveDoesNotDuplicate(t *testing.T) {
	base := t.TempDir()
	root, origin, target, manager := filepath.Join(base, "project"), filepath.Join(base, "origin"), filepath.Join(base, "target"), filepath.Join(base, "manager")
	mustMkdir(t, root)
	source := reviewPaginatedLines(t, reviewSourceID, root, nil, "source", "later tail")
	originPath := reviewWriteRollout(t, origin, reviewSourceID, source)
	if _, err := Init(InitOptions{ProjectRoot: root, ProfileHome: origin, ManagerHome: manager, ImportExisting: true}); err != nil {
		t.Fatal(err)
	}
	reviewSync(t, root, target, manager)
	relative, _ := filepath.Rel(origin, originPath)
	targetActive := filepath.Join(target, relative)
	before := mustReadFile(t, targetActive)
	otherRoot := filepath.Join(base, "other-project")
	mustMkdir(t, otherRoot)
	childPath := reviewWriteRollout(t, target, reviewChildID, reviewPaginatedLines(t, reviewChildID, otherRoot, reviewCutoff(reviewSourceID, source[:2], 2), "other project child"))
	childBefore := mustReadFile(t, childPath)
	originArchived := filepath.Join(origin, "archived_sessions", filepath.Base(originPath))
	mustMkdir(t, filepath.Dir(originArchived))
	if err := os.Rename(originPath, originArchived); err != nil {
		t.Fatal(err)
	}
	reviewSync(t, root, origin, manager)
	reviewSync(t, root, target, manager)
	if _, err := os.Stat(targetActive); !os.IsNotExist(err) {
		t.Fatalf("old physical rollout path remains after archive move: %v", err)
	}
	targetArchived := filepath.Join(target, "archived_sessions", filepath.Base(originPath))
	if !bytes.Equal(before, mustReadFile(t, targetArchived)) || !bytes.Equal(childBefore, mustReadFile(t, childPath)) {
		t.Fatal("archive move changed source or dependent bytes")
	}
	if _, err := scanProfile(target, root); err != nil {
		t.Fatalf("archive move left an invalid physical catalog: %v", err)
	}
	reviewSync(t, root, target, manager)
}

func TestReviewVersionOneStandalonePaginatedMirrorUpgradesOnWrite(t *testing.T) {
	base := t.TempDir()
	root, origin, manager := filepath.Join(base, "project"), filepath.Join(base, "origin"), filepath.Join(base, "manager")
	mustMkdir(t, root)
	if _, err := Init(InitOptions{ProjectRoot: root, ManagerHome: manager}); err != nil {
		t.Fatal(err)
	}
	lines := reviewPaginatedLines(t, reviewSourceID, root, nil, "existing version one history")
	originPath := reviewWriteRollout(t, origin, reviewSourceID, lines)
	source, err := readRollout(originPath, root, nil)
	if err != nil {
		t.Fatal(err)
	}
	relative, _ := filepath.Rel(origin, originPath)
	if err := writeRollout(filepath.Join(root, projectDirName, relative), lines, false); err != nil {
		t.Fatal(err)
	}
	metadataPath := filepath.Join(root, projectDirName, "metadata", reviewSourceID+".json")
	legacy := Metadata{FormatVersion: 1, SessionID: source.ID, RolloutPath: filepath.ToSlash(relative), Checksum: source.Checksum, LineCount: source.LineCount, CWDs: source.CWDs}
	if err := atomicWriteJSON(metadataPath, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := scanProject(root); err != nil {
		t.Fatalf("previously supported standalone v1 paginated mirror became unreadable: %v", err)
	}
	lines = append(lines, mustJSON(t, map[string]any{"type": "event_msg", "ordinal": 2, "payload": map[string]any{"message": "new append"}}))
	if err := writeRollout(originPath, lines, false); err != nil {
		t.Fatal(err)
	}
	reviewSync(t, root, origin, manager)
	_, project, err := FindProject(root)
	if err != nil || project.FormatVersion != HistoryFormatVersion {
		t.Fatalf("write did not publish old-reader version gate: project=%+v error=%v", project, err)
	}
	var updated Metadata
	if err := readJSON(metadataPath, &updated); err != nil {
		t.Fatal(err)
	}
	if updated.FormatVersion != HistoryFormatVersion || updated.RawChecksum != checksumLines(lines) {
		t.Fatalf("write did not upgrade standalone paginated sidecar: %+v", updated)
	}
	reviewSync(t, root, filepath.Join(base, "target"), manager)
}

func TestReviewConflictingPhysicalSnapshotsFailBeforeImport(t *testing.T) {
	base := t.TempDir()
	root, target, manager := filepath.Join(base, "project"), filepath.Join(base, "target"), filepath.Join(base, "manager")
	mustMkdir(t, root)
	if _, err := Init(InitOptions{ProjectRoot: root, ManagerHome: manager}); err != nil {
		t.Fatal(err)
	}
	otherRoot := filepath.Join(base, "other-project")
	mustMkdir(t, otherRoot)
	for i, childID := range []string{reviewChildID, reviewOtherChildID} {
		home := filepath.Join(base, childID)
		message := []string{"source version one", "incompatible source version two"}[i]
		source := reviewPaginatedLines(t, reviewSourceID, otherRoot, nil, message)
		reviewWriteRollout(t, home, reviewSourceID, source)
		reviewWriteRollout(t, home, childID, reviewPaginatedLines(t, childID, root, reviewCutoff(reviewSourceID, source, 2), "child"))
		sessions, err := scanProfile(home, root)
		if err != nil {
			t.Fatal(err)
		}
		if err := exportProfileSession(root, sessions[childID], nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Sync(Options{ProjectRoot: root, ProfileHome: target, ManagerHome: manager}); err == nil {
		t.Fatal("accepted individually valid sidecars that disagree on shared physical history")
	}
	for _, collection := range []string{"sessions", "archived_sessions"} {
		files, err := regularFiles(filepath.Join(target, collection), isRolloutFile)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 0 {
			t.Fatalf("conflicting physical snapshots caused partial profile writes: %v", files)
		}
	}
}

func TestReviewManagedTipShorterThanRequiredDependencyFailsBeforeWrites(t *testing.T) {
	const sourceID = "99999999-dddd-4999-8999-999999999999"
	base := t.TempDir()
	root, origin, target, manager := filepath.Join(base, "project"), filepath.Join(base, "origin"), filepath.Join(base, "target"), filepath.Join(base, "manager")
	mustMkdir(t, root)
	full := reviewPaginatedLines(t, sourceID, root, nil, "first", "required later source record")
	reviewWriteRollout(t, origin, sourceID, full)
	reviewWriteRollout(t, origin, reviewChildID, reviewPaginatedLines(t, reviewChildID, root, reviewCutoff(sourceID, full, 3), "child"))
	if _, err := Init(InitOptions{ProjectRoot: root, ProfileHome: origin, ManagerHome: manager, ImportExisting: true}); err != nil {
		t.Fatal(err)
	}
	project, err := scanProject(root)
	if err != nil {
		t.Fatal(err)
	}
	// Model separately valid source and child sidecars arriving from different
	// mirror revisions: the child needs more source bytes than the selected tip.
	shortHome := filepath.Join(base, "short-source")
	reviewWriteRollout(t, shortHome, sourceID, full[:2])
	short, err := scanProfile(shortHome, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := exportProfileSession(root, short[sourceID], project.Sessions[sourceID]); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(Options{ProjectRoot: root, ProfileHome: target, ManagerHome: manager}); err == nil {
		t.Fatal("accepted a selected source tip shorter than its planned dependent cutoff")
	}
	for _, collection := range []string{"sessions", "archived_sessions"} {
		files, err := regularFiles(filepath.Join(target, collection), isRolloutFile)
		if err != nil {
			t.Fatal(err)
		}
		if len(files) != 0 {
			t.Fatalf("short managed source was rejected only after partial profile writes: %v", files)
		}
	}
}

func TestReviewDuplicatePhysicalTipIdentityFailsBeforeWrites(t *testing.T) {
	base := t.TempDir()
	root, target, manager := filepath.Join(base, "project"), filepath.Join(base, "target"), filepath.Join(base, "manager")
	mustMkdir(t, root)
	if _, err := Init(InitOptions{ProjectRoot: root, ManagerHome: manager}); err != nil {
		t.Fatal(err)
	}
	for _, logical := range []string{reviewChildID, reviewOtherChildID} {
		origin := filepath.Join(base, logical)
		relative := filepath.Join("sessions", "rollout-2026-01-02T03-04-05-"+logical+"_"+reviewSourceID+".jsonl")
		path := filepath.Join(origin, relative)
		if err := writeRollout(path, reviewPaginatedLines(t, logical, root, nil, "tip"), false); err != nil {
			t.Fatal(err)
		}
		source, err := readRollout(path, root, nil)
		if err != nil {
			t.Fatal(err)
		}
		source.Relative = filepath.ToSlash(relative)
		if err := exportProfileSession(root, source, nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Sync(Options{ProjectRoot: root, ProfileHome: target, ManagerHome: manager}); err == nil {
		t.Fatal("accepted two logical tips claiming the same physical rollout identity")
	}
	files, err := regularFiles(filepath.Join(target, "sessions"), isRolloutFile)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("physical identity collision was discovered only after partial profile writes: %v", files)
	}
}

func reviewMirrorWithDependency(t *testing.T) (string, Metadata) {
	t.Helper()
	base := t.TempDir()
	root, origin, manager := filepath.Join(base, "project"), filepath.Join(base, "origin"), filepath.Join(base, "manager")
	mustMkdir(t, root)
	otherRoot := filepath.Join(base, "other-project")
	mustMkdir(t, otherRoot)
	source := reviewPaginatedLines(t, reviewSourceID, otherRoot, nil, "source")
	reviewWriteRollout(t, origin, reviewSourceID, source)
	reviewWriteRollout(t, origin, reviewChildID, reviewPaginatedLines(t, reviewChildID, root, reviewCutoff(reviewSourceID, source, 2), "child"))
	if _, err := Init(InitOptions{ProjectRoot: root, ProfileHome: origin, ManagerHome: manager, ImportExisting: true}); err != nil {
		t.Fatal(err)
	}
	var metadata Metadata
	if err := readJSON(filepath.Join(root, projectDirName, "metadata", reviewChildID+".json"), &metadata); err != nil {
		t.Fatal(err)
	}
	return root, metadata
}

func TestReviewDependencySnapshotPathsRejectNonHexChecksums(t *testing.T) {
	for _, checksum := range []string{strings.Repeat("a", 31) + "/" + strings.Repeat("b", 32), "../" + strings.Repeat("c", 61), strings.Repeat("A", 64)} {
		t.Run(checksum[:3], func(t *testing.T) {
			root, metadata := reviewMirrorWithDependency(t)
			dependency := &metadata.Dependencies[0]
			dependency.RawChecksum = checksum
			dependency.SnapshotPath = filepath.ToSlash(filepath.Join("history", dependency.RolloutID, checksum, filepath.Base(dependency.RolloutPath)))
			if err := atomicWriteJSON(filepath.Join(root, projectDirName, "metadata", reviewChildID+".json"), metadata, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := scanProject(root); err == nil || !strings.Contains(err.Error(), "invalid history snapshot path") {
				t.Fatalf("non-hex checksum was not rejected before reading its constructed path: %v", err)
			}
		})
	}
}

func TestReviewDependencySnapshotAncestorsRejectSymlinks(t *testing.T) {
	for _, location := range []string{"history-root", "checksum-directory"} {
		t.Run(location, func(t *testing.T) {
			root, metadata := reviewMirrorWithDependency(t)
			path := filepath.Join(root, projectDirName, "history")
			if location == "checksum-directory" {
				path = filepath.Dir(filepath.Join(root, projectDirName, filepath.FromSlash(metadata.Dependencies[0].SnapshotPath)))
			}
			outside := filepath.Join(t.TempDir(), "original-snapshots")
			if err := os.Rename(path, outside); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(outside, path); err != nil {
				t.Skipf("symbolic links are unavailable: %v", err)
			}
			if _, err := scanProject(root); err == nil || !strings.Contains(err.Error(), "symbolic link") {
				t.Fatalf("snapshot symlink ancestor was accepted: %v", err)
			}
		})
	}
}

func TestReviewResolveBacksUpCompleteRetiredAncestorTail(t *testing.T) {
	for _, scenario := range []string{"plain", "compressed", "backup-failure"} {
		t.Run(scenario, func(t *testing.T) {
			base := t.TempDir()
			root, winnerHome, loserHome, manager := filepath.Join(base, "project"), filepath.Join(base, "winner"), filepath.Join(base, "loser"), filepath.Join(base, "manager")
			mustMkdir(t, root)
			// A native revert to the beginning can have no history_base. This winning
			// branch does not retain the losing branch's historical physical ancestors.
			winner := reviewPaginatedLines(t, reviewSourceID, root, nil, "chosen replacement")
			winnerPath := reviewWritePhysicalRollout(t, winnerHome, reviewSourceID, reviewOtherChildID, winner)
			if _, err := Init(InitOptions{ProjectRoot: root, ProfileHome: winnerHome, ManagerHome: manager, ImportExisting: true}); err != nil {
				t.Fatal(err)
			}
			ancestor := reviewPaginatedLines(t, reviewSourceID, root, nil, "inherited prefix", "valuable source tail outside the losing tip cutoff")
			ancestorPath := reviewWritePhysicalRollout(t, loserHome, reviewSourceID, reviewSourceID, ancestor)
			if scenario == "compressed" {
				if err := writeRollout(ancestorPath+".zst", ancestor, true); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(ancestorPath); err != nil {
					t.Fatal(err)
				}
				ancestorPath += ".zst"
			}
			ancestorBefore := mustReadFile(t, ancestorPath)
			loser := reviewPaginatedLines(t, reviewSourceID, root, reviewCutoff(reviewSourceID, ancestor[:2], 2), "losing tip delta")
			loserPath := reviewWritePhysicalRollout(t, loserHome, reviewSourceID, reviewChildID, loser)
			loserBefore := mustReadFile(t, loserPath)
			if scenario == "backup-failure" {
				if err := os.WriteFile(filepath.Join(manager, "session-backups"), []byte("block backup directory creation"), 0o600); err != nil {
					t.Fatal(err)
				}
				if _, err := Resolve(ResolveOptions{ProjectRoot: root, ProfileHome: loserHome, ManagerHome: manager, SessionID: reviewSourceID, Use: "project"}); err == nil {
					t.Fatal("retired branch despite inability to create its backup")
				}
				if !bytes.Equal(ancestorBefore, mustReadFile(t, ancestorPath)) || !bytes.Equal(loserBefore, mustReadFile(t, loserPath)) {
					t.Fatal("failed backup changed or removed the original losing branch")
				}
				relative, _ := filepath.Rel(winnerHome, winnerPath)
				if _, err := os.Stat(filepath.Join(loserHome, relative)); !os.IsNotExist(err) {
					t.Fatalf("failed backup wrote the replacement branch: %v", err)
				}
				return
			}

			result, err := Resolve(ResolveOptions{ProjectRoot: root, ProfileHome: loserHome, ManagerHome: manager, SessionID: reviewSourceID, Use: "project"})
			if err != nil {
				t.Fatal(err)
			}
			if result.Backup == "" {
				t.Fatal("branch replacement did not return a backup")
			}
			_, project, err := FindProject(root)
			if err != nil {
				t.Fatal(err)
			}
			foundAncestor, foundLoser := false, false
			err = filepath.WalkDir(filepath.Join(manager, "session-backups", project.ProjectID), func(path string, entry os.DirEntry, walkErr error) error {
				if walkErr != nil {
					return walkErr
				}
				if entry.IsDir() {
					return nil
				}
				data, err := os.ReadFile(path)
				if err != nil {
					return err
				}
				foundAncestor = foundAncestor || bytes.Equal(data, ancestorBefore)
				foundLoser = foundLoser || bytes.Equal(data, loserBefore)
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !foundAncestor || !foundLoser {
				t.Fatalf("retired physical files were not fully backed up: complete ancestor=%v, complete losing tip=%v", foundAncestor, foundLoser)
			}
			for _, path := range []string{ancestorPath, loserPath} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("retired physical variant remains in the active catalog: %v", err)
				}
			}
			relative, _ := filepath.Rel(winnerHome, winnerPath)
			if !bytes.Equal(mustReadFile(t, winnerPath), mustReadFile(t, filepath.Join(loserHome, relative))) {
				t.Fatal("winning physical rollout changed during resolution")
			}
			sessions, err := scanProfile(loserHome, root)
			if err != nil || sessions[reviewSourceID] == nil || sessions[reviewSourceID].RolloutID != reviewOtherChildID {
				t.Fatalf("resolved profile did not retain one unambiguous winning tip: %v", err)
			}
		})
	}
}

func TestReviewResolveProtectsPendingMirrorChildBeforeRetirement(t *testing.T) {
	const pendingChild = "44444444-eeee-4444-8444-444444444444"
	base := t.TempDir()
	root, winnerHome, loserHome, manager := filepath.Join(base, "project"), filepath.Join(base, "winner"), filepath.Join(base, "loser"), filepath.Join(base, "manager")
	mustMkdir(t, root)
	winnerPath := reviewWritePhysicalRollout(t, winnerHome, reviewSourceID, reviewOtherChildID, reviewPaginatedLines(t, reviewSourceID, root, nil, "chosen replacement"))
	if _, err := Init(InitOptions{ProjectRoot: root, ProfileHome: winnerHome, ManagerHome: manager, ImportExisting: true}); err != nil {
		t.Fatal(err)
	}
	ancestor := reviewPaginatedLines(t, reviewSourceID, root, nil, "inherited prefix", "preserved later tail")
	ancestorPath := reviewWritePhysicalRollout(t, loserHome, reviewSourceID, reviewSourceID, ancestor)
	ancestorBefore := mustReadFile(t, ancestorPath)
	loser := reviewPaginatedLines(t, reviewSourceID, root, reviewCutoff(reviewSourceID, ancestor[:2], 2), "losing tip delta")
	loserPath := reviewWritePhysicalRollout(t, loserHome, reviewSourceID, reviewChildID, loser)
	loserBefore := mustReadFile(t, loserPath)
	// Export the child's complete mirror closure, then remove its profile copy.
	// Resolve must still account for this dependency before retiring the source.
	childPath := reviewWriteRollout(t, loserHome, pendingChild, reviewPaginatedLines(t, pendingChild, root, reviewCutoff(reviewChildID, loser, 4), "pending mirror child"))
	sessions, err := scanProfile(loserHome, root)
	if err != nil {
		t.Fatal(err)
	}
	if err := exportProfileSession(root, sessions[pendingChild], nil); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(childPath); err != nil {
		t.Fatal(err)
	}

	if _, err := Resolve(ResolveOptions{ProjectRoot: root, ProfileHome: loserHome, ManagerHome: manager, SessionID: reviewSourceID, Use: "project"}); err == nil {
		t.Fatal("retired a branch still required by a pending mirror child")
	}
	if !bytes.Equal(ancestorBefore, mustReadFile(t, ancestorPath)) || !bytes.Equal(loserBefore, mustReadFile(t, loserPath)) {
		t.Fatal("blocked resolution changed the losing branch's original bytes")
	}
	relative, _ := filepath.Rel(winnerHome, winnerPath)
	if _, err := os.Stat(filepath.Join(loserHome, relative)); !os.IsNotExist(err) {
		t.Fatalf("blocked resolution wrote the conflicting winner: %v", err)
	}
}
