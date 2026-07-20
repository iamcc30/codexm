package appserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type wireMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

type response struct {
	result json.RawMessage
	err    error
}

type Client struct {
	conn          *websocket.Conn
	nextID        atomic.Int64
	writeMu       sync.Mutex
	pendingMu     sync.Mutex
	pending       map[int64]chan response
	notifications chan Notification
	done          chan struct{}
	closeOnce     sync.Once
	readErr       atomic.Value
	retryBase     time.Duration
	maxRetries    int
}

func Dial(ctx context.Context, endpoint, token string) (*Client, error) {
	headers := http.Header{}
	if token != "" {
		headers.Set("Authorization", "Bearer "+token)
	}
	conn, resp, err := websocket.DefaultDialer.DialContext(ctx, endpoint, headers)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("app-server websocket handshake returned %s: %w", resp.Status, err)
		}
		return nil, fmt.Errorf("connect app-server: %w", err)
	}
	c := &Client{
		conn:          conn,
		pending:       map[int64]chan response{},
		notifications: make(chan Notification, 256),
		done:          make(chan struct{}),
		retryBase:     100 * time.Millisecond,
		maxRetries:    4,
	}
	go c.readLoop()
	var initialized struct{}
	if err := c.Call(ctx, "initialize", map[string]any{
		"clientInfo": map[string]string{
			"name":    "codexm",
			"title":   "codexm monitor",
			"version": "dev",
		},
		"capabilities": map[string]any{
			"experimentalApi": true,
			"optOutNotificationMethods": []string{
				"item/agentMessage/delta",
				"item/started",
				"item/completed",
				"item/plan/delta",
				"item/reasoning/textDelta",
				"item/reasoning/summaryPartAdded",
				"item/reasoning/summaryTextDelta",
				"item/commandExecution/outputDelta",
				"item/commandExecution/terminalInteraction",
				"item/fileChange/outputDelta",
				"item/fileChange/patchUpdated",
				"item/mcpToolCall/progress",
				"command/exec/outputDelta",
				"process/outputDelta",
				"thread/realtime/outputAudio/delta",
				"thread/realtime/transcript/delta",
				"turn/diff/updated",
				"turn/plan/updated",
			},
		},
	}, &initialized); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("initialize app-server: %w", err)
	}
	if err := c.Notify("initialized", map[string]any{}); err != nil {
		_ = c.Close()
		return nil, fmt.Errorf("acknowledge app-server initialization: %w", err)
	}
	return c, nil
}

func DialState(ctx context.Context, state State) (*Client, error) {
	tokenBytes, err := os.ReadFile(state.TokenFile)
	if err != nil {
		return nil, fmt.Errorf("read app-server capability token: %w", err)
	}
	return Dial(ctx, state.Endpoint, string(tokenBytes))
}

func (c *Client) Notifications() <-chan Notification { return c.notifications }
func (c *Client) Done() <-chan struct{}              { return c.done }

func (c *Client) Call(ctx context.Context, method string, params, out any) error {
	var last error
	for attempt := 0; attempt <= c.maxRetries; attempt++ {
		last = c.callOnce(ctx, method, params, out)
		var rpcErr *RPCError
		if !errors.As(last, &rpcErr) || rpcErr.Code != -32001 || attempt == c.maxRetries {
			return last
		}
		delay := c.retryBase << attempt
		if delay > 2*time.Second {
			delay = 2 * time.Second
		}
		// Deterministic per-request jitter avoids synchronized monitor clients
		// hammering a recovering bounded app-server queue.
		jitter := time.Duration((c.nextID.Load()*37)%51) * delay / 100
		timer := time.NewTimer(delay + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-c.done:
			timer.Stop()
			if err, _ := c.readErr.Load().(error); err != nil {
				return err
			}
			return errors.New("app-server connection closed")
		case <-timer.C:
		}
	}
	return last
}

func (c *Client) callOnce(ctx context.Context, method string, params, out any) error {
	id := c.nextID.Add(1)
	ch := make(chan response, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()
	if err := c.write(map[string]any{"id": id, "method": method, "params": params}); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		if err, _ := c.readErr.Load().(error); err != nil {
			return err
		}
		return errors.New("app-server connection closed")
	case resp := <-ch:
		if resp.err != nil {
			return resp.err
		}
		if out == nil || len(resp.result) == 0 || string(resp.result) == "null" {
			return nil
		}
		if err := json.Unmarshal(resp.result, out); err != nil {
			return fmt.Errorf("decode %s response: %w", method, err)
		}
		return nil
	}
}

func (c *Client) Notify(method string, params any) error {
	return c.write(map[string]any{"method": method, "params": params})
}

func (c *Client) write(value any) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	_ = c.conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := c.conn.WriteJSON(value); err != nil {
		return fmt.Errorf("write app-server message: %w", err)
	}
	return nil
}

