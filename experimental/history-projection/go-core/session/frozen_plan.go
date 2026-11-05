//go:build linux

package session

// This is an unintegrated recovery prototype. Sync and native launch paths do not
// call it. Its projection interface must be backed by the pinned native helper
// before any production guarantee is possible.

import (
    "bytes"
    "encoding/json"
    "errors"
    "fmt"
    "io"
    "os"
    "path/filepath"
    "sort"
    "strings"
    "syscall"
    "time"
)

const frozenPlanVersion = 1
const frozenMaxBytes = 128 << 20

// RecoveryRoots are supplied by local configuration, never by portable metadata.
// ProjectRoot is needed to prepare project writes. Recovery gets it from the
// authenticated local plan and checks the captured directory identity.
type RecoveryRoots struct {
    ManagerHome string `json:"manager_home"`
    ProfileHome string `json:"profile_home"`
    ProjectRoot string `json:"project_root"`
}

type frozenRootIdentity struct {
    Device uint64 `json:"device"`
    Inode uint64 `json:"inode"`
}

// FrozenFileChange is planning input. Scope is "profile" or "project". Project
// paths are relative to the project's .codexm directory, not its working tree.
type FrozenFileChange struct {
    Scope string
    Path string
    Data []byte
    Delete bool
}

type FrozenFileMutation struct {
    Scope string `json:"scope"`
    Path string `json:"path"`
    BeforeSHA256 string `json:"before_sha256"`
    BeforeBytes int64 `json:"before_bytes"`
    BeforeData []byte `json:"before_data,omitempty"`
    AfterSHA256 string `json:"after_sha256"`
    Data []byte `json:"data,omitempty"`
    Delete bool `json:"delete,omitempty"`
    RetainedTail bool `json:"retained_tail,omitempty"`
}

// FrozenRollout is the exact prefix used by native projection. Every physical
// ancestor needed by HistoryBase must also occur in this list. A destination file
// mutation, including a no-op retained file, must cover each rollout.
type FrozenRollout struct {
    RolloutID string `json:"rollout_id"`
    SessionID string `json:"session_id"`
    Path string `json:"path"`
    EndByteOffset uint64 `json:"end_byte_offset"`
    EndOrdinalExclusive uint64 `json:"end_ordinal_exclusive"`
    PrefixSHA256 string `json:"prefix_sha256"`
    Data []byte `json:"data"`
}

type FrozenRegistryCAS struct {
    SessionID string `json:"session_id"`
    Before *registration `json:"before,omitempty"`
    After *registration `json:"after,omitempty"`
}

type FrozenSyncPlan struct {
    Version int `json:"version"`
    CodexVersion string `json:"codex_version"`
    ProjectID string `json:"project_id"`
    Roots RecoveryRoots `json:"roots"`
    RootIdentities map[string]frozenRootIdentity `json:"root_identities"`
    Files []FrozenFileMutation `json:"files"`
    Rollouts []FrozenRollout `json:"rollouts"`
    Registry []FrozenRegistryCAS `json:"registry"`
}

