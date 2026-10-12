package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/iamcc30/codexm/internal/config"
)

const (
	historyCLISourceID = "11111111-1111-4111-8111-111111111111"
	historyCLIChildID  = "22222222-2222-4222-8222-222222222222"
	historyCLIThirdID  = "44444444-4444-4444-8444-444444444444"
)

type historyCLI struct {
	t        *testing.T
	app      *App
	root     string
	profiles map[string]string
	out      bytes.Buffer
	errOut   bytes.Buffer
}

func newHistoryCLI(t *testing.T) *historyCLI {
	t.Helper()
	base := t.TempDir()
	root := filepath.Join(base, "project")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	root, err := config.NormalizePath(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEXM_HOME", filepath.Join(base, "manager"))
	t.Setenv("CODEXM_PROFILES_HOME", filepath.Join(base, "profiles"))
	t.Setenv("CODEX_HOME", filepath.Join(base, "unused-native-home"))
	h := &historyCLI{t: t, app: New("test"), root: root, profiles: map[string]string{}}
	h.app.Out, h.app.Err = &h.out, &h.errOut
	for _, name := range []string{"first", "second"} {
		h.succeed("add", name)
	}
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	for name, profile := range cfg.Profiles {
		h.profiles[name] = profile.CodexHome
	}
	return h
}

func (h *historyCLI) run(args ...string) int {
	h.out.Reset()
	h.errOut.Reset()
	return h.app.Run(args)
}

func (h *historyCLI) succeed(args ...string) {
	h.t.Helper()
	if code := h.run(args...); code != 0 {
		h.t.Fatalf("%q exit=%d stdout=%s stderr=%s", args, code, h.out.String(), h.errOut.String())
	}
}

func historyCLIRecord(t *testing.T, ordinal uint64, kind string, payload any) []byte {
	t.Helper()
	data, err := json.Marshal(map[string]any{"ordinal": ordinal, "type": kind, "payload": payload})
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

func historyCLIMeta(id, cwd string, base map[string]any) map[string]any {
	payload := map[string]any{"id": id, "cwd": cwd, "timestamp": "2026-01-02T03:04:05Z", "history_mode": "paginated"}
	if base != nil {
		payload["history_base"] = base
	}
	return payload
}

func historyCLIMessage(t *testing.T, ordinal uint64, text string) []byte {
	t.Helper()
	return historyCLIRecord(t, ordinal, "response_item", map[string]any{
		"type": "message", "role": "user",
		"content": []any{map[string]any{"type": "input_text", "text": text}},
	})
}

func historyCLIWrite(t *testing.T, home, name string, data []byte) string {
	t.Helper()
	relative := filepath.Join("sessions", "2026", "01", "02", name)
	path := filepath.Join(home, relative)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return relative
}

func historyCLIRequireBytes(t *testing.T, path string, expected []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, expected) {
		t.Fatalf("rollout bytes changed at %s", path)
	}
}

func TestSessionCLICopiesCrossProjectHistoryDependency(t *testing.T) {
	h := newHistoryCLI(t)
	other := filepath.Join(filepath.Dir(h.root), "other-project")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	source := historyCLIRecord(t, 0, "session_meta", historyCLIMeta(historyCLISourceID, other, nil))
	source = append(source, historyCLIMessage(t, 1, "synthetic inherited context")...)
	sourceRel := historyCLIWrite(t, h.profiles["first"], "rollout-2026-01-02T03-04-05-"+historyCLISourceID+".jsonl", source)
	base := map[string]any{"thread_id": historyCLISourceID, "end_ordinal_exclusive": 2, "end_byte_offset": len(source)}
	child := historyCLIRecord(t, 2, "session_meta", historyCLIMeta(historyCLIChildID, h.root, base))
	child = append(child, historyCLIMessage(t, 3, "synthetic child turn")...)
	childRel := historyCLIWrite(t, h.profiles["first"], "rollout-2026-01-03T03-04-05-"+historyCLIChildID+".jsonl", child)
	h.succeed("session", "init", "--project", h.root, "--profile", "first", "--import-existing")
	h.succeed("session", "sync", "--project", h.root, "--profile", "second")
	historyCLIRequireBytes(t, filepath.Join(h.profiles["second"], sourceRel), source)
	historyCLIRequireBytes(t, filepath.Join(h.profiles["second"], childRel), child)
	h.succeed("session", "status", "--project", h.root, "--profile", "second")
	h.succeed("session", "sync", "--project", h.root, "--profile", "second")
	historyCLIRequireBytes(t, filepath.Join(h.profiles["first"], sourceRel), source)
}

func TestSessionCLIContinuesRevertedThreadAcrossProfiles(t *testing.T) {
	h := newHistoryCLI(t)
	source := historyCLIRecord(t, 0, "session_meta", historyCLIMeta(historyCLISourceID, h.root, nil))
	source = append(source, historyCLIMessage(t, 1, "synthetic original turn")...)
	// A real revert may discard a later source turn. It is not a plain JSONL
	// append, and the first profile must retain the source's original tail.
	fullSource := append(append([]byte(nil), source...), historyCLIMessage(t, 2, "turn discarded by revert")...)
	sourceRel := historyCLIWrite(t, h.profiles["first"], "rollout-2026-01-02T03-04-05-"+historyCLISourceID+".jsonl", fullSource)
	h.succeed("session", "init", "--project", h.root, "--profile", "first", "--import-existing")
	base := map[string]any{"thread_id": historyCLISourceID, "end_ordinal_exclusive": 2, "end_byte_offset": len(source)}
	child := historyCLIRecord(t, 2, "session_meta", historyCLIMeta(historyCLISourceID, h.root, base))
	child = append(child, historyCLIMessage(t, 3, "synthetic reverted continuation")...)
	childRel := historyCLIWrite(t, h.profiles["first"], "rollout-2026-01-03T03-04-05-"+historyCLISourceID+"_"+historyCLIChildID+".jsonl", child)
	h.succeed("session", "sync", "--project", h.root, "--profile", "first")
	h.succeed("session", "sync", "--project", h.root, "--profile", "second")
	continued := append(append([]byte(nil), child...), historyCLIMessage(t, 4, "continued using second profile")...)
	historyCLIWrite(t, h.profiles["second"], filepath.Base(childRel), continued)
	h.succeed("session", "sync", "--project", h.root, "--profile", "second")
	h.succeed("session", "sync", "--project", h.root, "--profile", "first")
	historyCLIRequireBytes(t, filepath.Join(h.profiles["first"], sourceRel), fullSource)
	historyCLIRequireBytes(t, filepath.Join(h.profiles["second"], sourceRel), source)
	for _, profile := range h.profiles {
		historyCLIRequireBytes(t, filepath.Join(profile, childRel), continued)
	}
}

func TestSessionCLIMissingHistoryBlocksStatusSyncAndBothRunModes(t *testing.T) {
	h := newHistoryCLI(t)
	h.succeed("session", "init", "--project", h.root, "--profile", "first")
	base := map[string]any{"thread_id": historyCLISourceID, "end_ordinal_exclusive": 2, "end_byte_offset": 100}
	child := historyCLIRecord(t, 2, "session_meta", historyCLIMeta(historyCLIChildID, h.root, base))
	child = append(child, historyCLIMessage(t, 3, "synthetic incomplete child")...)
	historyCLIWrite(t, h.profiles["first"], "rollout-2026-01-03T03-04-05-"+historyCLIChildID+".jsonl", child)
	for _, command := range []string{"status", "sync"} {
		if code := h.run("session", command, "--project", h.root, "--profile", "first"); code == 0 || !strings.Contains(h.errOut.String(), "missing") {
			t.Fatalf("%s accepted incomplete history: exit=%d stderr=%s", command, code, h.errOut.String())
		}
	}
	if runtime.GOOS == "windows" {
		return // The remaining launch marker is a Unix shell fixture.
	}
	marker := filepath.Join(t.TempDir(), "launched")
	fake := filepath.Join(t.TempDir(), "fake-codex")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf invoked > \"$CODEXM_HISTORY_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEXM_CODEX_BIN", fake)
	t.Setenv("CODEXM_HISTORY_MARKER", marker)
	for _, unmanaged := range []bool{false, true} {
		args := []string{"run", "--project", h.root}
		if unmanaged {
			args = append(args, "--unmanaged")
		}
		args = append(args, "first", "--", "resume", "--last")
		if code := h.run(args...); code == 0 || !strings.Contains(h.errOut.String(), "missing") {
			t.Fatalf("run unmanaged=%t accepted missing history: exit=%d stderr=%s", unmanaged, code, h.errOut.String())
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("native command launched with incomplete history: %v", err)
		}
	}
}

