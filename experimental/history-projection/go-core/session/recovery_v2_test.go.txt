//go:build linux

package session

import (
    "context"
    "encoding/json"
    "errors"
    "fmt"
    "os"
    "os/exec"
    "path/filepath"
    "reflect"
    "strings"
    "syscall"
    "testing"
    "time"
)

// The backend below is intentionally NOT SQLite or native Codex. These tests
// prove Go orchestration/bootstrap only. Native-helper integration remains a
// separate mandatory acceptance gate.
type frozenModelBackend struct { mode string }
type frozenModelEntry struct { Before *string `json:"before"`; After string `json:"after"` }
type frozenModelArtifact struct { Entries map[string]frozenModelEntry `json:"entries"` }
func (backend frozenModelBackend) Identity() string { return "test-only-model-v1" }
func frozenModelState(home string) (map[string]string, error) {
    data, exists, err := frozenReadFile(filepath.Join(home, "mock-history.json"))
    if err != nil { return nil, err }
    result := map[string]string{}
    if exists { err = decodeFrozenJSON(data, &result) }
    return result, err
}
func (backend frozenModelBackend) Prepare(ctx context.Context, request ProjectionRequest) (ProjectionArtifact, error) {
    current, err := frozenModelState(request.ProfileHome); if err != nil { return ProjectionArtifact{}, err }
    artifact := frozenModelArtifact{Entries: map[string]frozenModelEntry{}}
    for _, rollout := range request.Rollouts {
        entry := frozenModelEntry{After: rollout.PrefixSHA256}
        if value, exists := current[rollout.RolloutID]; exists { value := value; entry.Before = &value }
        artifact.Entries[rollout.RolloutID] = entry
    }
    data, _ := frozenJSON(artifact)
    if err := os.WriteFile(filepath.Join(request.WorkDir, "projection.sqlite"), data, 0600); err != nil { return ProjectionArtifact{}, err }
    return ProjectionArtifact{Path: "projection.sqlite", SHA256: checksumBytes(data)}, nil
}
func frozenLoadModelArtifact(request ProjectionRequest, artifact ProjectionArtifact) (frozenModelArtifact, error) {
    data, err := os.ReadFile(filepath.Join(request.WorkDir, artifact.Path))
    var result frozenModelArtifact
    if err != nil { return result, err }; err = decodeFrozenJSON(data, &result); return result, err
}
func (backend frozenModelBackend) Publish(ctx context.Context, request ProjectionRequest, artifact ProjectionArtifact) error {
    if backend.mode == "fail" { return errors.New("model failure before commit") }
    if backend.mode == "child" {
        env := append(os.Environ(), "CODEXM_FROZEN_TEST_ROLE=publisher-child", "CODEXM_FROZEN_TEST_PROFILE="+request.ProfileHome,
            "CODEXM_FROZEN_TEST_WORK="+request.WorkDir)
        _, err := request.Gate.Run(ctx, os.Args[0], []string{"-test.run=^TestFrozenV2Worker$"}, nil, env)
        return err
    }
    expected, err := frozenLoadModelArtifact(request, artifact); if err != nil { return err }
    current, err := frozenModelState(request.ProfileHome); if err != nil { return err }
    for id, entry := range expected.Entries {
        value, exists := current[id]
        if value == entry.After && exists { continue }
        if (entry.Before == nil && exists) || (entry.Before != nil && (!exists || value != *entry.Before)) { return errors.New("model selected partition conflict") }
        current[id] = entry.After
    }
    data, _ := frozenJSON(current)
    path := filepath.Join(request.ProfileHome, "mock-history.json")
    _, exists, err := frozenReadFile(path); if err != nil { return err }
    if err := frozenDurableWrite(path, data, !exists, request.ProfileHome, "mock-sql"); err != nil { return err }
    if backend.mode == "lost-ack" { return errors.New("model committed but acknowledgment lost") }
    return nil
}
func (backend frozenModelBackend) Verify(ctx context.Context, request ProjectionRequest, artifact ProjectionArtifact) error {
    expected, err := frozenLoadModelArtifact(request, artifact); if err != nil { return err }
    current, err := frozenModelState(request.ProfileHome); if err != nil { return err }
    for id, entry := range expected.Entries { if current[id] != entry.After { return errors.New("model projection not complete") } }
    return nil
}

