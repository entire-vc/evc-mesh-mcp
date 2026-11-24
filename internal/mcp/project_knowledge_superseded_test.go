package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

// get_project_knowledge forwards include_superseded only when the caller asks
// for it, so the API default (superseded entries left out) applies otherwise.
func TestGetProjectKnowledge_IncludeSupersededQuery(t *testing.T) {
	var seen []url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/projects/p1/knowledge" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		seen = append(seen, r.URL.Query())
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"project_memories":[],"workspace_memories":[]}`))
	}))
	t.Cleanup(srv.Close)
	s := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker(), session: &AgentSession{}}

	for _, tc := range []struct {
		name string
		args map[string]any
		want string
	}{
		{"default", map[string]any{"project_id": "p1"}, ""},
		{"explicit false", map[string]any{"project_id": "p1", "include_superseded": false}, ""},
		{"explicit true", map[string]any{"project_id": "p1", "include_superseded": true}, "true"},
	} {
		seen = nil
		res, err := s.handleGetProjectKnowledge(context.Background(), reqWith(tc.args))
		if err != nil || res.IsError {
			t.Fatalf("%s: unexpected failure: %v %+v", tc.name, err, res)
		}
		if len(seen) != 1 {
			t.Fatalf("%s: want 1 request, got %d", tc.name, len(seen))
		}
		if got := seen[0].Get("include_superseded"); got != tc.want {
			t.Fatalf("%s: include_superseded=%q, want %q (query %v)", tc.name, got, tc.want, seen[0])
		}
		if seen[0].Get("limit") != "100" {
			t.Fatalf("%s: limit default lost: %v", tc.name, seen[0])
		}
	}
}

// The shipped tool schema names the option as a boolean that defaults to false.
func TestGetProjectKnowledge_IncludeSupersededSchema(t *testing.T) {
	for _, tool := range newTestServerTools(t) {
		if tool.Name != "get_project_knowledge" {
			continue
		}
		prop, ok := tool.InputSchema.Properties["include_superseded"].(map[string]any)
		if !ok {
			t.Fatalf("include_superseded missing from schema: %v", tool.InputSchema.Properties)
		}
		if prop["type"] != "boolean" || prop["default"] != false {
			t.Fatalf("include_superseded schema = %v, want boolean default false", prop)
		}
		return
	}
	t.Fatal("get_project_knowledge not registered")
}