// NewFrozenSyncPlan does not write anything. It captures full file preimages and
// all selected registry preimages. The caller still owns semantic reconciliation;
// this function validates bounded native closure and filesystem publication rules.
func NewFrozenSyncPlan(roots RecoveryRoots, projectID string, changes []FrozenFileChange, rollouts []FrozenRollout, desired map[string]*registration) (FrozenSyncPlan, error) {
    plan := FrozenSyncPlan{Version: frozenPlanVersion, CodexVersion: "0.166.0", ProjectID: projectID, Roots: roots,
        RootIdentities: map[string]frozenRootIdentity{}}
    if err := validateRecoveryRoots(roots, true); err != nil { return plan, err }
    if !validUUID(projectID) { return plan, errors.New("invalid frozen project ID") }
    for name, path := range map[string]string{"manager": roots.ManagerHome, "profile": roots.ProfileHome, "project": roots.ProjectRoot} {
        identity, err := frozenDirectoryIdentity(path)
        if err != nil { return plan, err }
        plan.RootIdentities[name] = identity
    }
    for _, change := range changes {
        path, err := frozenTarget(plan.Roots, change.Scope, change.Path)
        if err != nil { return plan, err }
        before, exists, err := frozenReadFile(path)
        if err != nil { return plan, err }
        op := FrozenFileMutation{Scope: change.Scope, Path: change.Path, Delete: change.Delete, Data: append([]byte(nil), change.Data...)}
        if exists { op.BeforeSHA256 = checksumBytes(before); op.BeforeBytes = int64(len(before)); op.BeforeData = before }
        if change.Delete && len(change.Data) != 0 { return plan, errors.New("delete mutation contains bytes") }
        // Portable history is immutable by physical ID. Preserve an existing
        // compatible longer tail, and allow only exact append for that same file.
        if exists && !change.Delete && isRolloutFile(change.Path) {
            if bytes.HasPrefix(before, op.Data) {
                op.RetainedTail = len(before) > len(op.Data)
                op.Data = append([]byte(nil), before...)
            } else if !bytes.HasPrefix(op.Data, before) {
                return plan, fmt.Errorf("non-prefix rollout replacement is unsupported: %s", change.Path)
            }
        }
        if !op.Delete { op.AfterSHA256 = checksumBytes(op.Data) }
        plan.Files = append(plan.Files, op)
    }
    for _, rollout := range rollouts {
        rollout.Data = append([]byte(nil), rollout.Data...)
        plan.Rollouts = append(plan.Rollouts, rollout)
    }
    reg, err := readFrozenRegistry(roots, projectID)
    if err != nil { return plan, err }
    for id, after := range desired {
        entry := FrozenRegistryCAS{SessionID: id, After: copyFrozenRegistration(after)}
        if before, exists := reg.Sessions[id]; exists { entry.Before = copyFrozenRegistration(&before) }
        plan.Registry = append(plan.Registry, entry)
    }
    sort.Slice(plan.Files, func(i, j int) bool { return plan.Files[i].Scope+":"+plan.Files[i].Path < plan.Files[j].Scope+":"+plan.Files[j].Path })
    sort.Slice(plan.Rollouts, func(i, j int) bool { return plan.Rollouts[i].RolloutID < plan.Rollouts[j].RolloutID })
    sort.Slice(plan.Registry, func(i, j int) bool { return plan.Registry[i].SessionID < plan.Registry[j].SessionID })
    return plan, validateFrozenPlan(plan)
}

func copyFrozenRegistration(value *registration) *registration {
    if value == nil { return nil }; copy := *value; return &copy
}

func validateRecoveryRoots(roots RecoveryRoots, projectRequired bool) error {
    paths := []string{roots.ManagerHome, roots.ProfileHome}
    if projectRequired || roots.ProjectRoot != "" { paths = append(paths, roots.ProjectRoot) }
    for _, path := range paths {
        if !filepath.IsAbs(path) || filepath.Clean(path) != path || path == string(filepath.Separator) { return errors.New("recovery requires canonical absolute configured roots") }
        if err := frozenCheckPath(path, false); err != nil { return err }
        info, err := os.Stat(path)
        if err != nil { return err }
        if !info.IsDir() { return errors.New("recovery root is not a directory") }
        st := info.Sys().(*syscall.Stat_t)
        if st.Uid != uint32(os.Geteuid()) || info.Mode().Perm()&0022 != 0 { return errors.New("recovery root must be owned by this user and not writable by others") }
    }
    for i := range paths { for j := i+1; j < len(paths); j++ {
        if paths[i] == paths[j] || strings.HasPrefix(paths[i], paths[j]+string(filepath.Separator)) || strings.HasPrefix(paths[j], paths[i]+string(filepath.Separator)) { return errors.New("recovery roots must be disjoint") }
    } }
    return nil
}

func frozenDirectoryIdentity(path string) (frozenRootIdentity, error) {
    info, err := os.Stat(path)
    if err != nil { return frozenRootIdentity{}, err }
    if !info.IsDir() { return frozenRootIdentity{}, errors.New("expected directory") }
    st := info.Sys().(*syscall.Stat_t)
    return frozenRootIdentity{Device: uint64(st.Dev), Inode: st.Ino}, nil
}