type frozenFixture struct { roots RecoveryRoots; plan FrozenSyncPlan; request string }
const frozenTestProject = "00000000-0000-4000-8000-000000000101"
const frozenTestParent = "00000000-0000-4000-8000-000000000102"
const frozenTestChild = "00000000-0000-4000-8000-000000000103"
const frozenTestUnrelated = "00000000-0000-4000-8000-000000000104"

func frozenTestRollout(id string, base *HistoryPosition, records int) FrozenRollout {
    start := uint64(0); if base != nil { start = base.EndOrdinalExclusive }
    header := map[string]any{"timestamp": "2026-11-05T00:00:00Z", "type": "session_meta", "ordinal": start,
        "payload": map[string]any{"id": id, "history_mode": "paginated"}}
    if base != nil { header["payload"].(map[string]any)["history_base"] = base }
    data, _ := frozenJSON(header)
    for index := 1; index < records; index++ {
        line, _ := frozenJSON(map[string]any{"timestamp": "2026-11-05T00:00:00Z", "type": "event_msg", "ordinal": start+uint64(index), "payload": map[string]any{"type": "synthetic_test_only"}})
        data = append(data, line...)
    }
    return FrozenRollout{RolloutID: id, SessionID: id, Path: "sessions/2026/11/05/rollout-2026-11-05T00-00-00-"+id+".jsonl",
        EndByteOffset: uint64(len(data)), EndOrdinalExclusive: start+uint64(records), PrefixSHA256: checksumBytes(data), Data: data}
}
func newFrozenFixture(t *testing.T) frozenFixture {
    t.Helper()
    base := t.TempDir()
    roots := RecoveryRoots{ManagerHome: filepath.Join(base, "manager"), ProfileHome: filepath.Join(base, "profile"), ProjectRoot: filepath.Join(base, "project")}
    for _, path := range []string{roots.ManagerHome, roots.ProfileHome, roots.ProjectRoot, filepath.Join(roots.ProjectRoot, projectDirName)} { if err := os.MkdirAll(path, 0700); err != nil { t.Fatal(err) } }
    parent := frozenTestRollout(frozenTestParent, nil, 2)
    child := frozenTestRollout(frozenTestChild, &HistoryPosition{ThreadID: parent.RolloutID, EndByteOffset: parent.EndByteOffset, EndOrdinalExclusive: parent.EndOrdinalExclusive}, 2)
    old := frozenTestRollout(frozenTestParent, nil, 1)
    oldPath := filepath.Join(roots.ProfileHome, filepath.FromSlash(old.Path))
    if err := os.MkdirAll(filepath.Dir(oldPath), 0700); err != nil { t.Fatal(err) }
    if err := os.WriteFile(oldPath, old.Data, 0600); err != nil { t.Fatal(err) }
    metadata, _ := frozenJSON(Metadata{FormatVersion: HistoryFormatVersion, SessionID: child.SessionID, RolloutID: child.RolloutID, RolloutPath: child.Path, Checksum: checksumBytes(child.Data), RawChecksum: child.PrefixSHA256, LineCount: 2})
    changes := []FrozenFileChange{{Scope: "profile", Path: parent.Path, Data: parent.Data}, {Scope: "profile", Path: child.Path, Data: child.Data},
        {Scope: "project", Path: "metadata/"+frozenTestChild+".json", Data: metadata}}
    reg := &registry{FormatVersion: FormatVersion, ProjectID: frozenTestProject, ProjectRoot: roots.ProjectRoot, ProfileHome: roots.ProfileHome,
        Sessions: map[string]registration{frozenTestUnrelated: {RolloutID: frozenTestUnrelated, Checksum: checksumBytes([]byte("original")), Name: "unrelated"}}}
    if err := os.MkdirAll(filepath.Dir(frozenRegistryPath(roots, frozenTestProject)), 0700); err != nil { t.Fatal(err) }
    data, _ := frozenJSON(reg)
    if err := os.WriteFile(frozenRegistryPath(roots, frozenTestProject), data, 0600); err != nil { t.Fatal(err) }
    desired := map[string]*registration{frozenTestChild: {RolloutID: frozenTestChild, Checksum: checksumBytes(child.Data), Name: "child"}}
    plan, err := NewFrozenSyncPlan(roots, frozenTestProject, changes, []FrozenRollout{parent, child}, desired)
    if err != nil { t.Fatal(err) }
    request := filepath.Join(base, "caller-only-plan.json")
    data, _ = frozenJSON(plan)
    if err := os.WriteFile(request, data, 0600); err != nil { t.Fatal(err) }
    return frozenFixture{roots: roots, plan: plan, request: request}
}

