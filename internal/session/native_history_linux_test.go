package session

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestNativeLeaseSurvivesParentReleaseUntilChildExit(t *testing.T) {
	root, home := t.TempDir(), t.TempDir()
	lease, err := AcquireLease(root, home)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("/bin/sh", "-c", "echo ready; read release")
	cmd.ExtraFiles = lease.InheritedFiles()
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait(); lease.Release() }()
	if !bufio.NewScanner(output).Scan() {
		t.Fatal("child did not start")
	}
	lease.Release()
	if other, err := AcquireLease(root, home); err == nil {
		other.Release()
		t.Fatal("child lost inherited writer ownership")
	}
	input.Write([]byte("release\n"))
	input.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	other, err := AcquireLease(root, home)
	if err != nil {
		t.Fatal(err)
	}
	other.Release()
}

func TestNativeGateSharesLegacyLeaseAndPendingBarrier(t *testing.T) {
	f := newFrozenFixture(t)
	gate, err := OpenRecoveryGate(f.roots.ProfileHome)
	if err != nil {
		t.Fatal(err)
	}
	if lease, err := AcquireLease("", f.roots.ProfileHome); err == nil {
		lease.Release()
		t.Fatal("legacy launch bypassed native maintenance")
	}
	gate.Close()
	lease, err := AcquireLease("", f.roots.ProfileHome)
	if err != nil {
		t.Fatal(err)
	}
	if gate, err := OpenRecoveryGate(f.roots.ProfileHome); err == nil {
		gate.Close()
		t.Fatal("native maintenance bypassed legacy launch")
	}
	lease.Release()
}

func TestNativePlanBindsCompleteAncestryAndRejectsRetirement(t *testing.T) {
	f := newHistoryFixture(t)
	original := historyLines(t, testSessionA, f.root, nil, "retained")
	historyWrite(t, f.home, testSessionA, testSessionA, original, false, false)
	f.init(t)
	previous, err := scanProfile(f.home, f.root)
	if err != nil {
		t.Fatal(err)
	}
	reverted := historyLines(t, testSessionA, f.root, historyBase(testSessionA, original, 2))
	historyWrite(t, f.home, testSessionA, historyC, reverted, false, false)
	f.sync(t, f.home)
	projects, err := scanProject(f.root)
	if err != nil {
		t.Fatal(err)
	}
	_, project, err := FindProject(f.root)
	if err != nil {
		t.Fatal(err)
	}
	roots := RecoveryRoots{ManagerHome: f.manager, ProfileHome: f.home, ProjectRoot: f.root}
	plan, err := planNativeHistory(roots, project.ProjectID, projects.Sessions[testSessionA])
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Rollouts) != 2 {
		t.Fatal("incomplete physical closure", len(plan.Rollouts))
	}
	plan.NativeTips = []FrozenNativeTip{{ThreadID: testSessionA, ExpectedPath: previous[testSessionA].Relative, NewPath: projects.Sessions[testSessionA].Relative}}
	if err := validateFrozenPlan(plan); err != nil {
		t.Fatal(err)
	}
	plan.NativeTips[0].NewPath = "sessions/noncanonical.jsonl"
	if err := validateFrozenPlan(plan); err == nil {
		t.Fatal("unbound selected tip accepted")
	}
}

func TestNativeMaintenanceRejectsMissingAcknowledgmentWithoutMutation(t *testing.T) {
	root, home, manager := t.TempDir(), t.TempDir(), t.TempDir()
	_, err := SyncNativeHistory(NativeHistoryOptions{Options: Options{ProjectRoot: root, ProfileHome: home, ManagerHome: manager}})
	if err == nil {
		t.Fatal("stopped-writer prerequisite skipped")
	}
	entries, _ := os.ReadDir(home)
	if len(entries) != 0 {
		t.Fatal("failed precondition changed profile")
	}

}

func TestNativeStoppedWriterDetectionResolvesProfileAliases(t *testing.T) {
	home := t.TempDir()
	alias := filepath.Join(t.TempDir(), "profile-alias")
	if err := os.Symlink(home, alias); err != nil {
		t.Fatal(err)
	}
	if matches, err := sameNativeProfile(home, alias); err != nil || !matches {
		t.Fatal(matches, err)
	}
	if matches, err := sameNativeProfile(home, t.TempDir()); err != nil || matches {
		t.Fatal(matches, err)
	}
}
