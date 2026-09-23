package mcp

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// When startup authentication failed, a tool call must return the reason
// until authentication succeeds, then proceed normally — without re-running
// authentication on every later call.
func TestDeferredAuthRetriesUntilSuccess(t *testing.T) {
	var calls atomic.Int32
	var apiUp atomic.Bool
	srv := NewServer(ServerConfig{
		Profile:    ProfileCore,
		RESTClient: NewRESTClient("http://127.0.0.1:1", "agk_test"),
		Authenticate: func(ctx context.Context) (*AgentSession, error) {
			calls.Add(1)
			if !apiUp.Load() {
				return nil, errors.New("dial tcp: connection refused")
			}
			return &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New(), AgentName: "probe"}, nil
		},
	})
	rpc(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)

	list := rpc(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	if tools, _ := list["result"].(map[string]any)["tools"].([]any); len(tools) == 0 {
		t.Fatal("tools/list must work while unauthenticated")
	}

	call := func() map[string]any {
		r := rpc(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"heartbeat","arguments":{}}}`)
		res, _ := r["result"].(map[string]any)
		return res
	}
	res := call()
	if res["isError"] != true || !strings.Contains(mustJSON(t, res), "connection refused") {
		t.Fatalf("while the API is down a call must return the auth error, got %v", res)
	}
	if srv.getSession(context.Background()) != nil {
		t.Fatal("no session may be set after a failed authentication")
	}

	apiUp.Store(true)
	call() // the handler itself may fail (no real API) — what matters is auth
	if s := srv.getSession(context.Background()); s == nil || s.AgentName != "probe" {
		t.Fatalf("session not set after authentication succeeded: %+v", s)
	}
	before := calls.Load()
	call()
	if calls.Load() != before {
		t.Fatalf("authenticate re-ran after success (%d → %d calls)", before, calls.Load())
	}
}
