package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// TestListTasksTool_ExposesListRevisionParam pins the schema half of the fix:
// list_tasks' input schema previously had no list_revision field at all, so an
// agent could not pass one back even though the REST API (entire-vc/evc-mesh
// #710, ADR-0004) already validates it and the tool could already read one back
// on a response (pass-through on read was never broken — only the write side
// was unreachable).
func TestListTasksTool_ExposesListRevisionParam(t *testing.T) {
	server := NewServer(ServerConfig{})

	tool, ok := server.MCPServer().ListTools()["list_tasks"]
	if !ok {
		t.Fatal("list_tasks tool is not registered")
	}
	props := tool.Tool.InputSchema.Properties

	raw, present := props["list_revision"]
	if !present {
		t.Fatal("list_tasks must accept list_revision, otherwise no agent can send it back on a follow-up page")
	}
	spec, _ := raw.(map[string]any)
	desc, _ := spec["description"].(string)
	if strings.TrimSpace(desc) == "" {
		t.Error("list_revision needs a description: an undocumented parameter is one nobody passes")
	}

	for _, req := range tool.Tool.InputSchema.Required {
		if req == "list_revision" {
			t.Error("list_revision must stay optional — a fresh walk (page 1) has no prior revision to send")
		}
	}
}

// TestListTasksTool_RevisionRoundTrip exercises the full round trip through the
// actual MCP tool handler (handleListTasks), not just the REST client in
// isolation: a first list_tasks call, extracting list_revision from its
// response, reusing it on a second call against matching server state
// (succeeds), and then a case where the server state has since moved on — the
// tool call must surface the REST API's 410 list_revision_stale as an MCP
// error result, not a silent 200.
func TestListTasksTool_RevisionRoundTrip(t *testing.T) {
	projectID := uuid.New().String()
	const currentRevision = int64(7)

	var gotFirstCallQuery, gotSecondCallQuery string
	sawStaleRequest := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/projects/"+projectID+"/tasks" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")

		listRevision := r.URL.Query().Get("list_revision")
		switch listRevision {
		case "":
			// Page 1 of a fresh walk: no revision sent, server stamps the
			// response with the project's current counter.
			gotFirstCallQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":         []map[string]any{{"id": "task-1", "title": "first page"}},
				"total_count":   2,
				"list_revision": currentRevision,
			})
		case "7":
			// Follow-up page of the same walk, revision still matches —
			// server serves it normally (200), echoing the same revision.
			gotSecondCallQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":         []map[string]any{{"id": "task-2", "title": "second page"}},
				"total_count":   2,
				"list_revision": currentRevision,
			})
		default:
			// Any other value: the caller's cursor no longer matches the
			// project's current task_list_revision — mirrors the real
			// server's HTTP 410 body shape (entire-vc/evc-mesh#710).
			sawStaleRequest = true
			w.WriteHeader(http.StatusGone)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": "list_revision_stale",
				"message": "task_list_revision changed since this cursor was issued " +
					"(had 3, now 7); restart pagination from page 1",
				"requested_revision": 3,
				"current_revision":   currentRevision,
			})
		}
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	ctx := context.Background()

	// --- Call 1: fresh walk, no list_revision sent. ---
	firstResult, err := server.handleListTasks(ctx, requestWith(map[string]any{
		"project_id": projectID,
	}))
	if err != nil {
		t.Fatalf("call 1 (fresh walk): transport error: %v", err)
	}
	if firstResult.IsError {
		t.Fatalf("call 1 (fresh walk): unexpected error result: %s", resultText(t, firstResult))
	}
	if gotFirstCallQuery != "page_size=50" {
		t.Fatalf("call 1: expected no list_revision on the fresh-walk request, got query %q", gotFirstCallQuery)
	}

	var firstPage struct {
		ListRevision int64 `json:"list_revision"`
	}
	if err := json.Unmarshal([]byte(resultText(t, firstResult)), &firstPage); err != nil {
		t.Fatalf("call 1: could not decode response JSON: %v", err)
	}
	if firstPage.ListRevision != currentRevision {
		t.Fatalf("call 1: response list_revision = %d, want %d (pass-through on read must still work)",
			firstPage.ListRevision, currentRevision)
	}

	// --- Call 2: follow-up page, echoing back the revision from call 1. This is
	// the actual gap under test — before this fix there was no schema field to
	// carry firstPage.ListRevision back out to the tool caller. ---
	secondResult, err := server.handleListTasks(ctx, requestWith(map[string]any{
		"project_id":    projectID,
		"list_revision": firstPage.ListRevision,
	}))
	if err != nil {
		t.Fatalf("call 2 (matching revision): transport error: %v", err)
	}
	if secondResult.IsError {
		t.Fatalf("call 2 (matching revision): unexpected error result: %s", resultText(t, secondResult))
	}
	if gotSecondCallQuery == "" || !strings.Contains(gotSecondCallQuery, "list_revision=7") {
		t.Fatalf("call 2: expected list_revision=7 threaded through to the REST query, got %q", gotSecondCallQuery)
	}

	// --- Call 3: stale revision — the project moved on since call 1's snapshot.
	// The tool must surface the 410 as an MCP error result, not a silent 200. ---
	staleResult, err := server.handleListTasks(ctx, requestWith(map[string]any{
		"project_id":    projectID,
		"list_revision": 3,
	}))
	if err != nil {
		t.Fatalf("call 3 (stale revision): transport error: %v", err)
	}
	if !sawStaleRequest {
		t.Fatal("call 3: server never saw a request with the stale list_revision — param wasn't sent")
	}
	if !staleResult.IsError {
		t.Fatalf("call 3 (stale revision): expected an MCP error result (410 must not be swallowed as success), got: %s",
			resultText(t, staleResult))
	}
	msg := resultText(t, staleResult)
	for _, want := range []string{"list_revision", "restart pagination"} {
		if !strings.Contains(msg, want) {
			t.Errorf("call 3: error message = %q, expected it to mention %q — the 410's message body "+
				"must reach the caller, not be replaced with a generic failure", msg, want)
		}
	}
}
