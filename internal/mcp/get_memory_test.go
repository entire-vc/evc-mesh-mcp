package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

func TestHandleGetMemory_ReturnsFullTextOfExactKey(t *testing.T) {
	full := strings.Repeat("z", 2500)
	items := []any{
		map[string]any{"key": "other-key-about-same-topic", "content": "nope", "score": 0.03},
		map[string]any{"key": "wanted-key", "content": full, "score": 0.01, "agent_id": "a"},
	}
	server, closeFn := newRecallTestServer(t, items, nil)
	defer closeFn()

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"key": "wanted-key"}
	result, err := server.handleGetMemory(context.Background(), req)
	if err != nil || result.IsError {
		t.Fatalf("get_memory: err=%v result=%v", err, result)
	}
	out := decodeRecallResult(t, result)
	if out["key"] != "wanted-key" {
		t.Fatalf("got key %v, want wanted-key", out["key"])
	}
	if c, _ := out["content"].(string); c != full {
		t.Errorf("content must be verbatim and uncut, got %d chars", len(c))
	}
}

func TestHandleGetMemory_MissingKeyIsAnError(t *testing.T) {
	items := []any{map[string]any{"key": "something-else", "content": "x", "score": 0.03}}
	server, closeFn := newRecallTestServer(t, items, nil)
	defer closeFn()

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"key": "wanted-key"}
	result, _ := server.handleGetMemory(context.Background(), req)
	if !result.IsError {
		t.Fatal("a key with no exact match must be an error, not the nearest neighbour")
	}
}

func TestHandleGetMemory_RequiresKey(t *testing.T) {
	server, closeFn := newRecallTestServer(t, nil, nil)
	defer closeFn()
	result, _ := server.handleGetMemory(context.Background(), mcpsdk.CallToolRequest{})
	if !result.IsError {
		t.Fatal("empty key must be an error")
	}
}

func TestGetMemory_RegisteredInCoreProfile(t *testing.T) {
	tools := NewServer(ServerConfig{Profile: ProfileCore}).MCPServer().ListTools()
	if _, ok := tools["get_memory"]; !ok {
		t.Fatal("get_memory is not registered in the core profile")
	}
}

// On an API without /memories/by-key the exact key sits on the second page of
// search results (50 decoys first): get_memory must keep paging instead of
// reporting "not found".
func TestHandleGetMemory_FindsKeyBeyondFirstPage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if notFoundByKey(w, r) {
			return
		}
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		items := []any{}
		for i := 0; i < getMemoryPageSize; i++ {
			k := fmt.Sprintf("decoy-%d", off+i)
			if off == getMemoryPageSize && i == 7 {
				k = "deep-key"
			}
			items = append(items, map[string]any{"key": k, "content": "c", "score": 0.03})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total": len(items)})
	}))
	defer srv.Close()
	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"key": "deep-key"}
	result, _ := server.handleGetMemory(context.Background(), req)
	if result.IsError {
		t.Fatalf("exact key on page 2 must be found: %v", result.Content)
	}
	if out := decodeRecallResult(t, result); out["key"] != "deep-key" {
		t.Errorf("got %v", out["key"])
	}
}

// A key is an address: on the search fallback a superseded entry must stay
// reachable, so the request has to say exclude_superseded=false instead of
// inheriting the server default.
func TestHandleGetMemory_AsksServerToIncludeSuperseded(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if notFoundByKey(w, r) {
			return
		}
		got = r.URL.Query().Get("exclude_superseded")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"key": "old-key", "content": "c"}}, "total": 1})
	}))
	defer srv.Close()
	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"key": "old-key"}
	if result, _ := server.handleGetMemory(context.Background(), req); result.IsError {
		t.Fatalf("lookup failed: %v", result.Content)
	}
	if got != "false" {
		t.Errorf("exclude_superseded sent as %q, want \"false\"", got)
	}
}

// notFoundByKey answers /memories/by-key with 404, as an API that predates the
// route does, and reports whether it did.
func notFoundByKey(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/api/v1/memories/by-key" {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	return true
}

// get_memory reads /memories/by-key and forwards both lifecycle flags: an
// archived or expired entry is reachable only when the caller asks.
func TestHandleGetMemory_ReadsByKeyWithLifecycleFlags(t *testing.T) {
	var paths []string
	var query map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		q := r.URL.Query()
		query = map[string]string{"key": q.Get("key"), "include_archived": q.Get("include_archived"), "include_expired": q.Get("include_expired"), "scope": q.Get("scope")}
		w.Header().Set("Content-Type", "application/json")
		items := []any{}
		if q.Get("include_archived") == "true" {
			items = append(items, map[string]any{"key": "stale-key", "content": "kept for history", "archived": true})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total": len(items)})
	}))
	defer srv.Close()
	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"key": "stale-key"}
	result, _ := server.handleGetMemory(context.Background(), req)
	if !result.IsError {
		t.Fatal("an archived entry must not come back without include_archived")
	}
	text := result.Content[0].(mcpsdk.TextContent).Text
	if !strings.Contains(text, "include_archived=true") {
		t.Errorf("not-found must name the flag that would show a hidden entry, got %q", text)
	}

	req.Params.Arguments = map[string]any{"key": "stale-key", "include_archived": true, "include_expired": true, "scope": "project"}
	result, _ = server.handleGetMemory(context.Background(), req)
	if result.IsError {
		t.Fatalf("include_archived=true must return the archived entry: %v", result.Content)
	}
	if out := decodeRecallResult(t, result); out["archived"] != true {
		t.Errorf("got %v", out)
	}
	for _, p := range paths {
		if p != "/api/v1/memories/by-key" {
			t.Errorf("get_memory called %s; it must read by key when the route exists", p)
		}
	}
	want := map[string]string{"key": "stale-key", "include_archived": "true", "include_expired": "true", "scope": "project"}
	for k, v := range want {
		if query[k] != v {
			t.Errorf("%s sent as %q, want %q", k, query[k], v)
		}
	}
}

// On the search fallback include_expired reaches the server too.
func TestHandleGetMemory_FallbackForwardsIncludeExpired(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if notFoundByKey(w, r) {
			return
		}
		got = r.URL.Query().Get("include_expired")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"key": "old-key", "content": "c"}}, "total": 1})
	}))
	defer srv.Close()
	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"key": "old-key", "include_expired": true}
	if result, _ := server.handleGetMemory(context.Background(), req); result.IsError {
		t.Fatalf("lookup failed: %v", result.Content)
	}
	if got != "true" {
		t.Errorf("include_expired sent as %q, want \"true\"", got)
	}
}