func frozenWorker(t *testing.T, fixture frozenFixture, role, fault, mode string) *exec.Cmd {
    t.Helper()
    command := exec.Command(os.Args[0], "-test.run=^TestFrozenV2Worker$")
    command.Env = append(os.Environ(), "CODEXM_FROZEN_TEST_ROLE="+role, "CODEXM_FROZEN_TEST_REQUEST="+fixture.request,
        "CODEXM_FROZEN_TEST_MANAGER="+fixture.roots.ManagerHome, "CODEXM_FROZEN_TEST_PROFILE="+fixture.roots.ProfileHome,
        "CODEXM_FROZEN_TEST_FAULT="+fault, "CODEXM_FROZEN_TEST_MODE="+mode)
    return command
}
func TestFrozenV2Worker(t *testing.T) {
    role := os.Getenv("CODEXM_FROZEN_TEST_ROLE")
    if role == "" { return }
    if role == "publisher-child" {
        profile := os.Getenv("CODEXM_FROZEN_TEST_PROFILE")
        if err := os.WriteFile(filepath.Join(profile, "child-ready"), []byte("ready"), 0600); err != nil { os.Exit(81) }
        time.Sleep(1200*time.Millisecond)
        request := ProjectionRequest{ProfileHome: profile, WorkDir: os.Getenv("CODEXM_FROZEN_TEST_WORK")}
        if err := (frozenModelBackend{}).Publish(context.Background(), request, ProjectionArtifact{Path: "projection.sqlite"}); err != nil { os.Exit(82) }
        os.Exit(0)
    }
    if fault := os.Getenv("CODEXM_FROZEN_TEST_FAULT"); fault != "" {
        frozenRecoveryTestHook = func(point string) { if point == fault { _ = syscall.Kill(os.Getpid(), syscall.SIGKILL) } }
    }
    backend := frozenModelBackend{mode: os.Getenv("CODEXM_FROZEN_TEST_MODE")}
    var err error
    if role == "commit" {
        data, readErr := os.ReadFile(os.Getenv("CODEXM_FROZEN_TEST_REQUEST")); if readErr != nil { t.Fatal(readErr) }
        var plan FrozenSyncPlan; if err := decodeFrozenJSON(data, &plan); err != nil { t.Fatal(err) }
        _, err = CommitFrozenSync(context.Background(), plan, backend)
    } else if role == "recover" {
        // Deliberately no ProjectRoot or original plan. The original process and
        // request file are gone; trusted local bootstrap must supply everything.
        _, err = RecoverFrozenSync(context.Background(), RecoveryRoots{ManagerHome: os.Getenv("CODEXM_FROZEN_TEST_MANAGER"), ProfileHome: os.Getenv("CODEXM_FROZEN_TEST_PROFILE")}, backend)
    } else { t.Fatal("unexpected worker role") }
    if err != nil { t.Fatal(err) }
}

func frozenAssertComplete(t *testing.T, fixture frozenFixture) {
    t.Helper()
    if err := frozenVerifyFiles(fixture.plan); err != nil { t.Fatal(err) }
    if err := verifyFrozenRegistry(fixture.plan); err != nil { t.Fatal(err) }
    actual, err := frozenModelState(fixture.roots.ProfileHome); if err != nil { t.Fatal(err) }
    for _, rollout := range fixture.plan.Rollouts { if actual[rollout.RolloutID] != rollout.PrefixSHA256 { t.Fatal("model projection incomplete") } }
    if _, err := os.Lstat(frozenPendingPath(fixture.roots)); !errors.Is(err, os.ErrNotExist) { t.Fatal("pending barrier not cleared", err) }
}
func frozenExpectKilled(t *testing.T, command *exec.Cmd) {
    t.Helper()
    output, err := command.CombinedOutput()
    var exit *exec.ExitError
    if !errors.As(err, &exit) || !exit.ProcessState.Sys().(syscall.WaitStatus).Signaled() || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL { t.Fatalf("fault not reached: %v: %s", err, output) }
}