// No symlink is followed, including in ancestors. Existing regular files may not
// have a second hardlink. Concurrent hostile same-user path replacement is not
// excluded by this offline/cooperative protocol.
func frozenCheckPath(path string, missing bool) error {
    if !filepath.IsAbs(path) || filepath.Clean(path) != path { return errors.New("noncanonical recovery path") }
    parts := strings.Split(strings.TrimPrefix(path, string(filepath.Separator)), string(filepath.Separator))
    current := string(filepath.Separator)
    for _, part := range parts {
        current = filepath.Join(current, part)
        info, err := os.Lstat(current)
        if errors.Is(err, os.ErrNotExist) && missing { return nil }
        if err != nil { return err }
        if info.Mode()&os.ModeSymlink != 0 { return fmt.Errorf("symlink recovery path: %s", current) }
        if !info.IsDir() {
            if !info.Mode().IsRegular() || current != path { return fmt.Errorf("nonregular recovery path: %s", current) }
            if info.Sys().(*syscall.Stat_t).Nlink != 1 { return fmt.Errorf("hardlinked recovery path: %s", current) }
        }
    }
    return nil
}

func frozenReadFile(path string) ([]byte, bool, error) {
    if err := frozenCheckPath(path, true); err != nil { return nil, false, err }
    info, err := os.Stat(path)
    if errors.Is(err, os.ErrNotExist) { return nil, false, nil }
    if err != nil { return nil, false, err }
    if !info.Mode().IsRegular() || info.Size() > frozenMaxBytes { return nil, false, errors.New("invalid or oversized recovery file") }
    data, err := os.ReadFile(path)
    return data, true, err
}

func frozenTarget(roots RecoveryRoots, scope, relative string) (string, error) {
    var store transactionStore
    switch scope {
    case "profile": store = profileStore(roots.ProjectRoot, roots.ProfileHome)
    case "project": store = projectStore(roots.ProjectRoot)
    default: return "", errors.New("unknown frozen file scope")
    }
    // Existing allowlist excludes config/auth/SQLite/arbitrary working-tree files.
    target, err := store.target(relative)
    if err != nil { return "", err }
    return target, frozenCheckPath(target, true)
}

func frozenRegistryPath(roots RecoveryRoots, projectID string) string {
    return filepath.Join(roots.ManagerHome, "session-projects", projectID, pathKey(roots.ProfileHome)+".json")
}

func readFrozenRegistry(roots RecoveryRoots, projectID string) (*registry, error) {
    reg := &registry{FormatVersion: FormatVersion, ProjectID: projectID, ProjectRoot: roots.ProjectRoot, ProfileHome: roots.ProfileHome, Sessions: map[string]registration{}}
    data, exists, err := frozenReadFile(frozenRegistryPath(roots, projectID))
    if err != nil || !exists { return reg, err }
    if err := decodeFrozenJSON(data, reg); err != nil { return nil, fmt.Errorf("invalid frozen registry: %w", err) }
    if reg.FormatVersion != FormatVersion || reg.ProjectID != projectID || reg.ProjectRoot != roots.ProjectRoot || reg.ProfileHome != roots.ProfileHome || reg.Sessions == nil { return nil, errors.New("registry scope/version mismatch") }
    return reg, nil
}

func decodeFrozenJSON(data []byte, value any) error {
    decoder := json.NewDecoder(bytes.NewReader(data)); decoder.DisallowUnknownFields()
    if err := decoder.Decode(value); err != nil { return err }
    var extra any
    if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) { return errors.New("trailing recovery JSON") }
    return nil
}

