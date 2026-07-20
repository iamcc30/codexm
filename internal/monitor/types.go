package monitor

import (
	"sync"
	"time"

	"github.com/iamcc30/codexm/internal/appserver"
	"github.com/iamcc30/codexm/internal/session"
)

type Filter struct {
	Profile string
	Project string
}

type Snapshot struct {
	GeneratedAt time.Time  `json:"generated_at"`
	Locale      string     `json:"locale"`
	Summary     Summary    `json:"summary"`
	Accounts    []Account  `json:"accounts"`
	Projects    []Project  `json:"projects"`
	Sessions    []Session  `json:"sessions"`
	Tasks       []Task     `json:"tasks"`
	Subagents   []Subagent `json:"subagents"`
	Services    []Service  `json:"services"`
	Warnings    []string   `json:"warnings,omitempty"`
}

type Summary struct {
	Profiles        int `json:"profiles"`
	Projects        int `json:"projects"`
	Sessions        int `json:"sessions"`
	ActiveTasks     int `json:"active_tasks"`
	WaitingApproval int `json:"waiting_approval"`
	WaitingInput    int `json:"waiting_input"`
	Unmanaged       int `json:"unmanaged"`
	ServiceFailures int `json:"service_failures"`
	SyncConflicts   int `json:"sync_conflicts"`
}

type Account struct {
	Profile      string                     `json:"profile"`
	LoggedIn     bool                       `json:"logged_in"`
	Email        string                     `json:"email,omitempty"`
	Plan         string                     `json:"plan,omitempty"`
	Primary      *appserver.RateLimitWindow `json:"primary,omitempty"`
	Secondary    *appserver.RateLimitWindow `json:"secondary,omitempty"`
	Credits      *appserver.Credits         `json:"credits,omitempty"`
	Lifetime     *int64                     `json:"lifetime_tokens,omitempty"`
	Daily        []appserver.DailyUsage     `json:"daily_usage,omitempty"`
	CodexHealthy bool                       `json:"codex_healthy"`
	MCPHealthy   bool                       `json:"mcp_healthy"`
	MCPServers   []MCPServer                `json:"mcp_servers,omitempty"`
	Error        string                     `json:"error,omitempty"`
}

type MCPServer struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	AuthStatus string `json:"auth_status,omitempty"`
	Error      string `json:"error,omitempty"`
}

type Project struct {
	Root          string             `json:"root"`
	Profile       string             `json:"profile,omitempty"`
	BindingSource string             `json:"binding_source,omitempty"`
	GitRoot       string             `json:"git_root,omitempty"`
	GitBranch     string             `json:"git_branch,omitempty"`
	Sessions      int                `json:"sessions"`
	ActiveTasks   int                `json:"active_tasks"`
	Tokens        int64              `json:"tokens"`
	TokenSessions int                `json:"token_sessions"`
	Mirror        session.Inspection `json:"mirror"`
}

type Session struct {
	ID             string    `json:"id"`
	Title          string    `json:"title"`
	Preview        string    `json:"preview,omitempty"`
	Profile        string    `json:"profile"`
	Project        string    `json:"project"`
	Path           string    `json:"path,omitempty"`
	Model          string    `json:"model,omitempty"`
	Source         string    `json:"source,omitempty"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
	Tokens         int64     `json:"tokens"`
	TokenKnown     bool      `json:"token_known"`
	Archived       bool      `json:"archived"`
	GitBranch      string    `json:"git_branch,omitempty"`
	ParentThreadID string    `json:"parent_thread_id,omitempty"`
	Status         string    `json:"status"`
	AgentNickname  string    `json:"agent_nickname,omitempty"`
	AgentRole      string    `json:"agent_role,omitempty"`
}

type Task struct {
	ID           string    `json:"id"`
	Profile      string    `json:"profile"`
	Project      string    `json:"project,omitempty"`
	Title        string    `json:"title"`
	Status       string    `json:"status"`
	LastActivity time.Time `json:"last_activity"`
	Managed      bool      `json:"managed"`
	PID          int       `json:"pid,omitempty"`
}

type Subagent struct {
	ID       string `json:"id"`
	ParentID string `json:"parent_id,omitempty"`
	TaskID   string `json:"task_id,omitempty"`
	Profile  string `json:"profile"`
	Project  string `json:"project,omitempty"`
	Path     string `json:"path,omitempty"`
	Nickname string `json:"nickname,omitempty"`
	Role     string `json:"role,omitempty"`
	Status   string `json:"status"`
	Orphan   bool   `json:"orphan"`
	Cycle    bool   `json:"cycle"`
	Depth    int    `json:"depth"`
}

type Service struct {
	Profile  string    `json:"profile"`
	PID      int       `json:"pid,omitempty"`
	Endpoint string    `json:"endpoint,omitempty"`
	Version  string    `json:"version,omitempty"`
	Started  time.Time `json:"started_at,omitempty"`
	Healthy  bool      `json:"healthy"`
	Error    string    `json:"error,omitempty"`
}

type Store struct {
	mu      sync.RWMutex
	current Snapshot
	subs    map[chan Snapshot]struct{}
}

func NewStore() *Store { return &Store{subs: map[chan Snapshot]struct{}{}} }

func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.current
}

func (s *Store) Publish(snapshot Snapshot) {
	s.mu.Lock()
	s.current = snapshot
	for ch := range s.subs {
		select {
		case ch <- snapshot:
		default:
		}
	}
	s.mu.Unlock()
}

func (s *Store) Subscribe() (<-chan Snapshot, func()) {
	ch := make(chan Snapshot, 1)
	s.mu.Lock()
	s.subs[ch] = struct{}{}
	if !s.current.GeneratedAt.IsZero() {
		ch <- s.current
	}
	s.mu.Unlock()
	return ch, func() {
		s.mu.Lock()
		delete(s.subs, ch)
		close(ch)
		s.mu.Unlock()
	}
}
