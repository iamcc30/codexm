package monitor

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/iamcc30/codexm/internal/config"
)

func buildSnapshot(cfg *config.Config, data map[string]profileData, filter Filter, managerHome string) Snapshot {
	return buildSnapshotWithCache(cfg, data, filter, managerHome, newSnapshotCache())
}

func buildSnapshotWithCache(cfg *config.Config, data map[string]profileData, filter Filter, managerHome string, cache *snapshotCache) Snapshot {
	cache.prepare(cfg)
	snapshot := Snapshot{GeneratedAt: time.Now().UTC(), Locale: locale()}
	projects := map[string]*Project{}
	managedPIDs := map[int]bool{}
	for _, name := range config.SortedProfileNames(cfg) {
		if filter.Profile != "" && name != filter.Profile {
			continue
		}
		item := data[name]
		if item.account.Profile == "" {
			item.account.Profile = name
		}
		snapshot.Accounts = append(snapshot.Accounts, item.account)
		if item.service.Profile == "" {
			item.service.Profile = name
			item.service.Error = "not started"
		}
		if item.service.PID > 0 {
			managedPIDs[item.service.PID] = true
		}
		snapshot.Services = append(snapshot.Services, item.service)
		for _, raw := range item.threads {
			projectRoot, binding := cache.attributeProject(cfg, raw.CWD)
			if filter.Project != "" && !sameOrWithin(projectRoot, filter.Project) {
				continue
			}
			title := strings.TrimSpace(raw.Title)
			if title == "" {
				title = shortText(raw.Preview, 100)
			}
			if title == "" {
				title = raw.ID
			}
			status := threadStatus(raw.Status)
			token, tokenKnown := item.tokens[raw.ID]
			sess := Session{
				ID: raw.ID, Title: title, Preview: shortText(raw.Preview, 160),
				Profile: name, Project: projectRoot, Path: raw.Path, Model: raw.Model,
				Source: sourceName(raw.Source), CreatedAt: unixTime(raw.CreatedAt), UpdatedAt: unixTime(raw.UpdatedAt),
				Tokens: token, TokenKnown: tokenKnown, Archived: raw.Archived,
				ParentThreadID: raw.ParentThreadID, Status: status,
				AgentNickname: raw.AgentNickname, AgentRole: raw.AgentRole,
			}
			if raw.GitInfo != nil {
				sess.GitBranch = raw.GitInfo.Branch
			}
			snapshot.Sessions = append(snapshot.Sessions, sess)
			project := ensureProject(projects, projectRoot, name, binding)
			project.Sessions++
			if sess.TokenKnown {
				project.Tokens += sess.Tokens
				project.TokenSessions++
			}
			if raw.ParentThreadID == "" && (status == "active" || strings.HasPrefix(status, "waiting")) {
				project.ActiveTasks++
			}
			if raw.ParentThreadID == "" && !raw.Archived && status != "not_loaded" {
				snapshot.Tasks = append(snapshot.Tasks, Task{
					ID: raw.ID, Profile: name, Project: projectRoot, Title: title,
					Status: status, LastActivity: sess.UpdatedAt, Managed: true,
				})
			}
		}
	}
	for root, name := range cfg.Bindings {
		if normalized, err := config.NormalizePath(root); err == nil {
			root = normalized
		}
		if filter.Profile != "" && name != filter.Profile {
			continue
		}
		if filter.Project != "" && !sameOrWithin(root, filter.Project) {
			continue
		}
		ensureProject(projects, root, name, "binding")
	}
	for _, task := range cache.unmanaged(cfg, managedPIDs) {
		if filter.Profile != "" && task.Profile != filter.Profile {
			continue
		}
		task.Project, _ = cache.attributeProject(cfg, task.Project)
		if filter.Project != "" && !sameOrWithin(task.Project, filter.Project) {
			continue
		}
		snapshot.Tasks = append(snapshot.Tasks, task)
	}
	for _, project := range projects {
		profileHome := ""
		if profile, ok := cfg.Profiles[project.Profile]; ok {
			profileHome = profile.CodexHome
		}
		facts := cache.project(project.Root, profileHome, managerHome)
		project.Mirror = facts.mirror
		project.GitRoot, project.GitBranch = facts.gitRoot, facts.gitBranch
		snapshot.Projects = append(snapshot.Projects, *project)
	}
	sort.Slice(snapshot.Projects, func(i, j int) bool { return snapshot.Projects[i].Root < snapshot.Projects[j].Root })
	sort.Slice(snapshot.Sessions, func(i, j int) bool { return snapshot.Sessions[i].UpdatedAt.After(snapshot.Sessions[j].UpdatedAt) })
	sort.Slice(snapshot.Tasks, func(i, j int) bool { return snapshot.Tasks[i].LastActivity.After(snapshot.Tasks[j].LastActivity) })
	snapshot.Subagents = buildSubagents(snapshot.Sessions)
	snapshot.Summary = summarize(snapshot)
	addAvailabilityWarnings(&snapshot)
	return snapshot
}

