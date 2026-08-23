package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// getTaskVCSLinksHarness stands up a fake Mesh REST API that serves both
// GET /tasks/:id (the task itself) and GET /tasks/:id/vcs-links (the
// endpoint this test exists to wire up — internal/handler/vcs_link_handler.go
// List, in evc-mesh, already returns {"vcs_links":[...],"count":N}; nothing
// in evc-mesh changes for #5a6460b7, only this repo's get_task surface).
// vcsLinksHit records whether /vcs-links was ever requested, so the
// negative control (default get_task call) can assert on the wire, not just
// on the decoded response.
func getTaskVCSLinksHarness(t *testing.T, taskID string) (server *Server, vcsLinksHit *bool, closeFn func()) {
	t.Helper()
	hit := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+taskID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":             taskID,
				"title":          "some task",
				"vcs_link_count": 2,
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+taskID+"/vcs-links":
			hit = true
			_ = json.NewEncoder(w).Encode(map[string]any{
				"vcs_links": []map[string]any{
					{
						"id":          "11111111-1111-1111-1111-111111111111",
						"task_id":     taskID,
						"provider":    "gitlab",
						"link_type":   "pr",
						"external_id": "14",
						"url":         "https://git.entire.host/entire-vc/evc-mesh/-/merge_requests/14",
						"status":      "merged",
						"created_at":  "2026-08-23T10:00:00Z",
					},
					{
						"id":          "22222222-2222-2222-2222-222222222222",
						"task_id":     taskID,
						"provider":    "github",
						"link_type":   "pr",
						"external_id": "457",
						"url":         "https://github.com/entire-vc/evc-spark/pull/457",
						"status":      "open",
						"created_at":  "2026-08-24T09:00:00Z",
					},
				},
				"count": 2,
			})
		default:
			http.NotFound(w, r)
		}
	}))

	server = &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
	return server, &hit, srv.Close
}

func callGetTask(t *testing.T, server *Server, args map[string]any) (*mcpsdk.CallToolResult, map[string]any) {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	result, err := server.handleGetTask(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetTask returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned an error result: %v", result.Content)
	}
	text, ok := result.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("content[0] is not TextContent, got %T", result.Content[0])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody: %s", err, text.Text)
	}
	return result, decoded
}

// TestHandleGetTask_DefaultResponseDoesNotGrow is the mandatory negative
// control for #5a6460b7: get_task's default behaviour (no include_* flags)
// must be byte-for-byte unaffected by adding include_vcs_links. Before this
// change, /vcs-links wasn't called at all for a bare get_task — this test
// pins that it still isn't, and that no "vcs_links" key leaks into the
// response body when the caller never asked for it.
func TestHandleGetTask_DefaultResponseDoesNotGrow(t *testing.T) {
	taskID := uuid.New().String()
	server, vcsLinksHit, closeSrv := getTaskVCSLinksHarness(t, taskID)
	defer closeSrv()

	_, decoded := callGetTask(t, server, map[string]any{"task_id": taskID})

	if *vcsLinksHit {
		t.Error("GET /vcs-links was called on a bare get_task — default behaviour must not change")
	}
	for _, key := range []string{"vcs_links", "comments", "artifacts", "dependencies"} {
		if _, present := decoded[key]; present {
			t.Errorf("default get_task response unexpectedly contains %q: %#v", key, decoded[key])
		}
	}
	// Only "task" should be present — the response must not have grown.
	if len(decoded) != 1 {
		t.Errorf("default get_task response has %d top-level keys (%v), want exactly 1 (\"task\")", len(decoded), decoded)
	}
}

// TestHandleGetTask_IncludeVCSLinksTrue is the positive control: with the
// flag set, get_task must surface provider/status per link — the whole
// point of #5a6460b7 (diagnosing a misclassified/stuck link previously
// required a raw REST call no MCP tool exposed).
func TestHandleGetTask_IncludeVCSLinksTrue(t *testing.T) {
	taskID := uuid.New().String()
	server, vcsLinksHit, closeSrv := getTaskVCSLinksHarness(t, taskID)
	defer closeSrv()

	_, decoded := callGetTask(t, server, map[string]any{"task_id": taskID, "include_vcs_links": true})

	if !*vcsLinksHit {
		t.Fatal("GET /vcs-links was never called despite include_vcs_links=true")
	}
	links, ok := decoded["vcs_links"].([]any)
	if !ok || len(links) != 2 {
		t.Fatalf("vcs_links = %#v, want a 2-element array", decoded["vcs_links"])
	}
	first, ok := links[0].(map[string]any)
	if !ok {
		t.Fatalf("vcs_links[0] is not an object: %#v", links[0])
	}
	if first["provider"] != "gitlab" {
		t.Errorf("vcs_links[0].provider = %v, want gitlab", first["provider"])
	}
	if first["status"] != "merged" {
		t.Errorf("vcs_links[0].status = %v, want merged", first["status"])
	}
	for _, field := range []string{"id", "provider", "link_type", "external_id", "url", "status", "created_at"} {
		if _, present := first[field]; !present {
			t.Errorf("vcs_links[0] is missing field %q: %#v", field, first)
		}
	}
	second, ok := links[1].(map[string]any)
	if !ok {
		t.Fatalf("vcs_links[1] is not an object: %#v", links[1])
	}
	if second["provider"] != "github" || second["status"] != "open" {
		t.Errorf("vcs_links[1] = %#v, want provider=github status=open (must not collapse into the first link)", second)
	}
}

// TestHandleGetTask_IncludeVCSLinksEmpty checks the zero-links case renders
// an empty array, not a missing key or null — a caller iterating the
// response should never need to nil-check this field.
func TestHandleGetTask_IncludeVCSLinksEmpty(t *testing.T) {
	taskID := uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/tasks/"+taskID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID, "title": "no links yet"})
		case r.URL.Path == "/api/v1/tasks/"+taskID+"/vcs-links":
			_ = json.NewEncoder(w).Encode(map[string]any{"vcs_links": []map[string]any{}, "count": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}

	_, decoded := callGetTask(t, server, map[string]any{"task_id": taskID, "include_vcs_links": true})

	links, ok := decoded["vcs_links"].([]any)
	if !ok {
		t.Fatalf("vcs_links = %#v (%T), want an array", decoded["vcs_links"], decoded["vcs_links"])
	}
	if len(links) != 0 {
		t.Errorf("vcs_links = %#v, want empty", links)
	}
}
