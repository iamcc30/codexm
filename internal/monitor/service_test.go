package monitor

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/iamcc30/codexm/internal/appserver"
	"github.com/iamcc30/codexm/internal/config"
	gprocess "github.com/shirou/gopsutil/v4/process"
)

func TestThreadStatusAndSubagentTreeHandleWaitingOrphansAndCycles(t *testing.T) {
	status := func(kind string, flags ...string) json.RawMessage {
		data, _ := json.Marshal(map[string]any{"type": kind, "activeFlags": flags})
		return data
	}
	if got := threadStatus(status("active", "waitingOnApproval")); got != "waiting_approval" {
		t.Fatalf("waiting approval status = %q", got)
	}
	if got := threadStatus(status("active", "waitingOnUserInput")); got != "waiting_input" {
		t.Fatalf("waiting input status = %q", got)
	}
	sessions := []Session{
		{ID: "root", Status: "active"},
		{ID: "child", ParentThreadID: "root", Status: "active", AgentNickname: "ada", AgentRole: "worker"},
		{ID: "orphan", ParentThreadID: "missing", Status: "error"},
		{ID: "cycle-a", ParentThreadID: "cycle-b"},
		{ID: "cycle-b", ParentThreadID: "cycle-a"},
	}
	nodes := buildSubagents(sessions)
	byID := map[string]Subagent{}
	for _, node := range nodes {
		byID[node.ID] = node
	}
	if byID["child"].TaskID != "root" || byID["child"].Nickname != "ada" || byID["child"].Role != "worker" {
		t.Fatalf("child tree metadata missing: %+v", byID["child"])
	}
	if !byID["orphan"].Orphan {
		t.Fatalf("orphan not detected: %+v", byID["orphan"])
	}
	if !byID["cycle-a"].Cycle || !byID["cycle-b"].Cycle {
		t.Fatalf("cycle not detected: %+v", byID)
	}
}

func TestMaskEmailAndUnknownProtocolFields(t *testing.T) {
	if got := maskEmail("alice@example.com"); got != "a***e@example.com" {
		t.Fatalf("masked email = %q", got)
	}
	var thread appserver.Thread
	if err := json.Unmarshal([]byte(`{"id":"1","cwd":"/tmp","futureField":{"secret":"ignored"}}`), &thread); err != nil {
		t.Fatal(err)
	}
	if thread.ID != "1" || thread.CWD != "/tmp" {
		t.Fatalf("known fields were not retained: %+v", thread)
	}
	thread.Preview = strings.Repeat("private prompt ", 100)
	sanitizeThreadMetadata(&thread)
	if len([]rune(thread.Preview)) > 160 || !strings.HasSuffix(thread.Preview, "…") {
		t.Fatalf("prompt preview was retained without truncation: %d runes", len([]rune(thread.Preview)))
	}
}

func TestLiveNotificationsUpdateModelTokensAndMCPHealth(t *testing.T) {
	runner := &ServiceRunner{
		cfg: config.New(), manager: &appserver.Manager{Home: t.TempDir()},
		store: NewStore(), data: map[string]profileData{
			"test": {
				account: Account{Profile: "test", MCPHealthy: true},
				threads: []appserver.Thread{{ID: "thread-1"}},
			},
		},
	}
	runner.applyNotification("test", appserver.Notification{
		Method: "thread/settings/updated",
		Params: json.RawMessage(`{"threadId":"thread-1","threadSettings":{"model":"gpt-5.6"}}`),
	})
	runner.applyNotification("test", appserver.Notification{
		Method: "thread/tokenUsage/updated",
		Params: json.RawMessage(`{"threadId":"thread-1","turnId":"turn-1","tokenUsage":{"total":{"totalTokens":456}}}`),
	})
	runner.applyNotification("test", appserver.Notification{
		Method: "mcpServer/startupStatus/updated",
		Params: json.RawMessage(`{"name":"docs","status":"failed","error":"connection refused"}`),
	})
	item := runner.data["test"]
	if item.threads[0].Model != "gpt-5.6" || item.tokens["thread-1"] != 456 {
		t.Fatalf("live model/token data was not retained: %+v tokens=%+v", item.threads[0], item.tokens)
	}
	if item.account.MCPHealthy || len(item.account.MCPServers) != 1 ||
		item.account.MCPServers[0].Error != "connection refused" {
		t.Fatalf("MCP failure notification was not retained: %+v", item.account)
	}
}

