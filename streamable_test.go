package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpserver "github.com/entire-vc/evc-mesh-mcp/internal/mcp"
	sdkserver "github.com/mark3labs/mcp-go/server"
)

// fakeMeshAPI accepts exactly one agent key on /api/v1/agents/me.
func fakeMeshAPI(t *testing.T, goodKey string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agents/me" || r.Header.Get("X-Agent-Key") != goodKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           "11111111-1111-1111-1111-111111111111",
			"workspace_id": "22222222-2222-2222-2222-222222222222",
			"name":         "probe",
			"agent_type":   "agent",
		})
	}))
}

func postRPC(t *testing.T, url, key, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if key != "" {
		req.Header.Set("X-Agent-Key", key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The streamable endpoints must enforce the same agent-key auth as /sse and
// serve each profile's own tool set.
func TestStreamableHTTP_AuthAndProfiles(t *testing.T) {
	const key = "agk_good"
	api := fakeMeshAPI(t, key)
	defer api.Close()

	cache := &agentSessionCache{apiURL: api.URL}
	rc := mcpserver.NewRESTClient(api.URL, "")
	full := mcpserver.NewServer(mcpserver.ServerConfig{RESTClient: rc, Profile: mcpserver.ProfileFull})
	core := mcpserver.NewServer(mcpserver.ServerConfig{RESTClient: rc, Profile: mcpserver.ProfileCore})
	ctxFn := func(ctx context.Context, r *http.Request) context.Context {
		s, err := cache.GetOrAuthenticate(ctx, extractAgentKeyFromRequest(r))
		if err != nil {
			return ctx
		}
		return mcpserver.ContextWithSession(ctx, s)
	}
	opts := []sdkserver.StreamableHTTPOption{sdkserver.WithStateLess(true), sdkserver.WithHTTPContextFunc(ctxFn)}
	mux := http.NewServeMux()
	mux.Handle(streamablePath, requireAgentKey(cache, nil, "t", sdkserver.NewStreamableHTTPServer(full.MCPServer(), opts...)))
	mux.Handle(coreBasePath, requireAgentKey(cache, nil, "t", sdkserver.NewStreamableHTTPServer(core.MCPServer(), opts...)))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	initMsg := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`
	if code, _ := postRPC(t, ts.URL+streamablePath, "", initMsg); code != http.StatusUnauthorized {
		t.Errorf("no key: got %d, want 401", code)
	}
	if code, _ := postRPC(t, ts.URL+streamablePath, "agk_bad", initMsg); code != http.StatusForbidden {
		t.Errorf("bad key: got %d, want 403", code)
	}
	if code, _ := postRPC(t, ts.URL+streamablePath+"?agent_key="+key, "", initMsg); code != http.StatusBadRequest {
		t.Errorf("key in query string: got %d, want 400", code)
	}
	code, body := postRPC(t, ts.URL+streamablePath, key, initMsg)
	if code != http.StatusOK || !strings.Contains(body, `"serverInfo"`) {
		t.Fatalf("good key initialize: %d %s", code, body)
	}

	count := func(path string) int {
		code, body := postRPC(t, ts.URL+path, key, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
		if code != http.StatusOK {
			t.Fatalf("tools/list %s: %d %s", path, code, body)
		}
		return strings.Count(body, `"inputSchema"`)
	}
	nFull, nCore := count(streamablePath), count(coreBasePath)
	if nCore == 0 || nCore >= nFull {
		t.Errorf("core=%d full=%d tools; core must be non-empty and strictly smaller", nCore, nFull)
	}
}