func addAvailabilityWarnings(snapshot *Snapshot) {
	tokenWarning := "Historical per-thread token usage is unavailable from thread/list; totals appear after live token notifications."
	modelWarning := "Historical per-thread model is unavailable from thread/list; it appears after live settings or reroute notifications."
	if snapshot.Locale == "zh-CN" {
		tokenWarning = "thread/list 不提供历史 thread token 用量；实时 token 通知到达后才会显示总量。"
		modelWarning = "thread/list 不提供历史 thread 模型；实时 settings 或 reroute 通知到达后才会显示。"
	}
	for _, item := range snapshot.Sessions {
		if !item.TokenKnown {
			snapshot.Warnings = append(snapshot.Warnings, tokenWarning)
			break
		}
	}
	for _, item := range snapshot.Sessions {
		if item.Model == "" {
			snapshot.Warnings = append(snapshot.Warnings, modelWarning)
			break
		}
	}
}

func ensureProject(projects map[string]*Project, root, profile, source string) *Project {
	if root == "" {
		root = "(unknown)"
	}
	if item := projects[root]; item != nil {
		return item
	}
	item := &Project{Root: root, Profile: profile, BindingSource: source}
	projects[root] = item
	return item
}

func gitInfo(path string) (string, string) {
	if path == "" || path == "(unknown)" {
		return "", ""
	}
	rootBytes, err := exec.Command("git", "-C", path, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", ""
	}
	root := strings.TrimSpace(string(rootBytes))
	branchBytes, _ := exec.Command("git", "-C", path, "branch", "--show-current").Output()
	return root, strings.TrimSpace(string(branchBytes))
}

func threadStatus(raw json.RawMessage) string {
	var value struct {
		Type        string   `json:"type"`
		ActiveFlags []string `json:"activeFlags"`
	}
	if json.Unmarshal(raw, &value) != nil {
		return "unknown"
	}
	if value.Type == "active" {
		for _, flag := range value.ActiveFlags {
			switch flag {
			case "waitingOnApproval":
				return "waiting_approval"
			case "waitingOnUserInput":
				return "waiting_input"
			}
		}
		return "active"
	}
	switch value.Type {
	case "notLoaded":
		return "not_loaded"
	case "systemError":
		return "error"
	case "":
		return "unknown"
	default:
		return strings.ToLower(value.Type)
	}
}

func sourceName(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(raw, &object) != nil {
		return ""
	}
	for _, key := range []string{"subAgent", "custom"} {
		if value, ok := object[key]; ok {
			var nested string
			if json.Unmarshal(value, &nested) == nil {
				return key + ":" + nested
			}
			return key
		}
	}
	return ""
}

func buildSubagents(sessions []Session) []Subagent {
	byID := map[string]Session{}
	for _, item := range sessions {
		byID[item.ID] = item
	}
	var result []Subagent
	for _, item := range sessions {
		if item.ParentThreadID == "" {
			continue
		}
		node := Subagent{
			ID: item.ID, ParentID: item.ParentThreadID, Profile: item.Profile,
			Project: item.Project, Path: item.Path, Status: item.Status,
			Nickname: item.AgentNickname, Role: item.AgentRole,
		}
		seen := map[string]bool{item.ID: true}
		parent := item.ParentThreadID
		for parent != "" {
			if seen[parent] {
				node.Cycle = true
				break
			}
			seen[parent] = true
			node.Depth++
			ancestor, ok := byID[parent]
			if !ok {
				node.Orphan = true
				break
			}
			if ancestor.ParentThreadID == "" {
				node.TaskID = ancestor.ID
				break
			}
			parent = ancestor.ParentThreadID
		}
		result = append(result, node)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].TaskID == result[j].TaskID {
			return result[i].Depth < result[j].Depth
		}
		return result[i].TaskID < result[j].TaskID
	})
	return result
}

func summarize(snapshot Snapshot) Summary {
	out := Summary{Profiles: len(snapshot.Accounts), Projects: len(snapshot.Projects), Sessions: len(snapshot.Sessions)}
	for _, service := range snapshot.Services {
		if !service.Healthy {
			out.ServiceFailures++
		}
	}
	for _, task := range snapshot.Tasks {
		switch task.Status {
		case "active":
			out.ActiveTasks++
		case "waiting_approval":
			out.ActiveTasks++
			out.WaitingApproval++
		case "waiting_input":
			out.ActiveTasks++
			out.WaitingInput++
		case "unmanaged":
			out.Unmanaged++
		}
	}
	for _, project := range snapshot.Projects {
		out.SyncConflicts += len(project.Mirror.Pending.Conflicts)
	}
	return out
}

func maskEmail(email string) string {
	at := strings.IndexByte(email, '@')
	if at <= 0 {
		return ""
	}
	local, domain := email[:at], email[at+1:]
	if len(local) == 1 {
		local += "***"
	} else {
		local = local[:1] + "***" + local[len(local)-1:]
	}
	return local + "@" + domain
}

func mcpHealthy(servers []MCPServer) bool {
	for _, server := range servers {
		if server.Status == "failed" || server.Status == "cancelled" || server.Status == "authentication_required" || server.AuthStatus == "notLoggedIn" {
			return false
		}
	}
	return true
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}

func shortText(value string, max int) string {
	value = strings.Join(strings.Fields(value), " ")
	runes := []rune(value)
	if len(runes) <= max {
		return value
	}
	return string(runes[:max-1]) + "…"
}

func unixTime(value int64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return time.Unix(value, 0).UTC()
}

func sameOrWithin(path, root string) bool {
	if root == "" {
		return true
	}
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func locale() string {
	for _, name := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		value := strings.ToLower(os.Getenv(name))
		if strings.HasPrefix(value, "zh") {
			return "zh-CN"
		}
	}
	return "en"
}