func TestFrozenV2DurableBootstrapAcrossProcessDeath(t *testing.T) {
    points := []string{"pending:renamed", "pending:durable", "files:verified", "sql:before-publish", "sql:returned", "sql:verified", "registry:verified", "all:verified", "pending:removed", "pending:cleared"}
    for index := 0; index < 3; index++ { for _, phase := range []string{"partial", "written", "synced", "renamed", "durable"} { points = append(points, fmt.Sprintf("file:%d:%s", index, phase)) } }
    for _, group := range []string{"mock-sql", "registry", "complete"} { for _, phase := range []string{"partial", "written", "synced", "renamed", "durable"} { points = append(points, group+":"+phase) } }
    for _, point := range points { t.Run(point, func(t *testing.T) {
        fixture := newFrozenFixture(t)
        frozenExpectKilled(t, frozenWorker(t, fixture, "commit", point, ""))
        if err := os.Remove(fixture.request); err != nil { t.Fatal(err) }
        reg, err := readFrozenRegistry(fixture.roots, frozenTestProject); if err != nil { t.Fatal(err) }
        unrelated := reg.Sessions[frozenTestUnrelated]; unrelated.Name = "independent after kill"; reg.Sessions[frozenTestUnrelated] = unrelated
        data, _ := frozenJSON(reg); if err := os.WriteFile(frozenRegistryPath(fixture.roots, frozenTestProject), data, 0600); err != nil { t.Fatal(err) }
        history, err := frozenModelState(fixture.roots.ProfileHome); if err != nil { t.Fatal(err) }
        history[frozenTestUnrelated] = "independent projection"
        data, _ = frozenJSON(history); if err := os.WriteFile(filepath.Join(fixture.roots.ProfileHome, "mock-history.json"), data, 0600); err != nil { t.Fatal(err) }
        preserved := filepath.Join(fixture.roots.ProfileHome, "unrelated-file")
        if err := os.WriteFile(preserved, []byte("independent file"), 0600); err != nil { t.Fatal(err) }
        output, err := frozenWorker(t, fixture, "recover", "", "").CombinedOutput(); if err != nil { t.Fatalf("recover without caller failed: %v %s", err, output) }
        frozenAssertComplete(t, fixture)
        reg, _ = readFrozenRegistry(fixture.roots, frozenTestProject); if reg.Sessions[frozenTestUnrelated].Name != "independent after kill" { t.Fatal("unrelated registry entry lost") }
        history, _ = frozenModelState(fixture.roots.ProfileHome); if history[frozenTestUnrelated] != "independent projection" { t.Fatal("unrelated projection lost") }
        data, _ = os.ReadFile(preserved); if string(data) != "independent file" { t.Fatal("independent file lost") }
        output, err = frozenWorker(t, fixture, "recover", "", "").CombinedOutput(); if err != nil { t.Fatalf("idempotent recovery failed: %v %s", err, output) }
    }) }
}

func TestFrozenV2PreparationDeathHasNoPublicMutation(t *testing.T) {
    for _, point := range []string{"authority:partial", "authority:renamed", "input:partial", "input:renamed", "plan:partial", "plan:renamed", "prepare:durable", "pending:partial", "pending:synced"} { t.Run(point, func(t *testing.T) {
        fixture := newFrozenFixture(t)
        frozenExpectKilled(t, frozenWorker(t, fixture, "commit", point, ""))
        if err := frozenCheckPreimages(fixture.plan, false); err != nil { t.Fatal("pre-intent death changed targets", err) }
        if _, err := RecoverFrozenSync(context.Background(), RecoveryRoots{ManagerHome: fixture.roots.ManagerHome, ProfileHome: fixture.roots.ProfileHome}, frozenModelBackend{}); err != nil { t.Fatal(err) }
        if err := frozenCheckPreimages(fixture.plan, false); err != nil { t.Fatal(err) }
        if _, err := CommitFrozenSync(context.Background(), fixture.plan, frozenModelBackend{}); err != nil { t.Fatal("retry after preparation death failed", err) }
        frozenAssertComplete(t, fixture)
    }) }
}