func TestBuildSnapshotKeepsProfilesQuotasProjectsArchivesAndTokensSeparate(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	empty := filepath.Join(root, "empty-binding")
	for _, path := range []string{nested, empty} {
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cfg := config.New()
	cfg.Profiles["one"] = config.Profile{CodexHome: t.TempDir()}
	cfg.Profiles["two"] = config.Profile{CodexHome: t.TempDir()}
	cfg.Bindings[root] = "one"
	cfg.Bindings[nested] = "two"
	cfg.Bindings[empty] = "one"
	normalizedRoot, _ := config.NormalizePath(root)
	normalizedNested, _ := config.NormalizePath(nested)
	normalizedEmpty, _ := config.NormalizePath(empty)
	primary := &appserver.RateLimitWindow{UsedPercent: 40}
	activeStatus := json.RawMessage(`{"type":"active","activeFlags":[]}`)
	idleStatus := json.RawMessage(`{"type":"idle"}`)
	data := map[string]profileData{
		"one": {
			account: Account{Profile: "one", LoggedIn: true, Email: "s***e@example.com", Primary: primary},
			threads: []appserver.Thread{{
				ID: "one-active", Title: "One", CWD: root, Status: activeStatus,
				CreatedAt: 1, UpdatedAt: 3, Source: json.RawMessage(`"cli"`),
			}},
			tokens:  map[string]int64{"one-active": 100},
			service: Service{Profile: "one", Healthy: true},
		},
		"two": {
			account: Account{Profile: "two", LoggedIn: true, Email: "s***e@example.com", Primary: primary},
			threads: []appserver.Thread{
				{
					ID: "two-archived", Title: "Archived", CWD: nested, Status: idleStatus,
					CreatedAt: 1, UpdatedAt: 2, Archived: true, Source: json.RawMessage(`"cli"`),
				},
				{
					ID: "two-child", ParentThreadID: "missing", AgentNickname: "ada",
					CWD: nested, Path: filepath.Join(nested, "rollout.jsonl"), Status: idleStatus,
				},
			},
			service: Service{Profile: "two", Healthy: true},
		},
	}
	snapshot := buildSnapshot(cfg, data, Filter{}, t.TempDir())
	if snapshot.Summary.Profiles != 2 || len(snapshot.Accounts) != 2 {
		t.Fatalf("profiles were deduplicated instead of remaining separate: %+v", snapshot.Summary)
	}
	for _, account := range snapshot.Accounts {
		if account.Primary == nil || account.Primary.UsedPercent != 40 {
			t.Fatalf("profile quota was lost or aggregated: %+v", account)
		}
	}
	projects := map[string]Project{}
	for _, project := range snapshot.Projects {
		projects[project.Root] = project
	}
	if projects[normalizedRoot].Profile != "one" || projects[normalizedRoot].Tokens != 100 ||
		projects[normalizedRoot].TokenSessions != 1 || projects[normalizedNested].Profile != "two" {
		t.Fatalf("nested project attribution/token totals are wrong: %+v", projects)
	}
	if _, ok := projects[normalizedEmpty]; !ok {
		t.Fatalf("explicit binding without sessions was omitted: %+v", projects)
	}
	archived := false
	for _, item := range snapshot.Sessions {
		archived = archived || item.ID == "two-archived" && item.Archived
	}
	if !archived {
		t.Fatalf("archived session was omitted: %+v", snapshot.Sessions)
	}
	if len(snapshot.Subagents) != 1 || !snapshot.Subagents[0].Orphan ||
		snapshot.Subagents[0].Path == "" {
		t.Fatalf("subagent orphan/path metadata is wrong: %+v", snapshot.Subagents)
	}
}

func TestServiceReconnectsAndIsolatesProfileFailures(t *testing.T) {
	var connections atomic.Int32
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/readyz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		connection := connections.Add(1)
		defer conn.Close()
		calls := 0
		for {
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params json.RawMessage `json:"params"`
			}
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			if len(request.ID) == 0 {
				continue
			}
			calls++
			result := mockRPCResult(request.Method, request.Params)
			if err := conn.WriteJSON(map[string]any{"id": request.ID, "result": result}); err != nil {
				return
			}
			if connection == 1 && calls == 7 {
				return
			}
		}
	}))
	defer server.Close()

	home := t.TempDir()
	tokenPath := filepath.Join(home, "runtime", "app-server", "healthy", "capability.token")
	if err := os.MkdirAll(filepath.Dir(tokenPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenPath, []byte("test-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	proc, err := gprocess.NewProcess(int32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	createdMillis, err := proc.CreateTime()
	if err != nil {
		t.Fatal(err)
	}
	state := appserver.State{
		Profile: "healthy", PID: os.Getpid(),
		Endpoint:  strings.Replace(server.URL, "http://", "ws://", 1),
		TokenFile: tokenPath, LogFile: filepath.Join(filepath.Dir(tokenPath), "app-server.log"),
		StartedAt: time.UnixMilli(createdMillis).UTC(),
	}
	stateBytes, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(tokenPath), "state.json"), stateBytes, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := config.New()
	cfg.Profiles["healthy"] = config.Profile{CodexHome: t.TempDir()}
	cfg.Profiles["broken"] = config.Profile{CodexHome: t.TempDir()}
	store := NewStore()
	manager := &appserver.Manager{Home: home}
	runner := NewService(cfg, manager, store, ServiceOptions{
		StatusInterval: 20 * time.Millisecond, ThreadInterval: 30 * time.Millisecond,
		AccountInterval: time.Hour,
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runner.Run(ctx)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("service did not stop after cancellation")
		}
	})

	eventually(t, 4*time.Second, func() bool {
		snapshot := store.Snapshot()
		return connections.Load() >= 2 &&
			accountByProfile(snapshot.Accounts, "healthy").LoggedIn &&
			!accountByProfile(snapshot.Accounts, "broken").CodexHealthy
	})
	snapshot := store.Snapshot()
	if len(snapshot.Sessions) != 1 || snapshot.Sessions[0].ID != "thread-1" {
		t.Fatalf("healthy profile data was lost after reconnect: %+v", snapshot.Sessions)
	}
	if got := accountByProfile(snapshot.Accounts, "healthy"); got.Email != "s***e@example.com" ||
		len(got.MCPServers) != 1 || !got.MCPHealthy {
		t.Fatalf("account/MCP data missing after reconnect: %+v", got)
	}
}

