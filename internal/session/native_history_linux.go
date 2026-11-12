package session

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/iamcc30/codexm/internal/historycompat"
)

// SyncNativeHistory is offline, explicit maintenance for one existing paginated
// thread. The stopped-writer assertion is a prerequisite, not a concurrency proof.
func SyncNativeHistory(opts NativeHistoryOptions) (Result, error) {
	result := Result{}
	if !opts.StoppedWriters {
		return result, errors.New("native-history sync requires --native-writers-stopped; stop all external Codex clients and keep them stopped until completion")
	}
	if opts.DryRun || opts.Lease != nil {
		return result, errors.New("native-history sync requires its own maintenance gate and is not a status/dry-run operation")
	}
	root, project, err := resolveOptions(&opts.Options)
	if err != nil {
		return result, err
	}
	roots := RecoveryRoots{ManagerHome: opts.ManagerHome, ProfileHome: opts.ProfileHome, ProjectRoot: root}
	if err := validateRecoveryRoots(roots, true); err != nil {
		return result, err
	}
	for _, path := range []string{roots.ManagerHome, roots.ProjectRoot, roots.ProfileHome,
		filepath.Join(roots.ManagerHome, "session-recovery-v2"), filepath.Join(roots.ProjectRoot, projectDirName),
		filepath.Join(roots.ProfileHome, ".codexm-sync-v2"), filepath.Join(roots.ProfileHome, "state_5.sqlite"), filepath.Join(roots.ProfileHome, "thread_history_1.sqlite")} {
		if err := nativeLocalFilesystem(path); err != nil {
			return result, err
		}
	}
	executable, err := os.Executable()
	if err != nil {
		return result, err
	}
	helper := filepath.Join(filepath.Dir(executable), "native_history_helper")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	request := nativePreflightRequest(opts, root, helper)
	snapshot, err := historycompat.Check(ctx, request)
	if err != nil {
		return result, err
	}
	gate, err := OpenRecoveryGate(opts.ProfileHome)
	if err != nil {
		return result, err
	}
	defer gate.Close()
	if err := gate.acquireProject(root); err != nil {
		return result, err
	}
	if err := snapshot.Revalidate(ctx, request); err != nil {
		return result, err
	}
	// Refuse an older file-only journal instead of replaying it outside this intent.
	for _, path := range []string{transactionPath(root), profileStore(root, opts.ProfileHome).journalPath()} {
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return result, fmt.Errorf("old file-only transaction is present or unreadable; resolve it before native maintenance: %s", path)
		}
	}
	backend := &nativeHistoryBackend{helper: helper, gate: gate, home: opts.ProfileHome}
	defer backend.Close()
	if _, exists, err := frozenReadFile(frozenPendingPath(roots)); err != nil {
		return result, err
	} else if exists {
		recovered, err := recoverFrozenSyncLocked(ctx, roots, backend, gate)
		if err != nil {
			return result, err
		}
		result.Actions = append(result.Actions, "recovered native-history operation "+recovered.OperationID)
		return result, nil
	}
	projects, profiles, reg, _, err := loadReconcileState(opts.Options, root, project)
	if err != nil {
		return result, err
	}
	id := opts.SessionID
	if id == "" {
		if len(projects.Sessions) != 1 {
			return result, errors.New("select one existing logical thread with --session UUID")
		}
		for selected := range projects.Sessions {
			id = selected
		}
	}
	if !validUUID(id) {
		return result, errors.New("invalid native-history session ID")
	}
	source, previous := projects.Sessions[id], profiles[id]
	if source == nil || previous == nil || source.Archived || previous.Archived || source.Compressed || previous.Compressed || source.HistoryMode != "paginated" || previous.HistoryMode != "paginated" {
		return result, errors.New("native-history sync requires existing active uncompressed paginated project and profile threads")
	}
	if _, deleted := projects.Tombstones[id]; deleted {
		return result, errors.New("native-history deletion is unsupported")
	}
	base, owned := reg.Sessions[id]
	conflict, winner := compareSessions(source, previous, base, owned)
	if conflict != "" || winner == "profile" {
		return result, errors.New("native-history import conflicts with profile changes; export or resolve those changes first")
	}
	plan, err := planNativeHistory(roots, project.ProjectID, source)
	if err != nil {
		return result, err
	}
	for _, mutation := range plan.Files {
		path, err := frozenTarget(plan.Roots, mutation.Scope, mutation.Path)
		if err != nil {
			return result, err
		}
		if err := nativeLocalFilesystem(path); err != nil {
			return result, err
		}
	}
	if err := backend.acquire(ctx, plan.Rollouts); err != nil {
		return result, err
	}
	var inspected struct {
		Tips []struct {
			ThreadID     string `json:"thread_id"`
			ExpectedPath string `json:"expected_path"`
		} `json:"tips"`
	}
	if err := backend.command(ctx, map[string]any{"action": "inspect", "thread_ids": []string{id}}, &inspected); err != nil {
		return result, err
	}
	if len(inspected.Tips) != 1 || inspected.Tips[0].ThreadID != id {
		return result, errors.New("native helper returned inconsistent selected metadata")
	}
	plan.NativeTips = []FrozenNativeTip{{ThreadID: id, ExpectedPath: inspected.Tips[0].ExpectedPath, NewPath: source.Relative}}
	if err := validateFrozenPlan(plan); err != nil {
		return result, err
	}
	// This permanent marker restricts codexm launch routes; it does not stop a user
	// deliberately invoking an external native binary against the same profile.
	managed, err := NativeHistoryManaged(opts.ProfileHome)
	if err != nil {
		return result, err
	}
	if !managed {
		if err := frozenDurableWrite(filepath.Join(opts.ProfileHome, nativeManagedFile), []byte("codexm offline native history v1\n"), true, opts.ProfileHome, "managed"); err != nil {
			return result, err
		}
	}
	if err := snapshot.Revalidate(ctx, request); err != nil {
		return result, err
	}
	operation, err := commitFrozenSyncLocked(ctx, plan, backend, gate)
	if err != nil {
		return result, err
	}
	result.UpdatedProfile = 1
	result.Actions = append(result.Actions, "verified native-history operation "+operation.OperationID+" for "+id)
	return result, nil
}

