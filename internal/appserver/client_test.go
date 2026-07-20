package appserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestClientInitializesRoutesOutOfOrderResponsesAndNotifications(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var mu sync.Mutex
	var initialized bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var init wireMessage
		if err := conn.ReadJSON(&init); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"id": rawID(init.ID), "result": map[string]any{"userAgent": "test"}})
		var ack wireMessage
		_ = conn.ReadJSON(&ack)
		mu.Lock()
		initialized = ack.Method == "initialized"
		mu.Unlock()

		var first, second wireMessage
		_ = conn.ReadJSON(&first)
		_ = conn.ReadJSON(&second)
		_ = conn.WriteJSON(map[string]any{
			"method": "item/agentMessage/delta",
			"params": map[string]any{"delta": "must not enter the monitor queue"},
		})
		_ = conn.WriteJSON(map[string]any{
			"method": "thread/status/changed",
			"params": map[string]any{"threadId": "thread-1", "status": map[string]any{"type": "active"}},
		})
		_ = conn.WriteJSON(map[string]any{"id": rawID(second.ID), "result": map[string]any{"value": second.Method}})
		_ = conn.WriteJSON(map[string]any{"id": rawID(first.ID), "result": map[string]any{"value": first.Method}})
		<-time.After(50 * time.Millisecond)
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client, err := Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	type result struct {
		Value string `json:"value"`
	}
	results := make(chan result, 2)
	errors := make(chan error, 2)
	for _, method := range []string{"first/read", "second/read"} {
		go func(method string) {
			var out result
			errors <- client.Call(ctx, method, map[string]any{}, &out)
			results <- out
		}(method)
	}
	for range 2 {
		if err := <-errors; err != nil {
			t.Fatal(err)
		}
	}
	got := map[string]bool{(<-results).Value: true, (<-results).Value: true}
	if !got["first/read"] || !got["second/read"] {
		t.Fatalf("responses were misrouted: %+v", got)
	}
	select {
	case note := <-client.Notifications():
		if note.Method != "thread/status/changed" {
			t.Fatalf("unexpected notification: %+v", note)
		}
	case <-ctx.Done():
		t.Fatal("notification was not delivered")
	}
	mu.Lock()
	defer mu.Unlock()
	if !initialized {
		t.Fatal("initialized notification was not sent")
	}
}

func TestClientReturnsRPCMethodUnavailableAndAuthenticationFailure(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer good" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var init wireMessage
		_ = conn.ReadJSON(&init)
		_ = conn.WriteJSON(map[string]any{"id": rawID(init.ID), "result": map[string]any{}})
		var ack wireMessage
		_ = conn.ReadJSON(&ack)
		var request wireMessage
		_ = conn.ReadJSON(&request)
		_ = conn.WriteJSON(map[string]any{
			"id":    rawID(request.ID),
			"error": map[string]any{"code": -32601, "message": "Method not found"},
		})
	}))
	defer server.Close()
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := Dial(ctx, endpoint, "bad"); err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("authentication failure was not surfaced: %v", err)
	}
	client, err := Dial(ctx, endpoint, "good")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	err = client.Call(ctx, "missing/read", map[string]any{}, nil)
	var rpcErr *RPCError
	if !errorsAs(err, &rpcErr) || rpcErr.Code != -32601 {
		t.Fatalf("method unavailable error was not preserved: %T %v", err, err)
	}
}

func TestClientRetriesServerOverloadWithBackoff(t *testing.T) {
	upgrader := websocket.Upgrader{}
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var init wireMessage
		_ = conn.ReadJSON(&init)
		_ = conn.WriteJSON(map[string]any{"id": rawID(init.ID), "result": map[string]any{}})
		var ack wireMessage
		_ = conn.ReadJSON(&ack)
		for {
			var request wireMessage
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			requests++
			if requests < 3 {
				_ = conn.WriteJSON(map[string]any{
					"id":    rawID(request.ID),
					"error": map[string]any{"code": -32001, "message": "Server overloaded; retry later."},
				})
				continue
			}
			_ = conn.WriteJSON(map[string]any{"id": rawID(request.ID), "result": map[string]any{"ok": true}})
			return
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.retryBase = time.Millisecond
	client.maxRetries = 3
	var result struct {
		OK bool `json:"ok"`
	}
	if err := client.Call(ctx, "thread/list", map[string]any{}, &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK || requests != 3 {
		t.Fatalf("overload retry result=%+v requests=%d", result, requests)
	}
}

func TestReadOnlyMethodsUseProtocolParamsAndLoadedThreadPagination(t *testing.T) {
	upgrader := websocket.Upgrader{}
	failures := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			var request wireMessage
			if err := conn.ReadJSON(&request); err != nil {
				return
			}
			if len(request.ID) == 0 {
				continue
			}
			result := any(map[string]any{})
			switch request.Method {
			case "account/rateLimits/read", "account/usage/read":
				if string(request.Params) != "null" {
					failures <- request.Method + " params were " + string(request.Params)
				}
			case "thread/loaded/list":
				var params struct {
					Cursor string `json:"cursor"`
					Limit  int    `json:"limit"`
				}
				if err := json.Unmarshal(request.Params, &params); err != nil || params.Limit != 200 {
					failures <- "invalid loaded/list params: " + string(request.Params)
				}
				if params.Cursor == "" {
					next := "next-page"
					result = map[string]any{"data": []string{"one"}, "nextCursor": next}
				} else {
					result = map[string]any{"data": []string{"two"}}
				}
			}
			_ = conn.WriteJSON(map[string]any{"id": rawID(request.ID), "result": result})
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), "")
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	if _, err := client.RateLimits(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Usage(ctx); err != nil {
		t.Fatal(err)
	}
	loaded, err := client.LoadedThreads(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 2 || loaded[0] != "one" || loaded[1] != "two" {
		t.Fatalf("loaded thread pages were not combined: %q", loaded)
	}
	select {
	case failure := <-failures:
		t.Fatal(failure)
	default:
	}
}

func rawID(raw json.RawMessage) any {
	var value any
	_ = json.Unmarshal(raw, &value)
	return value
}

func errorsAs(err error, target **RPCError) bool {
	rpc, ok := err.(*RPCError)
	if ok {
		*target = rpc
	}
	return ok
}
