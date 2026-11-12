//go:build linux

package session

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const recoveryV2Directory = ".codexm-sync-v2"

// ProjectionBackend is trusted compiled code, not a program selected by a
// journal. Prepare may write only WorkDir. Publish must preserve unrelated
// partitions and use the pinned native schema and transaction rules. Verify must
// inspect actual target state, including schema, and cannot trust receipts.
// Prepare must also finish read-only target/schema/preimage checks before it
// returns. The artifact must bind the selected expected SQL preimages needed by
// Publish. The native-history backend supplies exact pinned SQLite and metadata
// verification; model backends are restricted to orchestration tests.
// HMAC protects local authority from portable/repository input and accidental
// corruption; it does not defend against an attacker with the same OS account.
// SIGKILL tests are not power-loss or filesystem-failure tests.
type ProjectionBackend interface {
	Identity() string
	Prepare(context.Context, ProjectionRequest) (ProjectionArtifact, error)
	Publish(context.Context, ProjectionRequest, ProjectionArtifact) error
	Verify(context.Context, ProjectionRequest, ProjectionArtifact) error
}

type ProjectionArtifact struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type ProjectionRequest struct {
	OperationID string
	ProfileHome string
	WorkDir     string
	InputsDir   string
	Rollouts    []FrozenRollout
	Tips        []FrozenNativeTip
	Gate        *RecoveryGate
}

// RecoveryGate's file description is inherited by helper children through Run.
// Closing the parent descriptor does not explicitly unlock it: the OS keeps the
// flock until the last inherited description closes, including after SIGKILL.
// This excludes cooperating callers only. Native Codex does not take this lock.
type RecoveryGate struct {
	file        *os.File
	profile     string
	projectFile *os.File
	project     string
	legacy      *os.File
}

func OpenRecoveryGate(profileHome string) (*RecoveryGate, error) {
	if !filepath.IsAbs(profileHome) || filepath.Clean(profileHome) != profileHome {
		return nil, errors.New("invalid configured profile root")
	}
	if err := frozenCheckPath(profileHome, false); err != nil {
		return nil, err
	}
	legacy, err := lockSessionLease(filepath.Join(profileHome, ".codexm-session-writer.lock"))
	if err != nil {
		return nil, err
	}
	success := false
	defer func() {
		if !success {
			legacy.close()
		}
	}()
	directory := filepath.Join(profileHome, recoveryV2Directory)
	if err := frozenPrivateDir(directory); err != nil {
		return nil, err
	}
	path := filepath.Join(directory, "gate.lock")
	if err := frozenCheckPath(path, true); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return nil, fmt.Errorf("profile recovery gate busy: %w", err)
	}
	success = true
	return &RecoveryGate{file: file, profile: profileHome, legacy: legacy.file()}, nil
}

func (gate *RecoveryGate) Close() error {
	if gate == nil || gate.file == nil {
		return nil
	}
	var projectErr error
	if gate.projectFile != nil {
		projectErr = gate.projectFile.Close()
		gate.projectFile = nil
	}
	err := gate.file.Close()
	gate.file = nil
	if gate.legacy != nil {
		if e := gate.legacy.Close(); err == nil {
			err = e
		}
		gate.legacy = nil
	}
	if err != nil {
		return err
	}
	return projectErr
}

// acquireProject uses the same shared-project namespace as the existing Lease.
// Acquisition is nonblocking, so profile-then-project acquisition cannot deadlock.
// This prototype is intentionally not callable under an already-held old Lease;
// integration must pass a common inherited gate rather than double-locking it.
func (gate *RecoveryGate) acquireProject(projectRoot string) error {
	if gate == nil || gate.file == nil {
		return errors.New("closed recovery gate")
	}
	if gate.projectFile != nil {
		if gate.project != projectRoot {
			return errors.New("recovery gate has another project")
		}
		return nil
	}
	path := filepath.Join(projectRoot, projectDirName, "session-writer.lock")
	if err := frozenCheckPath(path, true); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		file.Close()
		return fmt.Errorf("project recovery gate busy: %w", err)
	}
	gate.projectFile, gate.project = file, projectRoot
	return nil
}