func nativeLocalFilesystem(path string) error {
	for {
		var stat syscall.Statfs_t
		err := syscall.Statfs(path, &stat)
		if errors.Is(err, os.ErrNotExist) && filepath.Dir(path) != path {
			path = filepath.Dir(path)
			continue
		}
		if err != nil {
			return err
		}
		// Initial support is limited to local Linux filesystems used by native CI.
		switch uint64(stat.Type) {
		case 0xef53, 0x58465342, 0x9123683e, 0x01021994, 0x794c7630:
			return nil // ext, XFS, Btrfs, tmpfs, overlay
		default:
			return fmt.Errorf("native-history maintenance requires a supported local filesystem; %s has type %#x", path, stat.Type)
		}
	}
}

func planNativeHistory(roots RecoveryRoots, projectID string, source *rollout) (FrozenSyncPlan, error) {
	required := map[string]*rollout{}
	for _, item := range source.Dependencies {
		required[item.RolloutID] = item
	}
	required[source.RolloutID] = source
	operations, err := planProfileImportTransaction(roots.ProjectRoot, roots.ProfileHome, []*rollout{source}, required)
	if err != nil {
		return FrozenSyncPlan{}, err
	}
	changes := map[string]FrozenFileChange{}
	for _, op := range operations {
		if op.Delete {
			return FrozenSyncPlan{}, errors.New("native-history import would retire a file; archive/delete transitions are unsupported")
		}
		changes[op.Path] = FrozenFileChange{Scope: "profile", Path: op.Path, Data: op.Data}
	}
	var rollouts []FrozenRollout
	for _, physical := range sortedIDs(required) {
		item := required[physical]
		if item.HistoryMode != "paginated" || item.Archived || item.Compressed {
			return FrozenSyncPlan{}, errors.New("unsupported native-history ancestor")
		}
		lines, err := materializeLines(item, roots.ProjectRoot)
		if err != nil {
			return FrozenSyncPlan{}, err
		}
		data, err := encodeRollout(lines, false)
		if err != nil {
			return FrozenSyncPlan{}, err
		}
		changes[item.Relative] = FrozenFileChange{Scope: "profile", Path: item.Relative, Data: data}
		rollouts = append(rollouts, FrozenRollout{RolloutID: physical, SessionID: item.ID, Path: item.Relative, EndByteOffset: uint64(len(data)), EndOrdinalExclusive: item.EndOrdinal, PrefixSHA256: checksumBytes(data), Data: data})
	}
	var files []FrozenFileChange
	for _, path := range sortedIDs(changes) {
		files = append(files, changes[path])
	}
	desired := registrationFor(source)
	return NewFrozenSyncPlan(roots, projectID, files, rollouts, map[string]*registration{source.ID: &desired})
}

type nativeConfigurationEvidence struct{}

func (nativeConfigurationEvidence) Fingerprint(_ context.Context, _, _ string) (string, error) {
	// Linux has no macOS managed preferences. Refuse environment configuration
	// routes beyond the inspected home; preflight checks system/project TOML layers.
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "CODEX_") && key != "CODEX_HOME" {
			return "", fmt.Errorf("unsupported native configuration environment: %s", key)
		}
	}
	return "linux-default-loader-6ff1e1ca3b30fc8b914720681fcf5fe62e95f856", nil
}