func TestSessionCLIUnrelatedFutureHistoryDoesNotBlockSelectedProject(t *testing.T) {
	h := newHistoryCLI(t)
	source := historyCLIRecord(t, 0, "session_meta", historyCLIMeta(historyCLISourceID, h.root, nil))
	source = append(source, historyCLIMessage(t, 1, "selected project")...)
	historyCLIWrite(t, h.profiles["first"], "rollout-2026-01-02T03-04-05-"+historyCLISourceID+".jsonl", source)
	other := filepath.Join(filepath.Dir(h.root), "unrelated")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := historyCLIMeta(historyCLIChildID, other, nil)
	meta["history_mode"] = "future-unsupported-mode"
	unknown := historyCLIRecord(t, 0, "session_meta", meta)
	unknown = append(unknown, []byte("this unrelated body must not be parsed\n")...)
	rel := historyCLIWrite(t, h.profiles["first"], "rollout-future-layout.jsonl", unknown)
	h.succeed("session", "init", "--project", h.root, "--profile", "first", "--import-existing")
	h.succeed("session", "status", "--project", h.root, "--profile", "first")
	historyCLIRequireBytes(t, filepath.Join(h.profiles["first"], rel), unknown)
}

func TestSessionCLIPlansSharedSourceAtLongestRequiredPrefix(t *testing.T) {
	h := newHistoryCLI(t)
	other := filepath.Join(filepath.Dir(h.root), "source-project")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	short := historyCLIRecord(t, 0, "session_meta", historyCLIMeta(historyCLISourceID, other, nil))
	short = append(short, historyCLIMessage(t, 1, "first inherited turn")...)
	long := append(append([]byte(nil), short...), historyCLIMessage(t, 2, "second inherited turn")...)
	sourceRel := historyCLIWrite(t, h.profiles["first"], "rollout-2026-01-02T03-04-05-"+historyCLISourceID+".jsonl", long)
	for index, id := range []string{historyCLIChildID, historyCLIThirdID} {
		prefix := short
		ordinal := uint64(2)
		if index == 1 {
			prefix, ordinal = long, 3
		}
		base := map[string]any{"thread_id": historyCLISourceID, "end_ordinal_exclusive": ordinal, "end_byte_offset": len(prefix)}
		child := historyCLIRecord(t, ordinal, "session_meta", historyCLIMeta(id, h.root, base))
		child = append(child, historyCLIMessage(t, ordinal+1, "independent child")...)
		historyCLIWrite(t, h.profiles["first"], "rollout-2026-01-03T03-04-05-"+id+".jsonl", child)
	}
	h.succeed("session", "init", "--project", h.root, "--profile", "first", "--import-existing")
	h.succeed("session", "sync", "--project", h.root, "--profile", "second")
	historyCLIRequireBytes(t, filepath.Join(h.profiles["second"], sourceRel), long)
	h.succeed("session", "status", "--project", h.root, "--profile", "second")
}