func (c *Client) readLoop() {
	defer c.shutdown(errors.New("app-server connection closed"))
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			c.shutdown(fmt.Errorf("read app-server message: %w", err))
			return
		}
		var msg wireMessage
		if err := json.Unmarshal(data, &msg); err != nil {
			continue
		}
		if len(msg.ID) > 0 {
			var id int64
			if err := json.Unmarshal(msg.ID, &id); err != nil {
				continue
			}
			c.pendingMu.Lock()
			ch := c.pending[id]
			c.pendingMu.Unlock()
			if ch != nil {
				if msg.Error != nil {
					ch <- response{err: msg.Error}
				} else {
					ch <- response{result: msg.Result}
				}
			}
			continue
		}
		if isMonitorNotification(msg.Method) {
			params := msg.Params
			if msg.Method == "turn/started" || msg.Method == "turn/completed" {
				params = nil
			}
			select {
			case c.notifications <- Notification{Method: msg.Method, Params: params}:
			default:
				// Monitoring is reconciled periodically, so a slow UI must never
				// apply backpressure to the app-server connection.
			}
		}
	}
}

func isMonitorNotification(method string) bool {
	switch method {
	case "thread/tokenUsage/updated",
		"account/rateLimits/updated",
		"mcpServer/startupStatus/updated",
		"thread/status/changed",
		"thread/settings/updated",
		"thread/name/updated",
		"model/rerouted",
		"model/safetyBuffering/updated",
		"thread/started",
		"thread/archived",
		"thread/unarchived",
		"thread/deleted",
		"turn/started",
		"turn/completed":
		return true
	default:
		return false
	}
}

func (c *Client) shutdown(err error) {
	c.closeOnce.Do(func() {
		c.readErr.Store(err)
		close(c.done)
		close(c.notifications)
		c.pendingMu.Lock()
		for _, ch := range c.pending {
			select {
			case ch <- response{err: err}:
			default:
			}
		}
		c.pendingMu.Unlock()
	})
}

func (c *Client) Close() error {
	err := c.conn.Close()
	// The read loop owns channel shutdown. Waiting for it avoids racing a
	// notification send against closing the notification channel.
	<-c.done
	return err
}

func (c *Client) Account(ctx context.Context) (AccountResponse, error) {
	var out AccountResponse
	err := c.Call(ctx, "account/read", map[string]any{"refreshToken": false}, &out)
	return out, err
}

func (c *Client) RateLimits(ctx context.Context) (RateLimitsResponse, error) {
	var out RateLimitsResponse
	err := c.Call(ctx, "account/rateLimits/read", nil, &out)
	return out, err
}

func (c *Client) Usage(ctx context.Context) (UsageResponse, error) {
	var out UsageResponse
	err := c.Call(ctx, "account/usage/read", nil, &out)
	return out, err
}

func (c *Client) MCPServers(ctx context.Context) ([]MCPServerStatus, error) {
	var all []MCPServerStatus
	var cursor *string
	for {
		var out MCPServerListResponse
		params := map[string]any{"limit": 200, "detail": "toolsAndAuthOnly"}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		if err := c.Call(ctx, "mcpServerStatus/list", params, &out); err != nil {
			return all, err
		}
		all = append(all, out.Data...)
		if out.NextCursor == nil || *out.NextCursor == "" {
			return all, nil
		}
		cursor = out.NextCursor
	}
}

func (c *Client) Threads(ctx context.Context, archived bool) ([]Thread, error) {
	var all []Thread
	var cursor *string
	for {
		var out ThreadListResponse
		params := map[string]any{
			"archived":      archived,
			"limit":         200,
			"sortKey":       "updated_at",
			"sortDirection": "desc",
			"sourceKinds": []string{
				"cli", "vscode", "exec", "appServer", "subAgent",
				"subAgentReview", "subAgentCompact", "subAgentThreadSpawn",
				"subAgentOther", "unknown",
			},
		}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		if err := c.Call(ctx, "thread/list", params, &out); err != nil {
			return all, err
		}
		all = append(all, out.Data...)
		if out.NextCursor == nil || *out.NextCursor == "" {
			return all, nil
		}
		cursor = out.NextCursor
	}
}

func (c *Client) LoadedThreads(ctx context.Context) ([]string, error) {
	var all []string
	var cursor *string
	for {
		var out LoadedThreadListResponse
		params := map[string]any{"limit": 200}
		if cursor != nil {
			params["cursor"] = *cursor
		}
		if err := c.Call(ctx, "thread/loaded/list", params, &out); err != nil {
			return all, err
		}
		all = append(all, out.Data...)
		if out.NextCursor == nil || *out.NextCursor == "" {
			return all, nil
		}
		cursor = out.NextCursor
	}
}