type stoppedNativeProcesses struct {
	home   string
	helper string
}

func (s stoppedNativeProcesses) CheckStopped(ctx context.Context) error {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return err
		}
		pid, err := strconv.Atoi(entry.Name())
		if err != nil || pid == os.Getpid() {
			continue
		}
		base := filepath.Join("/proc", entry.Name())
		info, err := os.Stat(base)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
			continue
		}
		processExe, linkErr := os.Readlink(filepath.Join(base, "exe"))
		if linkErr == nil && processExe == s.helper {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(base, "environ"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return fmt.Errorf("cannot establish stopped native writers for process %d: %w", pid, err)
		}
		for _, entry := range strings.Split(string(raw), "\x00") {
			key, value, _ := strings.Cut(entry, "=")
			if (key == "CODEX_HOME" || key == "CODEXM_ACTIVE_HOME") && value != "" {
				if !filepath.IsAbs(value) {
					cwd, err := os.Readlink(filepath.Join(base, "cwd"))
					if errors.Is(err, os.ErrNotExist) {
						continue
					}
					if err != nil {
						return err
					}
					value = filepath.Join(cwd, value)
				}
				matches, err := sameNativeProfile(s.home, value)
				if err != nil {
					return err
				}
				if matches {
					return fmt.Errorf("process %d still has this profile open; stop it before native-history maintenance", pid)
				}
			}
		}
		exe, err := os.Readlink(filepath.Join(base, "exe"))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		name := filepath.Base(strings.TrimSuffix(exe, " (deleted)"))
		if name == "codex" || strings.HasPrefix(name, "codex-") {
			return fmt.Errorf("native Codex process %d is still running; stop external native clients", pid)
		}
	}
	return nil
}
func sameNativeProfile(home, observed string) (bool, error) {
	expected, err := os.Stat(home)
	if err != nil {
		return false, err
	}
	actual, err := os.Stat(observed)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return os.SameFile(expected, actual), nil
}

func nativePreflightRequest(opts NativeHistoryOptions, root, helper string) historycompat.Request {
	environment := make([]string, 0, len(os.Environ())+1)
	for _, entry := range os.Environ() {
		if !strings.HasPrefix(entry, "CODEX_HOME=") {
			environment = append(environment, entry)
		}
	}
	environment = append(environment, "CODEX_HOME="+opts.ProfileHome)
	return historycompat.Request{StoppedWritersAcknowledged: opts.StoppedWriters, ProfileHome: opts.ProfileHome, ProjectRoot: root,
		Native:      historycompat.Artifact{Path: opts.NativeExecutable, Pin: historycompat.ArtifactPin{Version: historycompat.NativeVersion, SHA256: NativeHistoryBinarySHA256}},
		Helper:      historycompat.Artifact{Path: helper, Pin: historycompat.ArtifactPin{Version: historycompat.NativeVersion, SHA256: NativeHistoryHelperSHA256}},
		Environment: environment, Scanner: stoppedNativeProcesses{home: opts.ProfileHome, helper: helper}, Configuration: nativeConfigurationEvidence{}}
}

func validateNativeHistoryLaunch(home, project, native string) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	helper := filepath.Join(filepath.Dir(executable), "native_history_helper")
	request := nativePreflightRequest(NativeHistoryOptions{Options: Options{ProfileHome: home}, NativeExecutable: native, StoppedWriters: true}, project, helper)
	_, err = historycompat.Check(context.Background(), request)
	return err
}

type nativeHistoryBackend struct {
	helper string
	home   string
	gate   *RecoveryGate
	cmd    *exec.Cmd
	input  io.WriteCloser
	output *bufio.Scanner
	stderr strings.Builder
	owners []string
}

// Only the nativehistorytest build installs subprocess fault instrumentation.
var nativeHistoryTestChildSetup func(*exec.Cmd)
var nativeHistoryTestChildStarted func(int)