func TestFrozenV2LostAcknowledgmentAndVerifyFailure(t *testing.T) {
    fixture := newFrozenFixture(t)
    result, err := CommitFrozenSync(context.Background(), fixture.plan, frozenModelBackend{mode: "lost-ack"})
    if err != nil || !result.PublishOutcomeUncertain { t.Fatal("lost acknowledgment was not reconciled", result, err) }
    frozenAssertComplete(t, fixture)
    failed := newFrozenFixture(t)
    if _, err := CommitFrozenSync(context.Background(), failed.plan, frozenModelBackend{mode: "fail"}); err == nil { t.Fatal("failed projection reported success") }
    reg, err := readFrozenRegistry(failed.roots, frozenTestProject); if err != nil { t.Fatal(err) }
    if _, exists := reg.Sessions[frozenTestChild]; exists { t.Fatal("registry published before verified projection") }
    if _, err := os.Stat(frozenPendingPath(failed.roots)); err != nil { t.Fatal("failure lost recovery barrier", err) }
    if _, err := RecoverFrozenSync(context.Background(), failed.roots, frozenModelBackend{}); err != nil { t.Fatal(err) }
    frozenAssertComplete(t, failed)
}

func TestFrozenV2RejectsTamperingAndPreservesConflicts(t *testing.T) {
    for _, kind := range []string{"plan", "unknown-field", "pointer", "key-missing", "snapshot", "artifact", "file", "registry", "projection", "wrong-manager", "symlink", "hardlink", "root-identity"} { t.Run(kind, func(t *testing.T) {
        fixture := newFrozenFixture(t)
        frozenExpectKilled(t, frozenWorker(t, fixture, "commit", "sql:returned", ""))
        record, directory, err := loadFrozenRecord(fixture.roots, frozenModelBackend{}); if err != nil { t.Fatal(err) }
        recoverRoots := RecoveryRoots{ManagerHome: fixture.roots.ManagerHome, ProfileHome: fixture.roots.ProfileHome}
        switch kind {
        case "plan":
            record.Body.Plan.Files[0].Data = []byte("untrusted replacement")
            data, _ := frozenJSON(record); if err := os.WriteFile(filepath.Join(directory, "plan.json"), data, 0600); err != nil { t.Fatal(err) }
        case "unknown-field":
            path := filepath.Join(directory, "plan.json"); data, _ := os.ReadFile(path)
            var value map[string]any; _ = json.Unmarshal(data, &value); value["unknown"] = true; data, _ = frozenJSON(value); if err := os.WriteFile(path, data, 0600); err != nil { t.Fatal(err) }
        case "pointer":
            var pending frozenPending; data, _ := os.ReadFile(frozenPendingPath(fixture.roots)); _ = decodeFrozenJSON(data, &pending); pending.OperationID = strings.Repeat("0", 64); data, _ = frozenJSON(pending); if err := os.WriteFile(frozenPendingPath(fixture.roots), data, 0600); err != nil { t.Fatal(err) }
        case "key-missing":
            if err := os.Remove(filepath.Join(frozenAuthority(fixture.roots), "authority.key")); err != nil { t.Fatal(err) }
        case "snapshot":
            path := filepath.Join(directory, "inputs", filepath.Base(fixture.plan.Rollouts[0].Path)); _ = os.Chmod(path, 0600); if err := os.WriteFile(path, []byte("changed"), 0600); err != nil { t.Fatal(err) }; _ = os.Chmod(path, 0400)
        case "artifact":
            path := filepath.Join(directory, "projection.sqlite"); _ = os.Chmod(path, 0600); if err := os.WriteFile(path, []byte("changed"), 0600); err != nil { t.Fatal(err) }
        case "file":
            path, _ := frozenTarget(fixture.roots, fixture.plan.Files[0].Scope, fixture.plan.Files[0].Path); if err := os.WriteFile(path, []byte("independent file edit"), 0600); err != nil { t.Fatal(err) }
        case "registry":
            reg, _ := readFrozenRegistry(fixture.roots, frozenTestProject); reg.Sessions[frozenTestChild] = registration{RolloutID: frozenTestChild, Checksum: checksumBytes([]byte("other")), Name: "independent"}; data, _ := frozenJSON(reg); if err := os.WriteFile(frozenRegistryPath(fixture.roots, frozenTestProject), data, 0600); err != nil { t.Fatal(err) }
        case "projection":
            history, _ := frozenModelState(fixture.roots.ProfileHome); history[frozenTestChild] = "independent"; data, _ := frozenJSON(history); if err := os.WriteFile(filepath.Join(fixture.roots.ProfileHome, "mock-history.json"), data, 0600); err != nil { t.Fatal(err) }
        case "wrong-manager":
            recoverRoots.ManagerHome = filepath.Join(filepath.Dir(fixture.roots.ManagerHome), "other-manager"); if err := os.Mkdir(recoverRoots.ManagerHome, 0700); err != nil { t.Fatal(err) }
        case "symlink", "hardlink":
            target, _ := frozenTarget(fixture.roots, fixture.plan.Files[0].Scope, fixture.plan.Files[0].Path)
            if kind == "hardlink" { if err := os.Link(target, filepath.Join(fixture.roots.ProfileHome, "other-link")); err != nil { t.Fatal(err) } } else {
                original := target+".original"; if err := os.Rename(target, original); err != nil { t.Fatal(err) }; if err := os.Symlink(original, target); err != nil { t.Fatal(err) }
            }
        case "root-identity":
            original := fixture.roots.ProjectRoot+".old"; if err := os.Rename(fixture.roots.ProjectRoot, original); err != nil { t.Fatal(err) }; if err := os.Mkdir(fixture.roots.ProjectRoot, 0700); err != nil { t.Fatal(err) }
        }
        registryBefore, _ := os.ReadFile(frozenRegistryPath(fixture.roots, frozenTestProject))
        historyBefore, _ := os.ReadFile(filepath.Join(fixture.roots.ProfileHome, "mock-history.json"))
        if _, err := RecoverFrozenSync(context.Background(), recoverRoots, frozenModelBackend{}); err == nil { t.Fatal("tamper/conflict accepted", kind) }
        registryAfter, _ := os.ReadFile(frozenRegistryPath(fixture.roots, frozenTestProject)); historyAfter, _ := os.ReadFile(filepath.Join(fixture.roots.ProfileHome, "mock-history.json"))
        if string(registryBefore) != string(registryAfter) || string(historyBefore) != string(historyAfter) { t.Fatal("conflict overwrote independent state") }
        if _, err := os.Lstat(frozenPendingPath(fixture.roots)); err != nil { t.Fatal("conflict cleared startup barrier", err) }
    }) }
}

