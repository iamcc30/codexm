package session

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func transactionFixture(t *testing.T) (historyFixture, *rollout) {
	t.Helper()
	f := newHistoryFixture(t)
	path := historyWrite(t, f.home, testSessionA, testSessionA, historyLines(t, testSessionA, f.root, nil, "before"), false, false)
	if _, err := Init(InitOptions{ProjectRoot: f.root, ProfileHome: f.home, ManagerHome: f.manager, ImportExisting: true}); err != nil {
		t.Fatal(err)
	}
	source, err := readRollout(path, f.root, nil)
	if err != nil {
		t.Fatal(err)
	}
	source.Relative, err = filepath.Rel(f.home, path)
	if err != nil {
		t.Fatal(err)
	}
	return f, source
}

func persistInterruptedTransaction(t *testing.T, root string, ops []projectMutation, completed int) {
	t.Helper()
	persistInterruptedStore(t, projectStore(root), ops, completed)
}

func persistInterruptedStore(t *testing.T, store transactionStore, ops []projectMutation, completed int) {
	t.Helper()
	var project Project
	if err := readJSON(filepath.Join(store.projectRoot, projectDirName, projectFileName), &project); err != nil {
		t.Fatal(err)
	}
	tx := projectTransaction{Version: 1, ProjectID: project.ProjectID, ProjectRoot: store.projectRoot, Operations: ops}
	for i := range tx.Operations {
		path, err := store.target(tx.Operations[i].Path)
		if err != nil {
			t.Fatal(err)
		}
		tx.Operations[i].Before, err = transactionDigest(path)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := atomicWriteJSON(store.journalPath(), tx, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, op := range ops[:completed] {
		target, err := store.target(op.Path)
		if err != nil {
			t.Fatal(err)
		}
		if op.Delete {
			err = os.Remove(target)
		} else {
			err = atomicWrite(target, op.Data, op.Mode)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestProjectTransactionRecoversEveryMutationBoundary(t *testing.T) {
	for _, kind := range []string{"export", "archive", "tombstone", "promotion"} {
		count := map[string]int{"export": 2, "archive": 3, "tombstone": 3, "promotion": 1}[kind]
		for completed := 0; completed <= count; completed++ {
			t.Run(fmt.Sprintf("%s/after-%d", kind, completed), func(t *testing.T) {
				f, source := transactionFixture(t)
				source.RawLines = historyLines(t, testSessionA, f.root, nil, "before", "after")
				sourcePath := historyWrite(t, f.home, testSessionA, testSessionA, source.RawLines, false, false)
				updated, err := readRollout(sourcePath, f.root, nil)
				if err != nil {
					t.Fatal(err)
				}
				updated.Relative = source.Relative
				oldRelative := filepath.ToSlash(source.Relative)
				if kind == "archive" {
					updated.Relative = filepath.Join("archived_sessions", filepath.Base(source.Relative))
					updated.Archived = true
				}
				data, err := encodeRollout(updated.RawLines, false)
				if err != nil {
					t.Fatal(err)
				}
				meta, err := projectJSONMutation("metadata/"+testSessionA+".json", metadataFor(updated))
				if err != nil {
					t.Fatal(err)
				}
				ops := []projectMutation{{Path: filepath.ToSlash(updated.Relative), Data: data, Mode: 0o600}, meta}
				if kind == "archive" {
					ops = append(ops, projectMutation{Path: oldRelative, Delete: true, Mode: 0o600})
				}
				if kind == "promotion" {
					meta, err = projectJSONMutation("metadata/"+testSessionA+".json", metadataFor(source))
					if err != nil {
						t.Fatal(err)
					}
					ops = []projectMutation{meta}
				}
				if kind == "tombstone" {
					tomb, err := projectJSONMutation("tombstones/"+testSessionA+".json", Tombstone{FormatVersion: FormatVersion, SessionID: testSessionA, DeletedAt: nowRFC3339(), Checksum: source.Checksum})
					if err != nil {
						t.Fatal(err)
					}
					ops = []projectMutation{tomb, {Path: "metadata/" + testSessionA + ".json", Delete: true, Mode: 0o644}, {Path: oldRelative, Delete: true, Mode: 0o600}}
				}
				persistInterruptedTransaction(t, f.root, ops, completed)
				journal := mustReadFile(t, transactionPath(f.root))
				if _, err := scanProject(f.root); err == nil || !strings.Contains(err.Error(), "pending project transaction") {
					t.Fatalf("read accepted interrupted state: %v", err)
				}
				if _, err := Sync(Options{ProjectRoot: f.root, ProfileHome: f.home, ManagerHome: f.manager, DryRun: true}); err == nil {
					t.Fatal("dry run accepted interrupted state")
				}
				if string(mustReadFile(t, transactionPath(f.root))) != string(journal) {
					t.Fatal("dry run changed journal")
				}
				if err := recoverProjectTransaction(f.root); err != nil {
					t.Fatal(err)
				}
				if err := recoverProjectTransaction(f.root); err != nil {
					t.Fatalf("repeat recovery: %v", err)
				}
				state, err := scanProject(f.root)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "tombstone" {
					if len(state.Sessions) != 0 || len(state.Tombstones) != 1 {
						t.Fatal("tombstone not recovered")
					}
				} else if len(state.Sessions) != 1 {
					t.Fatal("session missing")
				}
				if kind == "archive" && !state.Sessions[testSessionA].Archived {
					t.Fatal("archive missing")
				}
			})
		}
	}
}

func TestProjectTransactionRejectsUnsafeOrChangedJournal(t *testing.T) {
	for _, kind := range []string{"traversal", "absolute", "backslash", "unmanaged", "duplicate", "foreign", "changed", "invalid-digest", "trailing", "symlink", "metadata-directory", "tombstone-directory", "malformed-payload", "case-alias"} {
		t.Run(kind, func(t *testing.T) {
			f, source := transactionFixture(t)
			op := projectMutation{Path: filepath.ToSlash(source.Relative), Data: mustReadFile(t, source.Path), Mode: 0o600}
			persistInterruptedTransaction(t, f.root, []projectMutation{op}, 0)
			var tx projectTransaction
			if err := readJSON(transactionPath(f.root), &tx); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "case-alias":
				alias := tx.Operations[0]
				alias.Path = strings.Replace(alias.Path, "sessions/", "sessions/X/", 1)
				tx.Operations[0].Path = strings.Replace(tx.Operations[0].Path, "sessions/", "sessions/x/", 1)
				tx.Operations = append(tx.Operations, alias)
				tx.Operations[0].Before = ""
				tx.Operations[1].Before = ""

			case "metadata-directory":
				tx.Operations[0].Path = "metadata"
				tx.Operations[0].Data = []byte(`{}`)
			case "tombstone-directory":
				tx.Operations[0].Path = "tombstones"
				tx.Operations[0].Data = []byte(`{}`)
			case "malformed-payload":
				tx.Operations[0].Data = []byte("not JSON")
			case "traversal":
				tx.Operations[0].Path = "sessions/../../outside.jsonl"
			case "absolute":
				tx.Operations[0].Path = "/tmp/outside.jsonl"
			case "backslash":
				tx.Operations[0].Path = `sessions\..\outside.jsonl`
			case "unmanaged":
				tx.Operations[0].Path = "auth.json"
			case "duplicate":
				tx.Operations = append(tx.Operations, tx.Operations[0])
			case "foreign":
				tx.ProjectID = testSessionB
			case "changed":
				mustWriteFile(t, filepath.Join(f.root, projectDirName, source.Relative), "independent data\n")
			case "invalid-digest":
				tx.Operations[0].Before = "invalid"
			case "symlink":
				if runtime.GOOS == "windows" {
					t.Skip("symlink privilege varies")
				}
				original := filepath.Join(f.root, projectDirName, "sessions")
				moved := filepath.Join(filepath.Dir(f.root), "external-sessions")
				if err := os.Rename(original, moved); err != nil {
					t.Fatal(err)
				}
				if err := os.Symlink(moved, original); err != nil {
					t.Fatal(err)
				}
			}
			data, err := json.Marshal(tx)
			if err != nil {
				t.Fatal(err)
			}
			if kind == "trailing" {
				data = append(data, []byte(" junk")...)
			}
			if err := os.WriteFile(transactionPath(f.root), data, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := recoverProjectTransaction(f.root); err == nil {
				t.Fatal("unsafe journal accepted")
			}
			if string(mustReadFile(t, transactionPath(f.root))) != string(data) {
				t.Fatal("rejected journal changed")
			}
		})
	}
}

func TestProjectTransactionRetriesAfterWriteFailure(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permissions")
	}
	f, source := transactionFixture(t)
	path := historyWrite(t, f.home, testSessionA, testSessionA, historyLines(t, testSessionA, f.root, nil, "before", "continued"), false, false)
	_ = path
	_ = source
	metaDir := filepath.Join(f.root, projectDirName, "metadata")
	if err := os.Chmod(metaDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(metaDir, 0o755) })
	_, err := Sync(Options{ProjectRoot: f.root, ProfileHome: f.home, ManagerHome: f.manager})
	if err == nil {
		t.Skip("permissions not enforced")
	}
	if _, err := os.Stat(transactionPath(f.root)); err != nil {
		t.Fatalf("no recovery journal: %v", err)
	}
	if err := os.Chmod(metaDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := Sync(Options{ProjectRoot: f.root, ProfileHome: f.home, ManagerHome: f.manager}); err != nil {
		t.Fatalf("retry did not recover: %v", err)
	}
	if _, err := scanProject(f.root); err != nil {
		t.Fatal(err)
	}
}

func TestProfileTransactionRecoversEveryMutationBoundary(t *testing.T) {
	for _, kind := range []string{"archive", "retire", "dependency-chain", "delete-chain"} {
		count := 2
		if kind == "dependency-chain" || kind == "delete-chain" {
			count = 3
		}
		for completed := 0; completed <= count; completed++ {
			t.Run(fmt.Sprintf("%s/after-%d", kind, completed), func(t *testing.T) {
				f, source := transactionFixture(t)
				destination := filepath.Join(filepath.Dir(f.home), "destination")
				if err := os.MkdirAll(destination, 0o700); err != nil {
					t.Fatal(err)
				}
				desired := *source
				required := map[string]*rollout{}
				if kind == "archive" || kind == "retire" {
					historyWrite(t, destination, testSessionA, testSessionA, source.RawLines, false, false)
					if kind == "archive" {
						desired.Relative = filepath.Join("archived_sessions", filepath.Base(source.Relative))
						desired.Archived = true
					} else {
						target := historyWrite(t, f.home, testSessionA, testSessionB, historyLines(t, testSessionA, f.root, nil, "replacement branch"), false, false)
						parsed, err := readRollout(target, f.root, nil)
						if err != nil {
							t.Fatal(err)
						}
						desired = *parsed
						desired.Relative, err = filepath.Rel(f.home, target)
						if err != nil {
							t.Fatal(err)
						}
					}
				} else {
					secondLines := historyLines(t, testSessionA, f.root, historyBase(testSessionA, source.RawLines, 2), "second")
					secondPath := historyWrite(t, f.home, testSessionA, testSessionB, secondLines, false, false)
					thirdLines := historyLines(t, testSessionA, f.root, historyBase(testSessionB, secondLines, 4), "third")
					thirdPath := historyWrite(t, f.home, testSessionA, historyC, thirdLines, false, false)
					paths := map[string]string{testSessionA: source.Path, testSessionB: secondPath, historyC: thirdPath}
					tip, err := readRollout(thirdPath, f.root, nil)
					if err != nil {
						t.Fatal(err)
					}
					tip.Relative, err = filepath.Rel(f.home, thirdPath)
					if err != nil {
						t.Fatal(err)
					}
					if err := attachHistory(tip, f.root, paths); err != nil {
						t.Fatal(err)
					}
					for _, dependency := range tip.Dependencies {
						dependency.Relative, err = filepath.Rel(f.home, dependency.Path)
						if err != nil {
							t.Fatal(err)
						}
						required[dependency.RolloutID] = dependency
					}
					desired = *tip
				}
				ops, err := planProfileImportTransaction(f.root, destination, []*rollout{&desired}, required)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "delete-chain" {
					if err := applyProfileTransaction(f.root, destination, ops); err != nil {
						t.Fatal(err)
					}
					for i := range ops {
						ops[i].Delete = true
						ops[i].Data = nil
					}
				}
				if len(ops) != count {
					t.Fatalf("expected %d operations, got %d", count, len(ops))
				}
				store := profileStore(f.root, destination)
				persistInterruptedStore(t, store, ops, completed)
				if _, err := scanProfile(destination, f.root); err == nil || !strings.Contains(err.Error(), "pending profile transaction") {
					t.Fatalf("read accepted interrupted profile: %v", err)
				}
				if err := recoverProfileTransaction(f.root, destination); err != nil {
					t.Fatal(err)
				}
				if err := recoverProfileTransaction(f.root, destination); err != nil {
					t.Fatal(err)
				}
				state, err := scanProfile(destination, f.root)
				if err != nil {
					t.Fatal(err)
				}
				if kind == "delete-chain" {
					if len(state) != 0 {
						t.Fatal("deleted chain remains")
					}
					return
				}
				if len(state) != 1 || state[testSessionA].RolloutID != desired.RolloutID || state[testSessionA].Archived != desired.Archived {
					t.Fatalf("wrong recovered tip: %+v", state[testSessionA])
				}
				if kind == "dependency-chain" && len(state[testSessionA].Dependencies) != 2 {
					t.Fatal("dependency chain incomplete")
				}
			})
		}
	}
}

func TestProfileTransactionCannotChangeCredentialsOrAnotherProject(t *testing.T) {
	for _, kind := range []string{"credentials", "metadata", "different-project"} {
		t.Run(kind, func(t *testing.T) {
			f, source := transactionFixture(t)
			store := profileStore(f.root, f.home)
			ops := []projectMutation{{Path: filepath.ToSlash(source.Relative), Data: mustReadFile(t, source.Path), Mode: 0o600}}
			persistInterruptedStore(t, store, ops, 0)
			var tx projectTransaction
			if err := readJSON(store.journalPath(), &tx); err != nil {
				t.Fatal(err)
			}
			switch kind {
			case "credentials":
				tx.Operations[0].Path = "auth.json"
			case "metadata":
				tx.Operations[0].Path = "metadata/" + testSessionA + ".json"
			case "different-project":
				tx.ProjectID = testSessionB
			}
			if err := atomicWriteJSON(store.journalPath(), tx, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := recoverProfileTransaction(f.root, f.home); err == nil {
				t.Fatal("unsafe profile transaction accepted")
			}
			if _, err := os.Stat(store.journalPath()); err != nil {
				t.Fatal("journal was lost")
			}
		})
	}
}

func TestNamedFirstImportRecoversBeforeRegistryAndNameCommit(t *testing.T) {
	for completed := 0; completed <= 2; completed++ {
		t.Run(fmt.Sprintf("after-%d", completed), func(t *testing.T) {
			f, source := transactionFixture(t)
			source.Name = "synthetic named conversation"
			if err := appendSessionName(f.home, source.ID, source.Name); err != nil {
				t.Fatal(err)
			}
			if _, err := Sync(Options{ProjectRoot: f.root, ProfileHome: f.home, ManagerHome: f.manager}); err != nil {
				t.Fatal(err)
			}
			state, err := scanProject(f.root)
			if err != nil {
				t.Fatal(err)
			}
			destination := filepath.Join(filepath.Dir(f.home), "named-destination")
			if err := os.MkdirAll(destination, 0o700); err != nil {
				t.Fatal(err)
			}
			ops, err := planProfileImportTransaction(f.root, destination, []*rollout{state.Sessions[source.ID]}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(ops) != 2 {
				t.Fatalf("name and rollout must be one transaction: %+v", ops)
			}
			persistInterruptedStore(t, profileStore(f.root, destination), ops, completed)
			if _, err := Sync(Options{ProjectRoot: f.root, ProfileHome: destination, ManagerHome: f.manager}); err != nil {
				t.Fatalf("first import did not recover: %v", err)
			}
			names, err := readSessionNames(destination)
			if err != nil {
				t.Fatal(err)
			}
			if names[source.ID] != source.Name {
				t.Fatalf("name lost: %+v", names)
			}
			if _, err := Sync(Options{ProjectRoot: f.root, ProfileHome: destination, ManagerHome: f.manager}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProfileTransactionPreservesIndexAndRejectsPartialIndex(t *testing.T) {
	f, source := transactionFixture(t)
	source.Name = "new name"
	path := filepath.Join(f.home, "session_index.jsonl")
	previous := []byte(`{"id":"22222222-2222-4222-8222-222222222222","thread_name":"unrelated","extra":"preserved"}`)
	if err := os.WriteFile(path, previous, 0o600); err != nil {
		t.Fatal(err)
	}
	op, err := prepareSessionNameMutation(f.home, []*rollout{source})
	if err != nil {
		t.Fatal(err)
	}
	if op == nil || !strings.HasPrefix(string(op.Data), string(previous)+"\n") {
		t.Fatal("existing index bytes not preserved")
	}
	if err := applyProfileTransaction(f.root, f.home, []projectMutation{*op}); err != nil {
		t.Fatal(err)
	}
	names, err := readSessionNames(f.home)
	if err != nil || names[testSessionB] != "unrelated" || names[testSessionA] != source.Name {
		t.Fatalf("bad names: %+v %v", names, err)
	}
	if err := os.WriteFile(path, []byte(`{"id":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareSessionNameMutation(f.home, []*rollout{source}); err == nil {
		t.Fatal("partial index accepted")
	}
	if string(mustReadFile(t, path)) != `{"id":` {
		t.Fatal("partial index overwritten")
	}
}

// An unreadable but writable/searchable directory permits atomic file writes
// while denying directory fsync's open. This exercises the durability barrier;
// it is not a simulation of disk-cache loss or a power failure.
func TestHistorySnapshotAndVersionGateRequireParentSync(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory fsync and POSIX permissions")
	}
	t.Run("snapshot", func(t *testing.T) {
		f, source := transactionFixture(t)
		tip := &rollout{Dependencies: []*rollout{source}}
		descriptor := dependencyFor(source)
		path := filepath.Join(f.root, projectDirName, filepath.FromSlash(descriptor.SnapshotPath))
		dir := filepath.Dir(path)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(dir, 0o300); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		if probe, err := os.Open(dir); err == nil {
			_ = probe.Close()
			t.Skip("directory read permissions not enforced")
		}
		if err := writeHistoryDependencies(f.root, tip); err == nil {
			t.Fatal("snapshot write bypassed required parent-sync barrier")
		}
		if _, statErr := os.Stat(path); statErr != nil {
			t.Fatalf("write did not reach parent sync: %v", statErr)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := writeHistoryDependencies(f.root, tip); err != nil {
			t.Fatalf("snapshot barrier retry: %v", err)
		}
	})
	t.Run("version-gate", func(t *testing.T) {
		f := newHistoryFixture(t)
		if _, err := Init(InitOptions{ProjectRoot: f.root, ManagerHome: f.manager}); err != nil {
			t.Fatal(err)
		}
		dir := filepath.Join(f.root, projectDirName)
		if err := os.Chmod(dir, 0o300); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
		if probe, err := os.Open(dir); err == nil {
			_ = probe.Close()
			t.Skip("directory read permissions not enforced")
		}
		if err := requireHistoryVersion(f.root); err == nil {
			t.Fatal("version gate bypassed required parent-sync barrier")
		}
		var project Project
		if err := readJSON(filepath.Join(dir, projectFileName), &project); err != nil {
			t.Fatal(err)
		}
		if project.FormatVersion != HistoryFormatVersion {
			t.Fatal("gate write did not precede barrier")
		}
		if err := requireHistoryVersion(f.root); err == nil {
			t.Fatal("existing gate bypassed durability barrier")
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := requireHistoryVersion(f.root); err != nil {
			t.Fatalf("gate barrier retry: %v", err)
		}
	})
}