func mockRPCResult(method string, params json.RawMessage) any {
	switch method {
	case "account/read":
		return map[string]any{"account": map[string]any{
			"type": "chatgpt", "email": "same@example.com", "planType": "plus",
		}}
	case "account/rateLimits/read":
		return map[string]any{"rateLimits": map[string]any{}}
	case "account/usage/read":
		return map[string]any{"summary": map[string]any{"lifetimeTokens": 123}}
	case "mcpServerStatus/list":
		return map[string]any{"data": []map[string]any{{"name": "docs", "authStatus": "loggedIn"}}}
	case "thread/list":
		var value struct {
			Archived bool `json:"archived"`
		}
		_ = json.Unmarshal(params, &value)
		if value.Archived {
			return map[string]any{"data": []any{}}
		}
		return map[string]any{"data": []map[string]any{{
			"id": "thread-1", "name": "Reconnect test", "cwd": "/tmp/project",
			"path":      "/tmp/project/rollout.jsonl",
			"createdAt": 1, "updatedAt": 2,
			"status": map[string]any{"type": "idle"},
		}}}
	default:
		return map[string]any{}
	}
}

func accountByProfile(accounts []Account, profile string) Account {
	for _, account := range accounts {
		if account.Profile == profile {
			return account
		}
	}
	return Account{}
}

func eventually(t *testing.T, timeout time.Duration, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("condition was not met within %s", timeout)
}