func TestFrozenV2OrphanChildRetainsGate(t *testing.T) {
    fixture := newFrozenFixture(t)
    command := frozenWorker(t, fixture, "commit", "", "child")
    if err := command.Start(); err != nil { t.Fatal(err) }
    deadline := time.Now().Add(5*time.Second)
    ready := filepath.Join(fixture.roots.ProfileHome, "child-ready")
    for { if _, err := os.Stat(ready); err == nil { break }; if time.Now().After(deadline) { command.Process.Kill(); command.Wait(); t.Fatal("publisher child did not pause") }; time.Sleep(10*time.Millisecond) }
    if err := command.Process.Kill(); err != nil { t.Fatal(err) }; _ = command.Wait()
    if err := os.Remove(fixture.request); err != nil { t.Fatal(err) }
    if gate, err := OpenRecoveryGate(fixture.roots.ProfileHome); err == nil { gate.Close(); t.Fatal("orphan child did not keep gate") }
    if _, err := RecoverFrozenSync(context.Background(), fixture.roots, frozenModelBackend{}); err == nil { t.Fatal("recovery ran while orphan child active") }
    otherHome := filepath.Join(filepath.Dir(fixture.roots.ProfileHome), "orphan-other-profile")
    if err := os.Mkdir(otherHome, 0700); err != nil { t.Fatal(err) }
    otherGate, err := OpenRecoveryGate(otherHome); if err != nil { t.Fatal(err) }; defer otherGate.Close()
    if err := otherGate.acquireProject(fixture.roots.ProjectRoot); err == nil { t.Fatal("orphan child did not retain inherited project gate") }
    for {
        gate, err := OpenRecoveryGate(fixture.roots.ProfileHome)
        if err == nil { gate.Close(); break }
        if time.Now().After(deadline) { t.Fatal("orphan never released gate", err) }
        time.Sleep(20*time.Millisecond)
    }
    if err := otherGate.acquireProject(fixture.roots.ProjectRoot); err != nil { t.Fatal("orphan project gate did not release", err) }
    if err := otherGate.Close(); err != nil { t.Fatal(err) }
    output, err := frozenWorker(t, fixture, "recover", "", "").CombinedOutput(); if err != nil { t.Fatalf("recovery after orphan failed: %v %s", err, output) }
    frozenAssertComplete(t, fixture)
}

