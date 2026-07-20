package appserver

import (
	"encoding/json"
	"time"
)

const (
	RemoteTokenEnv   = "CODEXM_APP_SERVER_TOKEN"
	ManagedRemoteEnv = "CODEXM_MANAGED_REMOTE"
)

type State struct {
	Profile      string    `json:"profile"`
	PID          int       `json:"pid"`
	Endpoint     string    `json:"endpoint"`
	TokenFile    string    `json:"token_file"`
	LogFile      string    `json:"log_file"`
	CodexVersion string    `json:"codex_version,omitempty"`
	StartedAt    time.Time `json:"started_at"`
}

type Health struct {
	State
	Running bool   `json:"running"`
	Healthy bool   `json:"healthy"`
	Error   string `json:"error,omitempty"`
}

type Notification struct {
	Method string
	Params json.RawMessage
}

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string { return e.Message }

type Account struct {
	Type     string `json:"type"`
	Email    string `json:"email,omitempty"`
	PlanType string `json:"planType,omitempty"`
}

type AccountResponse struct {
	Account            *Account `json:"account"`
	RequiresOpenAIAuth bool     `json:"requiresOpenaiAuth"`
}

type RateLimitWindow struct {
	UsedPercent        int    `json:"usedPercent"`
	ResetsAt           *int64 `json:"resetsAt"`
	WindowDurationMins *int64 `json:"windowDurationMins"`
}

type Credits struct {
	HasCredits bool   `json:"hasCredits"`
	Unlimited  bool   `json:"unlimited"`
	Balance    string `json:"balance"`
}

type RateLimitSnapshot struct {
	LimitID   string           `json:"limitId"`
	LimitName string           `json:"limitName"`
	PlanType  string           `json:"planType"`
	Primary   *RateLimitWindow `json:"primary"`
	Secondary *RateLimitWindow `json:"secondary"`
	Credits   *Credits         `json:"credits"`
}

type RateLimitsResponse struct {
	RateLimits          RateLimitSnapshot            `json:"rateLimits"`
	RateLimitsByLimitID map[string]RateLimitSnapshot `json:"rateLimitsByLimitId"`
}

type UsageSummary struct {
	LifetimeTokens        *int64 `json:"lifetimeTokens"`
	PeakDailyTokens       *int64 `json:"peakDailyTokens"`
	CurrentStreakDays     *int64 `json:"currentStreakDays"`
	LongestStreakDays     *int64 `json:"longestStreakDays"`
	LongestRunningTurnSec *int64 `json:"longestRunningTurnSec"`
}

type DailyUsage struct {
	StartDate string `json:"startDate"`
	Tokens    int64  `json:"tokens"`
}

type UsageResponse struct {
	Summary           UsageSummary `json:"summary"`
	DailyUsageBuckets []DailyUsage `json:"dailyUsageBuckets"`
}

type MCPServerStatus struct {
	Name       string `json:"name"`
	AuthStatus string `json:"authStatus"`
}

type MCPServerListResponse struct {
	Data       []MCPServerStatus `json:"data"`
	NextCursor *string           `json:"nextCursor"`
}

// Thread intentionally contains metadata only. It does not deserialize turns,
// items, transcript content, tool output, or command output.
type Thread struct {
	ID             string          `json:"id"`
	Preview        string          `json:"preview"`
	Title          string          `json:"name"`
	ModelProvider  string          `json:"modelProvider"`
	Model          string          `json:"model"`
	CWD            string          `json:"cwd"`
	Path           string          `json:"path"`
	CreatedAt      int64           `json:"createdAt"`
	UpdatedAt      int64           `json:"updatedAt"`
	Source         json.RawMessage `json:"source"`
	ParentThreadID string          `json:"parentThreadId"`
	GitInfo        *GitInfo        `json:"gitInfo"`
	Status         json.RawMessage `json:"status"`
	Archived       bool            `json:"archived"`
	AgentNickname  string          `json:"agentNickname"`
	AgentRole      string          `json:"agentRole"`
}

type GitInfo struct {
	SHA        string `json:"sha"`
	Branch     string `json:"branch"`
	Repository string `json:"repositoryUrl"`
}

type ThreadListResponse struct {
	Data       []Thread `json:"data"`
	NextCursor *string  `json:"nextCursor"`
}

type LoadedThread struct {
	ThreadID string          `json:"threadId"`
	Status   json.RawMessage `json:"status"`
}

type LoadedThreadListResponse struct {
	Data       []string `json:"data"`
	NextCursor *string  `json:"nextCursor"`
}