func validateFrozenPlan(plan FrozenSyncPlan) error {
    if plan.Version != frozenPlanVersion || plan.CodexVersion != "0.166.0" || !validUUID(plan.ProjectID) { return errors.New("unsupported frozen plan identity/version") }
    if err := validateRecoveryRoots(plan.Roots, true); err != nil { return err }
    if len(plan.RootIdentities) != 3 { return errors.New("missing frozen root identities") }
    for name, path := range map[string]string{"manager": plan.Roots.ManagerHome, "profile": plan.Roots.ProfileHome, "project": plan.Roots.ProjectRoot} {
        identity, err := frozenDirectoryIdentity(path)
        if err != nil { return err }
        if identity != plan.RootIdentities[name] { return fmt.Errorf("frozen %s root was replaced", name) }
    }
    if len(plan.Files) == 0 || len(plan.Files) > 4096 || len(plan.Rollouts) > 1024 { return errors.New("invalid frozen plan size") }
    var total int64
    files := map[string]FrozenFileMutation{}
    for _, op := range plan.Files {
        if _, err := frozenTarget(plan.Roots, op.Scope, op.Path); err != nil { return err }
        key := op.Scope+":"+strings.ToLower(op.Path)
        if _, exists := files[key]; exists { return errors.New("duplicate frozen file mutation") }
        files[key] = op
        if op.BeforeSHA256 == "" {
            if op.BeforeBytes != 0 || len(op.BeforeData) != 0 { return errors.New("invalid absent file preimage") }
        } else if !validChecksum(op.BeforeSHA256) || op.BeforeBytes != int64(len(op.BeforeData)) || checksumBytes(op.BeforeData) != op.BeforeSHA256 { return errors.New("invalid full file preimage") }
        if op.Delete {
            if len(op.Data) != 0 || op.AfterSHA256 != "" || op.RetainedTail { return errors.New("invalid delete mutation") }
        } else if !validChecksum(op.AfterSHA256) || checksumBytes(op.Data) != op.AfterSHA256 { return errors.New("invalid desired file bytes") }
        if isRolloutFile(op.Path) {
            if strings.HasSuffix(op.Path, ".zst") { return errors.New("compressed recovery rollout is unsupported") }
            if !op.Delete && op.BeforeSHA256 != "" && !bytes.HasPrefix(op.Data, op.BeforeData) { return errors.New("frozen rollout replacement would change an existing prefix") }
        }
        if op.RetainedTail && (op.BeforeSHA256 == "" || op.BeforeSHA256 != op.AfterSHA256) { return errors.New("invalid retained-tail mutation") }
        if !op.Delete {
            if op.Path == "session_index.jsonl" { if err := validateSessionIndexData(op.Data); err != nil { return err } }
            if op.Scope == "project" && strings.HasPrefix(op.Path, "metadata/") {
                var metadata Metadata
                if err := decodeFrozenJSON(op.Data, &metadata); err != nil { return err }
                if !validUUID(metadata.SessionID) || filepath.Base(op.Path) != metadata.SessionID+".json" || (metadata.FormatVersion != FormatVersion && metadata.FormatVersion != HistoryFormatVersion) || !managedRolloutPath(metadata.RolloutPath) || !validChecksum(metadata.Checksum) { return errors.New("invalid frozen project metadata") }
            }
            if op.Scope == "project" && strings.HasPrefix(op.Path, "tombstones/") {
                var tombstone Tombstone
                if err := decodeFrozenJSON(op.Data, &tombstone); err != nil { return err }
                if !validUUID(tombstone.SessionID) || filepath.Base(op.Path) != tombstone.SessionID+".json" || tombstone.FormatVersion != FormatVersion || !validChecksum(tombstone.Checksum) { return errors.New("invalid frozen project tombstone") }
            }
        }
        total += int64(len(op.Data)+len(op.BeforeData))
    }
    if total > frozenMaxBytes { return errors.New("frozen mutations exceed size limit") }
    if err := validateFrozenClosure(plan, files); err != nil { return err }
    seen := map[string]bool{}
    for _, entry := range plan.Registry {
        if !validUUID(entry.SessionID) || seen[entry.SessionID] { return errors.New("invalid or duplicate registry CAS") }
        seen[entry.SessionID] = true
        for _, value := range []*registration{entry.Before, entry.After} {
            if value != nil && ((!value.Tombstone && !validUUID(value.RolloutID)) || !validChecksum(value.Checksum)) { return errors.New("invalid registry registration") }
        }
    }
    return nil
}