func TestFrozenV2RetainsCompatibleTailAndRejectsIncompleteClosure(t *testing.T) {
    fixture := newFrozenFixture(t)
    first := fixture.plan.Rollouts[0]
    // Choose the physical root explicitly rather than depending on sort order.
    for _, item := range fixture.plan.Rollouts { if item.RolloutID == frozenTestParent { first = item } }
    longer := frozenTestRollout(first.RolloutID, nil, 3)
    target := filepath.Join(fixture.roots.ProfileHome, filepath.FromSlash(first.Path))
    if err := os.WriteFile(target, longer.Data, 0600); err != nil { t.Fatal(err) }
    changes := []FrozenFileChange{}
    for _, op := range fixture.plan.Files { changes = append(changes, FrozenFileChange{Scope: op.Scope, Path: op.Path, Data: op.Data}) }
    plan, err := NewFrozenSyncPlan(fixture.roots, frozenTestProject, changes, fixture.plan.Rollouts, nil)
    if err != nil { t.Fatal(err) }
    if _, err := CommitFrozenSync(context.Background(), plan, frozenModelBackend{}); err != nil { t.Fatal(err) }
    current, _ := os.ReadFile(target); if string(current) != string(longer.Data) { t.Fatal("compatible tail was truncated") }
    fresh := newFrozenFixture(t)
    bad := fresh.plan; bad.Rollouts = []FrozenRollout{fresh.plan.Rollouts[1]}
    if err := validateFrozenPlan(bad); err == nil { t.Fatal("incomplete ancestry accepted") }
    bad = fresh.plan; bad.Files = append([]FrozenFileMutation(nil), fresh.plan.Files...); bad.Files[0].Path = "../auth.json"
    if err := validateFrozenPlan(bad); err == nil { t.Fatal("path escape accepted") }
    bad = fresh.plan; bad.Files = append([]FrozenFileMutation(nil), fresh.plan.Files...); bad.Files[0].Data = []byte("changed prefix"); bad.Files[0].AfterSHA256 = checksumBytes(bad.Files[0].Data)
    if err := validateFrozenPlan(bad); err == nil { t.Fatal("destructive replacement accepted") }
}

func TestFrozenV2CompletionEvidenceCannotBypassVerification(t *testing.T) {
    fixture := newFrozenFixture(t)
    frozenExpectKilled(t, frozenWorker(t, fixture, "commit", "pending:durable", ""))
    record, directory, err := loadFrozenRecord(fixture.roots, frozenModelBackend{}); if err != nil { t.Fatal(err) }
    if err := os.WriteFile(filepath.Join(directory, "complete"), []byte(record.OperationID+"\n"), 0600); err != nil { t.Fatal(err) }
    if _, err := RecoverFrozenSync(context.Background(), fixture.roots, frozenModelBackend{mode: "fail"}); err == nil { t.Fatal("forged completion bypassed projection verification") }
    if _, err := os.Stat(frozenPendingPath(fixture.roots)); err != nil { t.Fatal("forged completion cleared barrier") }
}