// Run is the required child-launch path for a backend. Context cancellation
// kills and reaps the child; the caller must still Verify an uncertain outcome.
func (gate *RecoveryGate) Run(ctx context.Context, executable string, args []string, input []byte, environment []string) ([]byte, error) {
	if gate == nil || gate.file == nil {
		return nil, errors.New("closed recovery gate")
	}
	command := exec.CommandContext(ctx, executable, args...)
	command.ExtraFiles = []*os.File{gate.file}
	if gate.legacy != nil {
		command.ExtraFiles = append(command.ExtraFiles, gate.legacy)
	}
	if gate.projectFile != nil {
		command.ExtraFiles = append(command.ExtraFiles, gate.projectFile)
	}
	command.Stdin = strings.NewReader(string(input))
	command.Env = append([]string(nil), environment...)
	return command.CombinedOutput()
}

type frozenRecordBody struct {
	Instance string             `json:"instance"`
	Plan     FrozenSyncPlan     `json:"plan"`
	Backend  string             `json:"backend"`
	Artifact ProjectionArtifact `json:"artifact"`
}

type frozenRecord struct {
	OperationID string           `json:"operation_id"`
	Body        frozenRecordBody `json:"body"`
	MAC         string           `json:"mac"`
}

type frozenPending struct {
	Version     int    `json:"version"`
	OperationID string `json:"operation_id"`
	ManagerKey  string `json:"manager_key"`
	ProfileKey  string `json:"profile_key"`
	MAC         string `json:"mac"`
}

type FrozenRecoveryResult struct {
	OperationID             string
	Recovered               bool
	PublishOutcomeUncertain bool
}

// Test instrumentation is package-private and never set from user-controlled
// environment variables in the implementation. Tests install it in subprocesses.
var frozenRecoveryTestHook func(string)

func frozenHit(point string) {
	if frozenRecoveryTestHook != nil {
		frozenRecoveryTestHook(point)
	}
}

func frozenJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	return append(data, '\n'), err
}
func frozenMAC(key []byte, purpose string, data []byte) string {
	hash := hmac.New(sha256.New, key)
	hash.Write([]byte(purpose))
	hash.Write([]byte{0})
	hash.Write(data)
	return hex.EncodeToString(hash.Sum(nil))
}
func validFrozenMAC(key []byte, purpose string, data []byte, expected string) bool {
	actual, err := hex.DecodeString(frozenMAC(key, purpose, data))
	if err != nil {
		return false
	}
	decoded, err := hex.DecodeString(expected)
	return err == nil && hmac.Equal(actual, decoded)
}
func frozenAuthority(roots RecoveryRoots) string {
	return filepath.Join(roots.ManagerHome, "session-recovery-v2", pathKey(roots.ProfileHome))
}
func frozenPendingPath(roots RecoveryRoots) string {
	return filepath.Join(roots.ProfileHome, recoveryV2Directory, "pending.json")
}
func frozenOperationDir(roots RecoveryRoots, id string) string {
	return filepath.Join(frozenAuthority(roots), "operations", id)
}

// CommitFrozenSync writes a private authenticated intent before publishing any
// planned profile/project file, SQL projection, or registry change. It is not
// called by Sync. The caller must first enforce supported version/configuration
// and quiescence, which cannot be established by this cooperative gate alone.
func CommitFrozenSync(ctx context.Context, plan FrozenSyncPlan, backend ProjectionBackend) (FrozenRecoveryResult, error) {
	if backend == nil || backend.Identity() == "" {
		return FrozenRecoveryResult{}, errors.New("projection backend required")
	}
	if err := validateFrozenPlan(plan); err != nil {
		return FrozenRecoveryResult{}, err
	}
	gate, err := OpenRecoveryGate(plan.Roots.ProfileHome)
	if err != nil {
		return FrozenRecoveryResult{}, err
	}
	defer gate.Close()
	return commitFrozenSyncLocked(ctx, plan, backend, gate)
}

