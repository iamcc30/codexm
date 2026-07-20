package monitor

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/iamcc30/codexm/internal/appserver"
	"github.com/iamcc30/codexm/internal/config"
)

type ServiceOptions struct {
	Filter          Filter
	StartDaemons    bool
	StatusInterval  time.Duration
	ThreadInterval  time.Duration
	AccountInterval time.Duration
}

type profileData struct {
	account   Account
	threads   []appserver.Thread
	tokens    map[string]int64
	service   Service
	updatedAt time.Time
}

type ServiceRunner struct {
	cfg     *config.Config
	manager *appserver.Manager
	store   *Store
	opts    ServiceOptions
	mu      sync.Mutex
	data    map[string]profileData

	publishMu sync.Mutex
	cache     *snapshotCache
}

func NewService(cfg *config.Config, manager *appserver.Manager, store *Store, opts ServiceOptions) *ServiceRunner {
	if opts.StatusInterval <= 0 {
		opts.StatusInterval = 2 * time.Second
	}
	if opts.ThreadInterval <= 0 {
		opts.ThreadInterval = 5 * time.Second
	}
	if opts.AccountInterval <= 0 {
		opts.AccountInterval = 60 * time.Second
	}
	return &ServiceRunner{
		cfg: cfg, manager: manager, store: store, opts: opts,
		data: map[string]profileData{}, cache: newSnapshotCache(),
	}
}

func (s *ServiceRunner) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for _, name := range config.SortedProfileNames(s.cfg) {
		if s.opts.Filter.Profile != "" && name != s.opts.Filter.Profile {
			continue
		}
		profile := s.cfg.Profiles[name]
		wg.Add(1)
		go func(name string, profile config.Profile) {
			defer wg.Done()
			s.watchProfile(ctx, name, profile)
		}(name, profile)
	}
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()
	s.publish()
	for {
		select {
		case <-ctx.Done():
			wg.Wait()
			return
		case <-ticker.C:
			s.publish()
		}
	}
}