func validateFrozenClosure(plan FrozenSyncPlan, files map[string]FrozenFileMutation) error {
    byID := map[string]FrozenRollout{}
    bases := map[string]*HistoryPosition{}
    var total uint64
    for _, rollout := range plan.Rollouts {
        if !validUUID(rollout.RolloutID) || !validUUID(rollout.SessionID) || rollout.EndByteOffset == 0 || rollout.EndByteOffset > 64<<20 || rollout.EndByteOffset != uint64(len(rollout.Data)) || rollout.EndOrdinalExclusive == 0 || rollout.EndOrdinalExclusive >= 1<<63 || checksumBytes(rollout.Data) != rollout.PrefixSHA256 { return errors.New("invalid frozen rollout bounds/hash") }
        if _, exists := byID[rollout.RolloutID]; exists { return errors.New("duplicate frozen physical ID") }
        physical, err := physicalRolloutID(rollout.Path, rollout.SessionID)
        if err != nil || physical != rollout.RolloutID || strings.HasSuffix(rollout.Path, ".zst") { return errors.New("invalid frozen canonical rollout path") }
        op, exists := files["profile:"+strings.ToLower(rollout.Path)]
        if !exists || op.Delete || !bytes.HasPrefix(op.Data, rollout.Data) { return errors.New("projection prefix lacks matching desired destination bytes") }
        if len(op.Data) != len(rollout.Data) && op.BeforeSHA256 != op.AfterSHA256 { return errors.New("new or appended rollout has unprojected tail") }
        base, err := validateFrozenPrefix(rollout)
        if err != nil { return err }
        byID[rollout.RolloutID], bases[rollout.RolloutID] = rollout, base
        total += rollout.EndByteOffset
    }
    if total > frozenMaxBytes { return errors.New("frozen projection prefixes exceed size limit") }
    for _, op := range plan.Files {
        if op.Scope != "profile" || op.Delete || !isRolloutFile(op.Path) { continue }
        covered := false
        for _, rollout := range plan.Rollouts { if rollout.Path == op.Path { covered = true; break } }
        if !covered { return errors.New("profile rollout mutation omitted from projection closure") }
    }
    for _, entry := range plan.Registry {
        if entry.After != nil && !entry.After.Tombstone {
            rollout, exists := byID[entry.After.RolloutID]
            if !exists || rollout.SessionID != entry.SessionID { return errors.New("registry destination omitted from selected physical closure") }
        }
    }
    visiting, complete := map[string]bool{}, map[string]bool{}
    var visit func(string) error
    visit = func(id string) error {
        if complete[id] { return nil }
        if visiting[id] { return errors.New("frozen history dependency cycle") }
        visiting[id] = true
        if base := bases[id]; base != nil {
            source, exists := byID[base.ThreadID]
            if !exists || base.EndByteOffset > uint64(len(source.Data)) { return errors.New("incomplete frozen history closure") }
            prefix := source
            prefix.Data = prefix.Data[:base.EndByteOffset]
            prefix.EndByteOffset, prefix.EndOrdinalExclusive = base.EndByteOffset, base.EndOrdinalExclusive
            prefix.PrefixSHA256 = checksumBytes(prefix.Data)
            if _, err := validateFrozenPrefix(prefix); err != nil { return err }
            if err := visit(base.ThreadID); err != nil { return err }
        }
        delete(visiting, id); complete[id] = true
        return nil
    }
    for id := range byID { if err := visit(id); err != nil { return err } }
    return nil
}

func validateFrozenPrefix(rollout FrozenRollout) (*HistoryPosition, error) {
    if len(rollout.Data) == 0 || rollout.Data[len(rollout.Data)-1] != '\n' { return nil, errors.New("frozen prefix is not record bounded") }
    lines := bytes.Split(rollout.Data[:len(rollout.Data)-1], []byte{'\n'})
    var base *HistoryPosition
    var expected uint64
    for i, line := range lines {
        value, err := decodeObject(line)
        if err != nil { return nil, err }
        if _, err := time.Parse(time.RFC3339Nano, stringValue(value["timestamp"])); err != nil { return nil, errors.New("invalid frozen record timestamp") }
        ordinal, err := unsignedField(value, "ordinal")
        if err != nil { return nil, err }
        if i == 0 {
            if stringValue(value["type"]) != "session_meta" { return nil, errors.New("missing frozen session header") }
            payload, ok := value["payload"].(map[string]any)
            if !ok || stringValue(payload["id"]) != rollout.SessionID { return nil, errors.New("frozen session identity mismatch") }
            mode, parsed, err := parseHistoryMeta(value)
            if err != nil || mode != "paginated" { return nil, errors.New("frozen projection requires paginated history") }
            base = parsed
            if base != nil { expected = base.EndOrdinalExclusive }
        } else if stringValue(value["type"]) == "session_meta" { return nil, errors.New("duplicate frozen session header") }
        if ordinal != expected || ordinal >= 1<<63 { return nil, errors.New("frozen ordinal gap/regression") }
        expected++
    }
    if expected != rollout.EndOrdinalExclusive { return nil, errors.New("frozen ordinal cutoff mismatch") }
    return base, nil
}