func TestFrozenV2ContextCancellationReapsChild(t *testing.T) {
    fixture := newFrozenFixture(t)
    gate, err := OpenRecoveryGate(fixture.roots.ProfileHome); if err != nil { t.Fatal(err) }
    ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond); defer cancel()
    env := append(os.Environ(), "CODEXM_FROZEN_TEST_ROLE=publisher-child", "CODEXM_FROZEN_TEST_PROFILE="+fixture.roots.ProfileHome, "CODEXM_FROZEN_TEST_WORK="+t.TempDir())
    if _, err := gate.Run(ctx, os.Args[0], []string{"-test.run=^TestFrozenV2Worker$"}, nil, env); err == nil { t.Fatal("child timeout not reported") }
    if err := gate.Close(); err != nil { t.Fatal(err) }
    next, err := OpenRecoveryGate(fixture.roots.ProfileHome); if err != nil { t.Fatal("cancelled child still owns gate", err) }; next.Close()
}

func TestFrozenV2PreparationIsPureAndSelectedRegistryCAS(t *testing.T) {
    fixture := newFrozenFixture(t)
    for _, path := range []string{frozenAuthority(fixture.roots), filepath.Join(fixture.roots.ProfileHome, recoveryV2Directory)} { if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) { t.Fatal("pure planner wrote recovery state", path, err) } }
    if err := frozenCheckPreimages(fixture.plan, false); err != nil { t.Fatal(err) }
    encoded, err := frozenJSON(fixture.plan); if err != nil { t.Fatal(err) }
    var copied FrozenSyncPlan; if err := decodeFrozenJSON(encoded, &copied); err != nil { t.Fatal(err) }
    if !reflect.DeepEqual(copied, fixture.plan) { t.Fatal("plan did not round-trip exactly") }
}

func TestFrozenV2SharedProjectGateExcludesDifferentProfiles(t *testing.T) {
    fixture := newFrozenFixture(t)
    first, err := OpenRecoveryGate(fixture.roots.ProfileHome); if err != nil { t.Fatal(err) }; defer first.Close()
    if err := first.acquireProject(fixture.roots.ProjectRoot); err != nil { t.Fatal(err) }
    secondHome := filepath.Join(filepath.Dir(fixture.roots.ProfileHome), "second-profile")
    if err := os.Mkdir(secondHome, 0700); err != nil { t.Fatal(err) }
    second, err := OpenRecoveryGate(secondHome); if err != nil { t.Fatal(err) }; defer second.Close()
    if err := second.acquireProject(fixture.roots.ProjectRoot); err == nil { t.Fatal("different-profile writer bypassed shared project gate") }
    if err := first.Close(); err != nil { t.Fatal(err) }
    if err := second.acquireProject(fixture.roots.ProjectRoot); err != nil { t.Fatal("project gate remained held", err) }
}

func TestFrozenV2DeletionRecoversWithoutRestoringSnapshot(t *testing.T) {
    fixture := newFrozenFixture(t)
    path := filepath.Join(fixture.roots.ProjectRoot, projectDirName, "metadata", frozenTestUnrelated+".json")
    if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil { t.Fatal(err) }
    if err := os.WriteFile(path, []byte("old selected metadata"), 0600); err != nil { t.Fatal(err) }
    changes := []FrozenFileChange{}
    for _, op := range fixture.plan.Files { changes = append(changes, FrozenFileChange{Scope: op.Scope, Path: op.Path, Data: op.Data}) }
    changes = append(changes, FrozenFileChange{Scope: "project", Path: "metadata/"+frozenTestUnrelated+".json", Delete: true})
    desired := map[string]*registration{frozenTestChild: fixture.plan.Registry[0].After}
    plan, err := NewFrozenSyncPlan(fixture.roots, frozenTestProject, changes, fixture.plan.Rollouts, desired)
    if err != nil { t.Fatal(err) }; fixture.plan = plan
    data, _ := frozenJSON(plan); if err := os.WriteFile(fixture.request, data, 0600); err != nil { t.Fatal(err) }
    frozenExpectKilled(t, frozenWorker(t, fixture, "commit", "file:3:removed", ""))
    if err := os.Remove(fixture.request); err != nil { t.Fatal(err) }
    output, err := frozenWorker(t, fixture, "recover", "", "").CombinedOutput(); if err != nil { t.Fatalf("delete recovery failed: %v %s", err, output) }
    if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) { t.Fatal("deleted selected file restored", err) }
    frozenAssertComplete(t, fixture)
}
