package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// finalRecallStub records every request the handler makes and answers
// /memories/recall-final/v1 with the given status and body.
type finalRecallStub struct {
	mu     sync.Mutex
	paths  []string
	finals []url.Values
}

func (f *finalRecallStub) server(t *testing.T, status int, body map[string]any) *Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		if r.URL.Path == "/api/v1/memories/recall-final/v1" {
			f.finals = append(f.finals, r.URL.Query())
		}
		f.mu.Unlock()
		if r.URL.Path != "/api/v1/memories/recall-final/v1" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	return &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker(), session: &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()}}
}

func TestRecallFinal_FailsClosedWithoutFallbackToSearch(t *testing.T) {
	t.Setenv("RECALL_GRAPH_ENABLED", "false")
	cases := map[string]struct {
		status int
		body   map[string]any
	}{
		"old API without the route":     {http.StatusNotFound, map[string]any{"error": "not found"}},
		"route without version":         {http.StatusOK, map[string]any{"items": []any{}, "total": 0}},
		"route with fractional version": {http.StatusOK, map[string]any{"items": []any{}, "final_recall_version": 1.5}},
		"route with another version":    {http.StatusOK, map[string]any{"items": []any{}, "final_recall_version": 2}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			stub := &finalRecallStub{}
			s := stub.server(t, tc.status, tc.body)
			result, err := s.handleRecall(context.Background(), requestWith(map[string]any{"query": "self release of a checkout lock"}))
			if err != nil || !result.IsError {
				t.Fatalf("want a tool error, got err=%v result=%v", err, result)
			}
			for _, p := range stub.paths {
				if p != "/api/v1/memories/recall-final/v1" {
					t.Errorf("fell back to %s; legacy search would boost rows the caller drops", p)
				}
			}
			if len(stub.paths) != 1 {
				t.Errorf("requests=%v, want exactly one final-recall call", stub.paths)
			}
		})
	}
}

// The handler formats the server's page: same rows, same order, no second
// request (no legacy search, no recall_graph), nothing dropped or appended.
func TestRecallFinal_ServerPageIsPassedThroughUnchanged(t *testing.T) {
	t.Setenv("RECALL_GRAPH_ENABLED", "true")
	items := []any{
		map[string]any{"key": "z-low-key-match", "content": "a", "score": 0.0123},
		map[string]any{"key": "self-release-checkout", "content": "b", "score": 0.0456},
		map[string]any{"key": "via-graph", "content": "c", "score": 0.5, "graph_boost": true, "provenance": "via:graph"},
	}
	stub := &finalRecallStub{}
	s := stub.server(t, http.StatusOK, map[string]any{"items": items, "total": 3, "final_recall_version": 1, "limit": 5})
	result, err := s.handleRecall(context.Background(), requestWith(map[string]any{"query": "self-release-checkout"}))
	if err != nil || result.IsError {
		t.Fatalf("handler failed: %v %v", err, result)
	}
	if len(stub.paths) != 1 {
		t.Fatalf("requests=%v, want one", stub.paths)
	}
	got := decodeRecallResult(t, result)["items"].([]any)
	want := []string{"z-low-key-match", "self-release-checkout", "via-graph"}
	if len(got) != len(want) {
		t.Fatalf("rows=%d, want %d", len(got), len(want))
	}
	for i, k := range want {
		if got[i].(map[string]any)["key"] != k {
			t.Errorf("row %d = %v, want %s (server order must survive)", i, got[i].(map[string]any)["key"], k)
		}
	}
	if got[2].(map[string]any)["provenance"] != "via:graph" {
		t.Error("graph provenance lost")
	}
}

func TestRecallFinal_ResolvedArgumentsReachTheServer(t *testing.T) {
	cases := []struct {
		name  string
		graph string
		args  map[string]any
		want  map[string]string
	}{
		{"omitted compact graph off", "false", nil, map[string]string{"limit": "5", "graph": ""}},
		{"omitted compact graph on", "true", nil, map[string]string{"limit": "5", "graph": "true"}},
		{"full", "false", map[string]any{"full": true}, map[string]string{"limit": "10"}},
		{"explicit", "false", map[string]any{"limit": 7, "offset": 3, "scope": "project", "min_importance": 0.6, "recall_profile": "default"},
			map[string]string{"limit": "7", "offset": "3", "scope": "project", "min_importance": "0.6"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("RECALL_GRAPH_ENABLED", tc.graph)
			stub := &finalRecallStub{}
			s := stub.server(t, http.StatusOK, map[string]any{"items": []any{}, "total": 0, "final_recall_version": 1})
			args := map[string]any{"query": "self release"}
			for k, v := range tc.args {
				args[k] = v
			}
			if _, err := s.handleRecall(context.Background(), requestWith(args)); err != nil {
				t.Fatal(err)
			}
			if len(stub.finals) != 1 {
				t.Fatalf("final calls=%d", len(stub.finals))
			}
			for k, v := range tc.want {
				if got := stub.finals[0].Get(k); got != v {
					t.Errorf("%s=%q, want %q", k, got, v)
				}
			}
		})
	}
}