func TestSessionCLIResolveCannotRewriteAnotherThreadsInheritedPrefix(t *testing.T) {
	h := newHistoryCLI(t)
	header := historyCLIRecord(t, 0, "session_meta", historyCLIMeta(historyCLISourceID, h.root, nil))
	original := append(append([]byte(nil), header...), historyCLIMessage(t, 1, "project copy")...)
	name := "rollout-2026-01-02T03-04-05-" + historyCLISourceID + ".jsonl"
	sourceRel := historyCLIWrite(t, h.profiles["first"], name, original)
	h.succeed("session", "init", "--project", h.root, "--profile", "first", "--import-existing")
	profile := append(append([]byte(nil), header...), historyCLIMessage(t, 1, "independent profile copy")...)
	historyCLIWrite(t, h.profiles["first"], name, profile)
	other := filepath.Join(filepath.Dir(h.root), "unrelated-dependent-project")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	base := map[string]any{"thread_id": historyCLISourceID, "end_ordinal_exclusive": 2, "end_byte_offset": len(profile)}
	child := historyCLIRecord(t, 2, "session_meta", historyCLIMeta(historyCLIChildID, other, base))
	child = append(child, historyCLIMessage(t, 3, "child depends on profile bytes")...)
	childRel := historyCLIWrite(t, h.profiles["first"], "rollout-2026-01-03T03-04-05-"+historyCLIChildID+".jsonl", child)
	if code := h.run("session", "resolve", "--project", h.root, "--profile", "first", "--use", "project", historyCLISourceID); code == 0 {
		t.Fatalf("resolution overwrote a referenced source: stdout=%s", h.out.String())
	}
	historyCLIRequireBytes(t, filepath.Join(h.profiles["first"], sourceRel), profile)
	historyCLIRequireBytes(t, filepath.Join(h.profiles["first"], childRel), child)
}
