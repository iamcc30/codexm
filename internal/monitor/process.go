package monitor

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/iamcc30/codexm/internal/appserver"
	"github.com/iamcc30/codexm/internal/config"
	"github.com/shirou/gopsutil/v4/process"
)

func scanUnmanaged(cfg *config.Config, managed map[int]bool) []Task {
	return filterManagedProcesses(scanProcessTasks(cfg), managed)
}

func scanProcessTasks(cfg *config.Config) []Task {
	processes, err := process.Processes()
	if err != nil {
		return nil
	}
	homes := map[string]string{}
	for name, profile := range cfg.Profiles {
		home := filepath.Clean(profile.CodexHome)
		if runtime.GOOS == "windows" {
			home = strings.ToLower(home)
		}
		homes[home] = name
	}
	var tasks []Task
	for _, proc := range processes {
		pid := int(proc.Pid)
		name, err := proc.Name()
		if err != nil || !isCodexProcess(name) {
			continue
		}
		profile, managedRemote, ok := identifyProcess(proc, homes)
		if !ok || managedRemote {
			continue
		}
		if profile == "" {
			continue
		}
		cwd, _ := proc.Cwd()
		created, _ := proc.CreateTime()
		tasks = append(tasks, Task{
			ID:           "pid-" + strings.TrimSpace(formatPID(pid)),
			Profile:      profile,
			Project:      cwd,
			Title:        "Unmanaged Codex process",
			Status:       "unmanaged",
			LastActivity: time.UnixMilli(created),
			Managed:      false,
			PID:          pid,
		})
	}
	return tasks
}

func filterManagedProcesses(tasks []Task, managed map[int]bool) []Task {
	result := make([]Task, 0, len(tasks))
	for _, task := range tasks {
		if managed[task.PID] {
			continue
		}
		result = append(result, task)
	}
	return result
}

func identifyProcess(proc *process.Process, homes map[string]string) (string, bool, bool) {
	env, err := proc.Environ()
	if err == nil {
		return profileFromEnvironment(env, homes), managedRemoteProcess(env), true
	}
	if runtime.GOOS != "darwin" {
		return "", false, false
	}
	// gopsutil does not currently implement Environ on macOS. ps eww is the
	// least-privileged portable fallback. The raw output is never returned,
	// logged, cached, or exposed; only exact known CODEX_HOME and marker values
	// are extracted. Choosing the last match favors the environment suffix over
	// identical prompt text in argv.
	raw, commandErr := exec.Command("ps", "eww", "-p", formatPID(int(proc.Pid)), "-o", "command=").Output()
	if commandErr != nil {
		return "", false, false
	}
	profile, position, valueLength := "", -1, 0
	for home, name := range homes {
		if match := environmentAssignmentPosition(string(raw), "CODEX_HOME", home); match > position ||
			(match == position && match >= 0 && len(home) > valueLength) {
			profile, position, valueLength = name, match, len(home)
		}
	}
	managed := environmentAssignmentPosition(string(raw), appserver.ManagedRemoteEnv, "1") >= 0
	return profile, managed, true
}

func profileFromEnvironment(env []string, homes map[string]string) string {
	for _, item := range env {
		if !strings.HasPrefix(item, "CODEX_HOME=") {
			continue
		}
		home := filepath.Clean(strings.TrimPrefix(item, "CODEX_HOME="))
		if runtime.GOOS == "windows" {
			home = strings.ToLower(home)
		}
		return homes[home]
	}
	return ""
}

func environmentAssignmentPosition(raw, key, value string) int {
	needle := key + "=" + value
	offset := 0
	last := -1
	for {
		relative := strings.Index(raw[offset:], needle)
		if relative < 0 {
			return last
		}
		position := offset + relative
		end := position + len(needle)
		leftBoundary := position == 0 || raw[position-1] == ' ' || raw[position-1] == '\t'
		rightBoundary := end == len(raw) || raw[end] == ' ' || raw[end] == '\t' ||
			raw[end] == '\r' || raw[end] == '\n'
		if leftBoundary && rightBoundary {
			last = position
		}
		offset = position + 1
	}
}

func managedRemoteProcess(env []string) bool {
	for _, item := range env {
		if item == appserver.ManagedRemoteEnv+"=1" {
			return true
		}
	}
	return false
}

func isCodexProcess(name string) bool {
	name = strings.ToLower(filepath.Base(name))
	return name == "codex" || name == "codex.exe"
}

func formatPID(pid int) string {
	const digits = "0123456789"
	if pid == 0 {
		return "0"
	}
	var buf [24]byte
	i := len(buf)
	for pid > 0 {
		i--
		buf[i] = digits[pid%10]
		pid /= 10
	}
	return string(buf[i:])
}