func (s *ServiceRunner) watchProfile(ctx context.Context, name string, profile config.Profile) {
	backoff := time.Second
	for ctx.Err() == nil {
		health := s.manager.Status(ctx, name)
		if !health.Healthy && s.opts.StartDaemons {
			started, err := s.manager.Start(ctx, name, profile.CodexHome)
			if err != nil {
				s.updateFailure(name, health, err)
				if !waitContext(ctx, backoff) {
					return
				}
				backoff = nextBackoff(backoff)
				continue
			}
			health = started
		}
		if !health.Healthy {
			s.updateFailure(name, health, errors.New(health.Error))
			if !waitContext(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		client, err := appserver.DialState(ctx, health.State)
		if err != nil {
			s.updateFailure(name, health, err)
			if !waitContext(ctx, backoff) {
				return
			}
			backoff = nextBackoff(backoff)
			continue
		}
		connectedAt := time.Now()
		s.consumeProfile(ctx, name, health, client)
		_ = client.Close()
		if ctx.Err() != nil {
			return
		}
		if time.Since(connectedAt) >= 30*time.Second {
			backoff = time.Second
		}
		if !waitContext(ctx, backoff) {
			return
		}
		backoff = nextBackoff(backoff)
	}
}

func (s *ServiceRunner) consumeProfile(ctx context.Context, name string, health appserver.Health, client *appserver.Client) {
	statusTicker := time.NewTicker(s.opts.StatusInterval)
	threadTicker := time.NewTicker(s.opts.ThreadInterval)
	accountTicker := time.NewTicker(s.opts.AccountInterval)
	defer statusTicker.Stop()
	defer threadTicker.Stop()
	defer accountTicker.Stop()
	s.updateService(name, health)
	s.refreshAccount(ctx, name, client)
	s.refreshThreads(ctx, name, client)
	for {
		select {
		case <-ctx.Done():
			return
		case <-client.Done():
			return
		case <-statusTicker.C:
			current := s.manager.Status(ctx, name)
			s.updateService(name, current)
			if !current.Healthy {
				return
			}
		case <-threadTicker.C:
			s.refreshThreads(ctx, name, client)
		case <-accountTicker.C:
			s.refreshAccount(ctx, name, client)
		case note, ok := <-client.Notifications():
			if !ok {
				return
			}
			s.applyNotification(name, note)
		}
	}
}

func (s *ServiceRunner) refreshAccount(ctx context.Context, name string, client *appserver.Client) {
	callCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	var accountResp appserver.AccountResponse
	var rateResp appserver.RateLimitsResponse
	var usageResp appserver.UsageResponse
	var mcpStatuses []appserver.MCPServerStatus
	var accountErr, rateErr, usageErr, mcpErr error
	var wait sync.WaitGroup
	wait.Add(4)
	go func() {
		defer wait.Done()
		accountResp, accountErr = client.Account(callCtx)
	}()
	go func() {
		defer wait.Done()
		rateResp, rateErr = client.RateLimits(callCtx)
	}()
	go func() {
		defer wait.Done()
		usageResp, usageErr = client.Usage(callCtx)
	}()
	go func() {
		defer wait.Done()
		mcpStatuses, mcpErr = client.MCPServers(callCtx)
	}()
	wait.Wait()
	account := Account{Profile: name, CodexHealthy: true, MCPHealthy: true}
	if accountResp.Account != nil {
		account.LoggedIn = true
		account.Email = maskEmail(accountResp.Account.Email)
		account.Plan = accountResp.Account.PlanType
	}
	if rateErr == nil {
		account.Primary = rateResp.RateLimits.Primary
		account.Secondary = rateResp.RateLimits.Secondary
		account.Credits = rateResp.RateLimits.Credits
	}
	if usageErr == nil {
		account.Lifetime = usageResp.Summary.LifetimeTokens
		account.Daily = usageResp.DailyUsageBuckets
	}
	if mcpErr == nil {
		for _, server := range mcpStatuses {
			status := MCPServer{Name: server.Name, Status: "ready", AuthStatus: server.AuthStatus}
			if server.AuthStatus == "notLoggedIn" {
				status.Status = "authentication_required"
				account.MCPHealthy = false
			}
			account.MCPServers = append(account.MCPServers, status)
		}
		sort.Slice(account.MCPServers, func(i, j int) bool {
			return account.MCPServers[i].Name < account.MCPServers[j].Name
		})
	}
	var errs []string
	checks := []struct {
		err error
		mcp bool
	}{{accountErr, false}, {rateErr, false}, {usageErr, false}, {mcpErr, true}}
	for _, check := range checks {
		if check.err != nil {
			var rpcErr *appserver.RPCError
			if errors.As(check.err, &rpcErr) && rpcErr.Code == -32601 {
				continue
			}
			if check.mcp {
				account.MCPHealthy = false
			}
			errs = append(errs, check.err.Error())
		}
	}
	account.Error = strings.Join(errs, "; ")
	s.mu.Lock()
	item := s.data[name]
	item.account = account
	item.updatedAt = time.Now()
	s.data[name] = item
	s.mu.Unlock()
	s.publish()
}

func (s *ServiceRunner) refreshThreads(ctx context.Context, name string, client *appserver.Client) {
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	var active, archived []appserver.Thread
	var activeErr, archivedErr error
	var wait sync.WaitGroup
	wait.Add(2)
	go func() {
		defer wait.Done()
		active, activeErr = client.Threads(callCtx, false)
	}()
	go func() {
		defer wait.Done()
		archived, archivedErr = client.Threads(callCtx, true)
	}()
	wait.Wait()
	err := activeErr
	if err != nil {
		s.setProfileError(name, err)
		return
	}
	if archivedErr == nil {
		for i := range archived {
			archived[i].Archived = true
		}
		active = append(active, archived...)
	}
	for i := range active {
		sanitizeThreadMetadata(&active[i])
	}
	s.mu.Lock()
	item := s.data[name]
	item.threads = active
	if item.tokens == nil {
		item.tokens = map[string]int64{}
	}
	item.updatedAt = time.Now()
	s.data[name] = item
	s.mu.Unlock()
	s.publish()
}

func (s *ServiceRunner) applyNotification(name string, note appserver.Notification) {
	if note.Method == "thread/tokenUsage/updated" {
		var payload struct {
			ThreadID   string `json:"threadId"`
			TokenUsage struct {
				Total struct {
					TotalTokens int64 `json:"totalTokens"`
				} `json:"total"`
			} `json:"tokenUsage"`
		}
		if json.Unmarshal(note.Params, &payload) == nil && payload.ThreadID != "" {
			s.mu.Lock()
			item := s.data[name]
			if item.tokens == nil {
				item.tokens = map[string]int64{}
			}
			item.tokens[payload.ThreadID] = payload.TokenUsage.Total.TotalTokens
			s.data[name] = item
			s.mu.Unlock()
			s.publish()
		}
		return
	}
	if note.Method == "account/rateLimits/updated" {
		var payload struct {
			RateLimits appserver.RateLimitSnapshot `json:"rateLimits"`
		}
		if json.Unmarshal(note.Params, &payload) == nil {
			s.mu.Lock()
			item := s.data[name]
			item.account.Primary = payload.RateLimits.Primary
			item.account.Secondary = payload.RateLimits.Secondary
			item.account.Credits = payload.RateLimits.Credits
			s.data[name] = item
			s.mu.Unlock()
			s.publish()
		}
		return
	}
	if note.Method == "mcpServer/startupStatus/updated" {
		var payload struct {
			Name          string `json:"name"`
			Status        string `json:"status"`
			Error         string `json:"error"`
			FailureReason string `json:"failureReason"`
		}
		if json.Unmarshal(note.Params, &payload) == nil && payload.Name != "" {
			s.mu.Lock()
			item := s.data[name]
			found := false
			for i := range item.account.MCPServers {
				if item.account.MCPServers[i].Name == payload.Name {
					item.account.MCPServers[i].Status = payload.Status
					item.account.MCPServers[i].Error = firstNonEmpty(payload.Error, payload.FailureReason)
					found = true
					break
				}
			}
			if !found {
				item.account.MCPServers = append(item.account.MCPServers, MCPServer{
					Name: payload.Name, Status: payload.Status,
					Error: firstNonEmpty(payload.Error, payload.FailureReason),
				})
			}
			item.account.MCPHealthy = mcpHealthy(item.account.MCPServers)
			s.data[name] = item
			s.mu.Unlock()
			s.publish()
		}
		return
	}
	if note.Method == "thread/status/changed" {
		var payload struct {
			ThreadID string          `json:"threadId"`
			Status   json.RawMessage `json:"status"`
		}
		if json.Unmarshal(note.Params, &payload) == nil && payload.ThreadID != "" {
			s.mu.Lock()
			item := s.data[name]
			for i := range item.threads {
				if item.threads[i].ID == payload.ThreadID {
					item.threads[i].Status = payload.Status
					item.threads[i].UpdatedAt = time.Now().Unix()
					break
				}
			}
			s.data[name] = item
			s.mu.Unlock()
			s.publish()
		}
		return
	}
	if note.Method == "thread/settings/updated" {
		var payload struct {
			ThreadID       string `json:"threadId"`
			ThreadSettings struct {
				Model string `json:"model"`
			} `json:"threadSettings"`
		}
		if json.Unmarshal(note.Params, &payload) == nil && payload.ThreadID != "" {
			s.updateThreadModel(name, payload.ThreadID, payload.ThreadSettings.Model)
		}
		return
	}
	if note.Method == "thread/name/updated" {
		var payload struct {
			ThreadID string `json:"threadId"`
			Name     string `json:"threadName"`
		}
		if json.Unmarshal(note.Params, &payload) == nil && payload.ThreadID != "" {
			s.mu.Lock()
			item := s.data[name]
			for i := range item.threads {
				if item.threads[i].ID == payload.ThreadID {
					item.threads[i].Title = shortText(payload.Name, 160)
					break
				}
			}
			s.data[name] = item
			s.mu.Unlock()
			s.publish()
		}
		return
	}
	if note.Method == "model/rerouted" || note.Method == "model/safetyBuffering/updated" {
		var payload struct {
			ThreadID string `json:"threadId"`
			ToModel  string `json:"toModel"`
			Model    string `json:"model"`
		}
		if json.Unmarshal(note.Params, &payload) == nil && payload.ThreadID != "" {
			s.updateThreadModel(name, payload.ThreadID, firstNonEmpty(payload.ToModel, payload.Model))
		}
		return
	}
	if note.Method == "thread/started" {
		var payload struct {
			Thread appserver.Thread `json:"thread"`
		}
		if json.Unmarshal(note.Params, &payload) == nil && payload.Thread.ID != "" {
			sanitizeThreadMetadata(&payload.Thread)
			s.mu.Lock()
			item := s.data[name]
			found := false
			for i := range item.threads {
				if item.threads[i].ID == payload.Thread.ID {
					item.threads[i] = payload.Thread
					found = true
					break
				}
			}
			if !found {
				item.threads = append(item.threads, payload.Thread)
			}
			s.data[name] = item
			s.mu.Unlock()
			s.publish()
		}
		return
	}
	switch note.Method {
	case "thread/archived", "thread/unarchived", "thread/deleted":
		var payload struct {
			ThreadID string `json:"threadId"`
		}
		if json.Unmarshal(note.Params, &payload) == nil && payload.ThreadID != "" {
			s.mu.Lock()
			item := s.data[name]
			for i := range item.threads {
				if item.threads[i].ID != payload.ThreadID {
					continue
				}
				if note.Method == "thread/deleted" {
					item.threads = append(item.threads[:i], item.threads[i+1:]...)
				} else {
					item.threads[i].Archived = note.Method == "thread/archived"
				}
				break
			}
			s.data[name] = item
			s.mu.Unlock()
			s.publish()
		}
		return
	}
	switch note.Method {
	case "turn/started", "turn/completed":
		// The periodic thread reconciliation follows shortly. Publish now so SSE
		// clients can update connection/service freshness without caching content.
		s.publish()
	}
}

func sanitizeThreadMetadata(thread *appserver.Thread) {
	thread.Title = shortText(thread.Title, 160)
	thread.Preview = shortText(thread.Preview, 160)
}

func (s *ServiceRunner) updateFailure(name string, health appserver.Health, err error) {
	s.mu.Lock()
	item := s.data[name]
	item.account.Profile = name
	item.account.CodexHealthy = false
	item.account.MCPHealthy = false
	if err != nil {
		item.account.Error = err.Error()
	}
	item.service = serviceFromHealth(name, health)
	s.data[name] = item
	s.mu.Unlock()
	s.publish()
}

func (s *ServiceRunner) updateService(name string, health appserver.Health) {
	s.mu.Lock()
	item := s.data[name]
	item.service = serviceFromHealth(name, health)
	s.data[name] = item
	s.mu.Unlock()
	s.publish()
}

func serviceFromHealth(name string, health appserver.Health) Service {
	return Service{
		Profile:  name,
		PID:      health.PID,
		Endpoint: health.Endpoint,
		Version:  health.CodexVersion,
		Started:  health.StartedAt,
		Healthy:  health.Healthy,
		Error:    health.Error,
	}
}

func (s *ServiceRunner) setProfileError(name string, err error) {
	s.mu.Lock()
	item := s.data[name]
	item.account.Error = err.Error()
	s.data[name] = item
	s.mu.Unlock()
	s.publish()
}

func (s *ServiceRunner) updateThreadModel(profile, threadID, model string) {
	if model == "" {
		return
	}
	s.mu.Lock()
	item := s.data[profile]
	for i := range item.threads {
		if item.threads[i].ID == threadID {
			item.threads[i].Model = model
			break
		}
	}
	s.data[profile] = item
	s.mu.Unlock()
	s.publish()
}

func (s *ServiceRunner) publish() {
	s.publishMu.Lock()
	defer s.publishMu.Unlock()
	if s.cache == nil {
		s.cache = newSnapshotCache()
	}
	s.mu.Lock()
	data := make(map[string]profileData, len(s.data))
	for name, item := range s.data {
		data[name] = item
	}
	s.mu.Unlock()
	snapshot := buildSnapshotWithCache(s.cfg, data, s.opts.Filter, s.manager.Home, s.cache)
	s.store.Publish(snapshot)
}

func waitContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func nextBackoff(current time.Duration) time.Duration {
	if current <= 0 {
		return time.Second
	}
	if current >= 30*time.Second {
		return 30 * time.Second
	}
	current *= 2
	if current > 30*time.Second {
		return 30 * time.Second
	}
	return current
}