func commitFrozenSyncLocked(ctx context.Context, plan FrozenSyncPlan, backend ProjectionBackend, gate *RecoveryGate) (FrozenRecoveryResult, error) {
	if _, exists, err := frozenReadFile(frozenPendingPath(plan.Roots)); err != nil || exists {
		if err != nil {
			return FrozenRecoveryResult{}, err
		}
		return FrozenRecoveryResult{}, errors.New("pending frozen synchronization; recover before preparing another")
	}
	if err := gate.acquireProject(plan.Roots.ProjectRoot); err != nil {
		return FrozenRecoveryResult{}, err
	}
	if err := frozenCheckPreimages(plan, false); err != nil {
		return FrozenRecoveryResult{}, err
	}
	authority := frozenAuthority(plan.Roots)
	if err := frozenPrivateDir(authority); err != nil {
		return FrozenRecoveryResult{}, err
	}
	key, err := frozenAuthorityKey(authority, true)
	if err != nil {
		return FrozenRecoveryResult{}, err
	}
	prepareDir, err := os.MkdirTemp(authority, ".prepare-")
	if err != nil {
		return FrozenRecoveryResult{}, err
	}
	// An interrupted prepare has no public mutations or pending pointer. Its
	// private directory is inert; no automatic broad garbage deletion is used.
	if err := writeFrozenInputs(prepareDir, plan.Rollouts); err != nil {
		return FrozenRecoveryResult{}, err
	}
	beforePlan, _ := frozenJSON(plan)
	request := frozenProjectionRequest(plan, prepareDir, "", gate)
	artifact, err := backend.Prepare(ctx, request)
	if err != nil {
		return FrozenRecoveryResult{}, err
	}
	afterPlan, _ := frozenJSON(plan)
	if string(beforePlan) != string(afterPlan) {
		return FrozenRecoveryResult{}, errors.New("backend mutated frozen planning data")
	}
	if err := verifyFrozenInputs(prepareDir, plan.Rollouts); err != nil {
		return FrozenRecoveryResult{}, err
	}
	if err := verifyFrozenArtifact(prepareDir, artifact); err != nil {
		return FrozenRecoveryResult{}, err
	}
	if err := os.Chmod(filepath.Join(prepareDir, artifact.Path), 0400); err != nil {
		return FrozenRecoveryResult{}, err
	}
	if err := syncFrozenTree(prepareDir); err != nil {
		return FrozenRecoveryResult{}, err
	}
	nonce := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return FrozenRecoveryResult{}, err
	}
	body := frozenRecordBody{Instance: hex.EncodeToString(nonce), Plan: plan, Backend: backend.Identity(), Artifact: artifact}
	bodyData, err := frozenJSON(body)
	if err != nil {
		return FrozenRecoveryResult{}, err
	}
	id := checksumBytes(bodyData)
	record := frozenRecord{OperationID: id, Body: body, MAC: frozenMAC(key, "plan", bodyData)}
	recordData, err := frozenJSON(record)
	if err != nil {
		return FrozenRecoveryResult{}, err
	}
	// Base64 and duplicated bound prefixes can exceed the raw-file budget.
	if len(recordData) > frozenMaxBytes {
		return FrozenRecoveryResult{}, errors.New("encoded recovery intent exceeds the readable journal limit")
	}
	if err := frozenDurableWrite(filepath.Join(prepareDir, "plan.json"), recordData, true, authority, "plan"); err != nil {
		return FrozenRecoveryResult{}, err
	}
	operations := filepath.Join(authority, "operations")
	if err := frozenPrivateDir(operations); err != nil {
		return FrozenRecoveryResult{}, err
	}
	operationDir := frozenOperationDir(plan.Roots, id)
	if err := unix.Renameat2(unix.AT_FDCWD, prepareDir, unix.AT_FDCWD, operationDir, unix.RENAME_NOREPLACE); err != nil {
		return FrozenRecoveryResult{}, fmt.Errorf("freeze operation directory: %w", err)
	}
	if err := frozenSyncParents(operationDir, plan.Roots.ManagerHome); err != nil {
		return FrozenRecoveryResult{}, err
	}
	frozenHit("prepare:durable")
	pending := frozenPending{Version: frozenPlanVersion, OperationID: id, ManagerKey: pathKey(plan.Roots.ManagerHome), ProfileKey: pathKey(plan.Roots.ProfileHome)}
	unsigned, _ := frozenJSON(pending)
	pending.MAC = frozenMAC(key, "pending", unsigned)
	pendingData, _ := frozenJSON(pending)
	if err := frozenDurableWrite(frozenPendingPath(plan.Roots), pendingData, true, plan.Roots.ProfileHome, "pending"); err != nil {
		return FrozenRecoveryResult{}, err
	}
	return resumeFrozenSync(ctx, record, operationDir, gate, backend)
}