func (b *nativeHistoryBackend) Identity() string {
	return "native-history-v1:" + NativeHistoryHelperSHA256 + ":" + NativeHistoryBinarySHA256
}
func (b *nativeHistoryBackend) acquire(ctx context.Context, rollouts []FrozenRollout) error {
	owners := map[string]bool{}
	for _, r := range rollouts {
		owners[r.SessionID] = true
	}
	if b.cmd != nil {
		for owner := range owners {
			found := false
			for _, held := range b.owners {
				if held == owner {
					found = true
				}
			}
			if !found {
				return errors.New("native ownership set changed")
			}
		}
		return nil
	}
	if b.gate == nil || b.gate.file == nil {
		return errors.New("native helper requires the inherited recovery gate")
	}
	cmd := exec.CommandContext(ctx, b.helper)
	cmd.ExtraFiles = []*os.File{b.gate.file, b.gate.legacy}
	if b.gate.projectFile != nil {
		cmd.ExtraFiles = append(cmd.ExtraFiles, b.gate.projectFile)
	}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "CODEX_HOME=" + b.home}
	input, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		input.Close()
		return err
	}
	cmd.Stderr = &b.stderr
	if nativeHistoryTestChildSetup != nil {
		nativeHistoryTestChildSetup(cmd)
	}
	if err := cmd.Start(); err != nil {
		input.Close()
		return err
	}
	if nativeHistoryTestChildStarted != nil {
		nativeHistoryTestChildStarted(cmd.Process.Pid)
	}
	b.cmd, b.input, b.output = cmd, input, bufio.NewScanner(output)
	b.output.Buffer(make([]byte, 4096), 1<<20)
	b.owners = sortedIDs(owners)
	return b.command(ctx, map[string]any{"action": "acquire", "profile_home": b.home, "owner_ids": b.owners}, nil)
}
func (b *nativeHistoryBackend) command(ctx context.Context, request any, result any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if b.input == nil {
		return errors.New("native helper session is not acquired")
	}
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	if len(data) > 1<<20 {
		return errors.New("oversized native helper command")
	}
	if _, err := b.input.Write(append(data, '\n')); err != nil {
		return err
	}
	if !b.output.Scan() {
		return fmt.Errorf("native helper stopped before acknowledgment: %v", b.output.Err())
	}
	var reply struct {
		OK     bool            `json:"ok"`
		Result json.RawMessage `json:"result"`
		Error  string          `json:"error"`
	}
	if err := json.Unmarshal(b.output.Bytes(), &reply); err != nil {
		return err
	}
	if !reply.OK {
		return fmt.Errorf("native history: %s", reply.Error)
	}
	if result != nil {
		return json.Unmarshal(reply.Result, result)
	}
	return nil
}
func (b *nativeHistoryBackend) Prepare(ctx context.Context, r ProjectionRequest) (ProjectionArtifact, error) {
	if err := b.acquire(ctx, r.Rollouts); err != nil {
		return ProjectionArtifact{}, err
	}
	var reply struct {
		Path   string `json:"artifact_path"`
		SHA256 string `json:"artifact_sha256"`
	}
	command := map[string]any{"action": "prepare", "profile_home": r.ProfileHome, "work_dir": r.WorkDir, "rollouts": nativeRolloutRequests(r.Rollouts), "tips": r.Tips}
	if err := b.command(ctx, command, &reply); err != nil {
		return ProjectionArtifact{}, err
	}
	return ProjectionArtifact{Path: reply.Path, SHA256: reply.SHA256}, nil
}
func nativeRolloutRequests(rollouts []FrozenRollout) []map[string]any {
	var result []map[string]any
	for _, r := range rollouts {
		result = append(result, map[string]any{"rollout_id": r.RolloutID, "session_id": r.SessionID, "path": r.Path, "end_byte_offset": r.EndByteOffset, "end_ordinal_exclusive": r.EndOrdinalExclusive, "prefix_sha256": r.PrefixSHA256})
	}
	return result
}
func (b *nativeHistoryBackend) Publish(ctx context.Context, r ProjectionRequest, a ProjectionArtifact) error {
	return b.artifactCommand(ctx, "publish", r, a)
}
func (b *nativeHistoryBackend) Verify(ctx context.Context, r ProjectionRequest, a ProjectionArtifact) error {
	return b.artifactCommand(ctx, "verify", r, a)
}
func (b *nativeHistoryBackend) artifactCommand(ctx context.Context, action string, r ProjectionRequest, a ProjectionArtifact) error {
	if err := b.acquire(ctx, r.Rollouts); err != nil {
		return err
	}
	return b.command(ctx, map[string]any{"action": action, "profile_home": r.ProfileHome, "work_dir": r.WorkDir, "artifact_sha256": a.SHA256}, nil)
}
func (b *nativeHistoryBackend) Close() {
	if b.cmd == nil {
		return
	}
	b.input.Close()
	// EOF releases the helper's ownership guards. Bound cleanup even after error.
	done := make(chan error, 1)
	go func() { done <- b.cmd.Wait() }()
	select {
	case <-done:
	case <-time.After(35 * time.Second):
		b.cmd.Process.Kill()
		<-done
	}
	b.cmd = nil
}
