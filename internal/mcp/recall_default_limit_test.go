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

// Exercise the request sent by the real handler, including profile selection,
// rather than copying its limit resolver into a test.
func TestRecallDefaultLimit_RequestAndFallback(t *testing.T) {
	t.Setenv("RECALL_GRAPH_ENABLED", "false")
	cases := []struct {
		name string
		args map[string]any
		want int
	}{
		{"omitted compact", nil, 5},
		{"omitted full", map[string]any{"full": true}, 10},
		{"explicit default profile", map[string]any{"recall_profile": "default"}, 10},
		{"explicit factual profile", map[string]any{"recall_profile": "factual"}, 10},
		{"automatic factual", map[string]any{"query": "/srv/app"}, 5},
		{"automatic multi session", map[string]any{"query": "deployment history"}, 20},
		{"automatic multi session full", map[string]any{"query": "deployment history", "full": true}, 20},
		{"explicit multi session", map[string]any{"recall_profile": "multi-session"}, 20},
		{"explicit limit beats automatic profile", map[string]any{"query": "deployment history", "limit": 6}, 6},
		{"explicit limit beats explicit profile", map[string]any{"recall_profile": "multi-session", "limit": 1}, 1},
		{"explicit old default", map[string]any{"limit": 10}, 10},
		{"explicit full limit", map[string]any{"limit": 40, "full": true}, 40},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			seen := 0
			content := strings.Repeat("deployment notes ", 40) + "ANSWER_AT_END"
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/memories/recall-final/v1" {
					http.NotFound(w, r)
					return
				}
				seen, _ = strconv.Atoi(r.URL.Query().Get("limit"))
				items := make([]any, seen)
				for i := range items {
					items[i] = map[string]any{"key": fmt.Sprintf("deployment-%02d", i), "content": content, "score": 0.03, "tags": []string{"kind:learning"}, "scope": "project"}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total": seen, "limit": seen, "final_recall_version": 1})
			}))
			defer httpServer.Close()
			s := &Server{restClient: NewRESTClient(httpServer.URL, "test-key"), tracker: NewSessionTracker(), session: &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()}}
			args := map[string]any{"query": "deployment notes"}
			for k, v := range tc.args {
				args[k] = v
			}
			result, err := s.handleRecall(context.Background(), requestWith(args))
			if err != nil || result.IsError {
				t.Fatalf("handler failed: %v, result=%v", err, result)
			}
			if seen != tc.want {
				t.Errorf("REST limit=%d, want %d", seen, tc.want)
			}
			out := decodeRecallResult(t, result)
			items := out["items"].([]any)
			if len(items) != tc.want {
				t.Fatalf("rows=%d, want %d", len(items), tc.want)
			}
			got := items[0].(map[string]any)["content"].(string)
			if args["full"] == true {
				if got != content {
					t.Error("full fallback lost complete answer text")
				}
			} else if len([]rune(got)) != 301 || !strings.HasSuffix(got, "…") {
				t.Error("compact content contract changed")
			}
		})
	}
}

func TestRecallDefaultLimit_InvalidArgumentsRefused(t *testing.T) {
	for _, invalid := range []map[string]any{
		{"limit": nil}, {"limit": "10"}, {"limit": 5.5}, {"limit": false}, {"limit": 0}, {"limit": 51},
		{"recall_profile": nil}, {"recall_profile": "unknown"}, {"recall_profile": ""},
		{"full": "true"}, {"full": nil}, {"page_size": 10},
	} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			calls := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				_, _ = w.Write([]byte(`{"items":[],"total":0}`))
			}))
			defer srv.Close()
			s := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker(), session: &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()}}
			args := map[string]any{"query": "deployment notes"}
			for k, v := range invalid {
				args[k] = v
			}
			result, err := s.handleRecall(context.Background(), requestWith(args))
			if err != nil || !result.IsError || calls != 0 {
				t.Errorf("invalid argument silently reached REST: error=%v isError=%v calls=%d", err, result.IsError, calls)
			}
		})
	}
}

func TestRecallDefaultLimit_GetMemoryWhole(t *testing.T) {
	content := strings.Repeat("x", 500) + "ANSWER_AT_END"
	s, closeFn := newRecallTestServer(t, []any{map[string]any{"key": "known-answer", "content": content}}, nil)
	defer closeFn()
	var req mcpsdk.CallToolRequest
	req.Params.Arguments = map[string]any{"key": "known-answer"}
	r, err := s.handleGetMemory(context.Background(), req)
	if err != nil || r.IsError || decodeRecallResult(t, r)["content"] != content {
		t.Fatal("get_memory did not preserve the whole known answer")
	}
}