// RecoverFrozenSync needs no original plan, source tree, or surviving caller.
// It locates only fixed local authority paths using configured manager/profile
// roots. A pending pointer for another manager blocks rather than being ignored.
func RecoverFrozenSync(ctx context.Context, roots RecoveryRoots, backend ProjectionBackend) (FrozenRecoveryResult, error) {
	if err := validateRecoveryRoots(roots, false); err != nil {
		return FrozenRecoveryResult{}, err
	}
	if _, exists, err := frozenReadFile(frozenPendingPath(roots)); err != nil || !exists {
		return FrozenRecoveryResult{}, err
	}
	if backend == nil || backend.Identity() == "" {
		return FrozenRecoveryResult{}, errors.New("projection backend required for pending recovery")
	}
	gate, err := OpenRecoveryGate(roots.ProfileHome)
	if err != nil {
		return FrozenRecoveryResult{}, err
	}
	defer gate.Close()
	return recoverFrozenSyncLocked(ctx, roots, backend, gate)
}

func recoverFrozenSyncLocked(ctx context.Context, roots RecoveryRoots, backend ProjectionBackend, gate *RecoveryGate) (FrozenRecoveryResult, error) {
	record, directory, err := loadFrozenRecord(roots, backend)
	if err != nil {
		return FrozenRecoveryResult{}, err
	}
	if err := gate.acquireProject(record.Body.Plan.Roots.ProjectRoot); err != nil {
		return FrozenRecoveryResult{}, err
	}
	result, err := resumeFrozenSync(ctx, record, directory, gate, backend)
	result.Recovered = true
	return result, err
}

func loadFrozenRecord(roots RecoveryRoots, backend ProjectionBackend) (frozenRecord, string, error) {
	var record frozenRecord
	pendingBytes, exists, err := frozenReadFile(frozenPendingPath(roots))
	if err != nil || !exists {
		return record, "", errors.New("pending pointer disappeared")
	}
	var pending frozenPending
	if err := decodeFrozenJSON(pendingBytes, &pending); err != nil {
		return record, "", err
	}
	if pending.Version != frozenPlanVersion || !validChecksum(pending.OperationID) || pending.ManagerKey != pathKey(roots.ManagerHome) || pending.ProfileKey != pathKey(roots.ProfileHome) {
		return record, "", errors.New("untrusted pending scope/identity")
	}
	key, err := frozenAuthorityKey(frozenAuthority(roots), false)
	if err != nil {
		return record, "", err
	}
	mac := pending.MAC
	pending.MAC = ""
	unsigned, _ := frozenJSON(pending)
	if !validFrozenMAC(key, "pending", unsigned, mac) {
		return record, "", errors.New("pending authority authentication failed")
	}
	directory := frozenOperationDir(roots, pending.OperationID)
	path := filepath.Join(directory, "plan.json")
	if err := frozenCheckPath(path, false); err != nil {
		return record, "", err
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 4*frozenMaxBytes {
		return record, "", errors.New("invalid or oversized authenticated plan")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return record, "", err
	}
	if err := decodeFrozenJSON(data, &record); err != nil {
		return record, "", err
	}
	nonce, nonceErr := hex.DecodeString(record.Body.Instance)
	if nonceErr != nil || len(nonce) != 16 {
		return record, "", errors.New("invalid recovery operation instance")
	}
	body, _ := frozenJSON(record.Body)
	if record.OperationID != pending.OperationID || checksumBytes(body) != record.OperationID || !validFrozenMAC(key, "plan", body, record.MAC) {
		return record, "", errors.New("frozen plan authority/hash mismatch")
	}
	if record.Body.Plan.Roots.ManagerHome != roots.ManagerHome || record.Body.Plan.Roots.ProfileHome != roots.ProfileHome || (roots.ProjectRoot != "" && roots.ProjectRoot != record.Body.Plan.Roots.ProjectRoot) {
		return record, "", errors.New("frozen roots differ from configured roots")
	}
	if record.Body.Backend != backend.Identity() {
		return record, "", errors.New("projection backend identity changed")
	}
	if err := validateFrozenPlan(record.Body.Plan); err != nil {
		return record, "", err
	}
	if err := verifyFrozenInputs(directory, record.Body.Plan.Rollouts); err != nil {
		return record, "", err
	}
	if err := verifyFrozenArtifact(directory, record.Body.Artifact); err != nil {
		return record, "", err
	}
	return record, directory, nil
}

func resumeFrozenSync(ctx context.Context, record frozenRecord, directory string, gate *RecoveryGate, backend ProjectionBackend) (FrozenRecoveryResult, error) {
	result := FrozenRecoveryResult{OperationID: record.OperationID}
	plan := record.Body.Plan
	if err := validateFrozenPlan(plan); err != nil {
		return result, err
	}
	if err := verifyFrozenInputs(directory, plan.Rollouts); err != nil {
		return result, err
	}
	if err := verifyFrozenArtifact(directory, record.Body.Artifact); err != nil {
		return result, err
	}
	if err := frozenCheckPreimages(plan, true); err != nil {
		return result, err
	}
	for index, op := range plan.Files {
		target, err := frozenTarget(plan.Roots, op.Scope, op.Path)
		if err != nil {
			return result, err
		}
		root := plan.Roots.ProfileHome
		if op.Scope == "project" {
			root = plan.Roots.ProjectRoot
		}
		current, exists, err := frozenReadFile(target)
		if err != nil {
			return result, err
		}
		currentHash := ""
		if exists {
			currentHash = checksumBytes(current)
		}
		if currentHash != op.AfterSHA256 {
			if currentHash != op.BeforeSHA256 {
				return result, fmt.Errorf("file changed independently: %s", op.Path)
			}
			if op.Delete {
				if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
					return result, err
				}
				frozenHit(fmt.Sprintf("file:%d:removed", index))
				if err := frozenSyncParents(target, root); err != nil {
					return result, err
				}
			} else if err := frozenDurableWrite(target, op.Data, !exists, root, fmt.Sprintf("file:%d", index)); err != nil {
				return result, err
			}
		} else {
			if exists {
				if err := frozenSyncFile(target); err != nil {
					return result, err
				}
			}
			if err := frozenSyncParents(target, root); err != nil {
				return result, err
			}
		}
	}
	if err := frozenVerifyFiles(plan); err != nil {
		return result, err
	}
	frozenHit("files:verified")
	request := frozenProjectionRequest(plan, directory, record.OperationID, gate)
	if err := verifyFrozenArtifact(directory, record.Body.Artifact); err != nil {
		return result, err
	}
	// Always inspect first, including after parent death or lost acknowledgment.
	if err := backend.Verify(ctx, request, record.Body.Artifact); err != nil {
		frozenHit("sql:before-publish")
		publishErr := backend.Publish(ctx, request, record.Body.Artifact)
		result.PublishOutcomeUncertain = publishErr != nil
		frozenHit("sql:returned")
		if err := verifyFrozenArtifact(directory, record.Body.Artifact); err != nil {
			return result, err
		}
		if verifyErr := backend.Verify(ctx, request, record.Body.Artifact); verifyErr != nil {
			return result, fmt.Errorf("projection still unverified (publish outcome %v): %w", publishErr, verifyErr)
		}
	}
	frozenHit("sql:verified")
	if err := frozenVerifyFiles(plan); err != nil {
		return result, err
	}
	if err := applyFrozenRegistry(plan); err != nil {
		return result, err
	}
	frozenHit("registry:verified")
	if err := verifyFrozenInputs(directory, plan.Rollouts); err != nil {
		return result, err
	}
	if err := verifyFrozenArtifact(directory, record.Body.Artifact); err != nil {
		return result, err
	}
	if err := frozenVerifyFiles(plan); err != nil {
		return result, err
	}
	if err := verifyFrozenRegistry(plan); err != nil {
		return result, err
	}
	if err := backend.Verify(ctx, request, record.Body.Artifact); err != nil {
		return result, err
	}
	frozenHit("all:verified")
	complete := []byte(record.OperationID + "\n")
	completePath := filepath.Join(directory, "complete")
	if data, exists, err := frozenReadFile(completePath); err != nil {
		return result, err
	} else if exists {
		if string(data) != string(complete) {
			return result, errors.New("invalid completion evidence")
		}
		if err := frozenSyncFile(completePath); err != nil {
			return result, err
		}
		if err := frozenSyncParents(completePath, plan.Roots.ManagerHome); err != nil {
			return result, err
		}
	} else if err := frozenDurableWrite(completePath, complete, true, frozenAuthority(plan.Roots), "complete"); err != nil {
		return result, err
	}
	// A completion marker alone is never read as authority. Every recovery above
	// re-verifies all resources before it can clear the profile startup barrier.
	if err := os.Remove(frozenPendingPath(plan.Roots)); err != nil {
		return result, err
	}
	frozenHit("pending:removed")
	if err := frozenSyncParents(frozenPendingPath(plan.Roots), plan.Roots.ProfileHome); err != nil {
		return result, err
	}
	frozenHit("pending:cleared")
	return result, nil
}

func frozenCheckPreimages(plan FrozenSyncPlan, allowDesired bool) error {
	for _, op := range plan.Files {
		path, err := frozenTarget(plan.Roots, op.Scope, op.Path)
		if err != nil {
			return err
		}
		data, exists, err := frozenReadFile(path)
		if err != nil {
			return err
		}
		digest := ""
		if exists {
			digest = checksumBytes(data)
		}
		if digest != op.BeforeSHA256 && (!allowDesired || digest != op.AfterSHA256) {
			return fmt.Errorf("file preimage conflict: %s", op.Path)
		}
	}
	reg, err := readFrozenRegistry(plan.Roots, plan.ProjectID)
	if err != nil {
		return err
	}
	for _, entry := range plan.Registry {
		var actual *registration
		if value, exists := reg.Sessions[entry.SessionID]; exists {
			actual = &value
		}
		if !reflect.DeepEqual(actual, entry.Before) && (!allowDesired || !reflect.DeepEqual(actual, entry.After)) {
			return fmt.Errorf("selected registry conflict: %s", entry.SessionID)
		}
	}
	return nil
}
func frozenVerifyFiles(plan FrozenSyncPlan) error {
	for _, op := range plan.Files {
		path, err := frozenTarget(plan.Roots, op.Scope, op.Path)
		if err != nil {
			return err
		}
		data, exists, err := frozenReadFile(path)
		if err != nil {
			return err
		}
		digest := ""
		if exists {
			digest = checksumBytes(data)
		}
		if digest != op.AfterSHA256 {
			return fmt.Errorf("file verification failed: %s", op.Path)
		}
	}
	return nil
}
func applyFrozenRegistry(plan FrozenSyncPlan) error {
	reg, err := readFrozenRegistry(plan.Roots, plan.ProjectID)
	if err != nil {
		return err
	}
	for _, entry := range plan.Registry {
		var current *registration
		if value, exists := reg.Sessions[entry.SessionID]; exists {
			current = &value
		}
		if !reflect.DeepEqual(current, entry.Before) && !reflect.DeepEqual(current, entry.After) {
			return fmt.Errorf("selected registry compare-and-set conflict: %s", entry.SessionID)
		}
		if entry.After == nil {
			delete(reg.Sessions, entry.SessionID)
		} else {
			reg.Sessions[entry.SessionID] = *entry.After
		}
	}
	data, err := frozenJSON(reg)
	if err != nil {
		return err
	}
	path := frozenRegistryPath(plan.Roots, plan.ProjectID)
	_, exists, err := frozenReadFile(path)
	if err != nil {
		return err
	}
	if err := frozenDurableWrite(path, data, !exists, plan.Roots.ManagerHome, "registry"); err != nil {
		return err
	}
	return verifyFrozenRegistry(plan)
}
func verifyFrozenRegistry(plan FrozenSyncPlan) error {
	reg, err := readFrozenRegistry(plan.Roots, plan.ProjectID)
	if err != nil {
		return err
	}
	for _, entry := range plan.Registry {
		var current *registration
		if value, exists := reg.Sessions[entry.SessionID]; exists {
			current = &value
		}
		if !reflect.DeepEqual(current, entry.After) {
			return errors.New("selected registry verification failed")
		}
	}
	return nil
}

func frozenProjectionRequest(plan FrozenSyncPlan, directory, id string, gate *RecoveryGate) ProjectionRequest {
	rollouts := make([]FrozenRollout, len(plan.Rollouts))
	for i, rollout := range plan.Rollouts {
		rollout.Data = append([]byte(nil), rollout.Data...)
		rollouts[i] = rollout
	}
	return ProjectionRequest{OperationID: id, ProfileHome: plan.Roots.ProfileHome, WorkDir: directory,
		InputsDir: filepath.Join(directory, "inputs"), Rollouts: rollouts, Tips: append([]FrozenNativeTip(nil), plan.NativeTips...), Gate: gate}
}
func writeFrozenInputs(directory string, rollouts []FrozenRollout) error {
	inputs := filepath.Join(directory, "inputs")
	if err := frozenPrivateDir(inputs); err != nil {
		return err
	}
	for _, rollout := range rollouts {
		path := filepath.Join(inputs, filepath.Base(rollout.Path))
		if err := frozenDurableWrite(path, rollout.Data, true, directory, "input"); err != nil {
			return err
		}
		if err := os.Chmod(path, 0400); err != nil {
			return err
		}
		if err := frozenSyncFile(path); err != nil {
			return err
		}
	}
	return nil
}
func verifyFrozenInputs(directory string, rollouts []FrozenRollout) error {
	for _, rollout := range rollouts {
		path := filepath.Join(directory, "inputs", filepath.Base(rollout.Path))
		data, exists, err := frozenReadFile(path)
		if err != nil || !exists {
			return errors.New("frozen source snapshot is missing or unsafe")
		}
		info, err := os.Stat(path)
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0222 != 0 || checksumBytes(data) != rollout.PrefixSHA256 || uint64(len(data)) != rollout.EndByteOffset {
			return errors.New("frozen source snapshot changed")
		}
	}
	return nil
}
func verifyFrozenArtifact(directory string, artifact ProjectionArtifact) error {
	if (artifact.Path != "projection.sqlite" && artifact.Path != "native-artifact.json") || !validChecksum(artifact.SHA256) {
		return errors.New("unsupported projection artifact path/hash")
	}
	path := filepath.Join(directory, artifact.Path)
	data, exists, err := frozenReadFile(path)
	if err != nil || !exists {
		return errors.New("missing or unsafe projection artifact")
	}
	if checksumBytes(data) != artifact.SHA256 {
		return errors.New("projection artifact changed")
	}
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if _, exists, err := frozenReadFile(path + suffix); err != nil || exists {
			return errors.New("projection artifact must be closed without sidecars")
		}
	}
	return nil
}

func frozenAuthorityKey(authority string, create bool) ([]byte, error) {
	path := filepath.Join(authority, "authority.key")
	data, exists, err := frozenReadFile(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		if !create {
			return nil, errors.New("missing local recovery authority; do not trust repository journals")
		}
		data = make([]byte, 32)
		if _, err := io.ReadFull(rand.Reader, data); err != nil {
			return nil, err
		}
		if err := frozenDurableWrite(path, data, true, authority, "authority"); err != nil {
			return nil, err
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if len(data) != 32 || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return nil, errors.New("unsafe local recovery authority")
	}
	return data, nil
}
func frozenPrivateDir(path string) error {
	if err := frozenCheckPath(path, true); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm()&0077 != 0 || info.Sys().(*syscall.Stat_t).Uid != uint32(os.Geteuid()) {
		return errors.New("recovery authority directory is not private")
	}
	return nil
}
func frozenSyncFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	err = file.Sync()
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}
func frozenSyncParents(path, stop string) error {
	for directory := filepath.Dir(path); ; directory = filepath.Dir(directory) {
		if directory != stop && !strings.HasPrefix(directory, stop+string(filepath.Separator)) {
			return errors.New("fsync escaped trusted root")
		}
		file, err := os.Open(directory)
		if err != nil {
			return err
		}
		err = file.Sync()
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		if directory == stop {
			return nil
		}
	}
}
func syncFrozenTree(root string) error {
	return filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if err := frozenCheckPath(path, false); err != nil {
			return err
		}
		return frozenSyncFile(path)
	})
}
func frozenDurableWrite(path string, data []byte, createOnly bool, root, point string) error {
	if err := frozenCheckPath(path, true); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	if err := frozenCheckPath(filepath.Dir(path), false); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".codexm-frozen-*.tmp")
	if err != nil {
		return err
	}
	name := temp.Name()
	renamed := false
	defer func() {
		temp.Close()
		if !renamed {
			os.Remove(name)
		}
	}()
	if err := temp.Chmod(0600); err != nil {
		return err
	}
	split := len(data) / 2
	if _, err := temp.Write(data[:split]); err != nil {
		return err
	}
	frozenHit(point + ":partial")
	if _, err := temp.Write(data[split:]); err != nil {
		return err
	}
	frozenHit(point + ":written")
	if err := temp.Sync(); err != nil {
		return err
	}
	frozenHit(point + ":synced")
	if err := temp.Close(); err != nil {
		return err
	}
	if createOnly {
		err = unix.Renameat2(unix.AT_FDCWD, name, unix.AT_FDCWD, path, unix.RENAME_NOREPLACE)
	} else {
		err = os.Rename(name, path)
	}
	if err != nil {
		return err
	}
	renamed = true
	frozenHit(point + ":renamed")
	if err := frozenSyncParents(path, root); err != nil {
		return err
	}
	frozenHit(point + ":durable")
	return nil
}
