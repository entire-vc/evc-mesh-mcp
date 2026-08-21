package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// resultText extracts the plain-text error message a tool handler returned, so
// tests can assert on which field its FIRST clause names.
func resultText(t *testing.T, result *mcpsdk.CallToolResult) string {
	t.Helper()
	if len(result.Content) == 0 {
		t.Fatal("result has no content")
	}
	text, ok := result.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("content[0] is not TextContent, got %T", result.Content[0])
	}
	return text.Text
}

// TestMoveTask_InvalidTaskID_NamesTaskIDNotStatusSlug is the regression test for the
// live bug measured 2026-08-20: move_task(task_id=<invalid>, status_slug=<valid>)
// returned "invalid status_slug: get statuses: Bad Request: invalid task_id" — the
// FIRST clause blamed status_slug even though status_slug was fine and task_id was
// the actual problem. The task-scoped status route fails before any status list is
// fetched, so resolveStatusSlugForTask never reaches pickStatusBySlug — that failure
// must be labeled against task_id, not status_slug.
func TestMoveTask_InvalidTaskID_NamesTaskIDNotStatusSlug(t *testing.T) {
	taskID := "not-a-valid-task-id"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Every route this bad task_id could hit rejects it the same way a real
		// server does for a malformed ID: 400, not 404 — so the fallback in
		// resolveStatusSlugForTask must NOT fire, and the failure must surface
		// directly from the first call.
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 400, "message": "invalid task_id"})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())

	result, err := server.handleMoveTask(ctx, buildMoveTaskRequest(taskID, "done"))
	if err != nil {
		t.Fatalf("handleMoveTask returned a transport error: %v", err)
	}
	if !result.IsError {
		t.Fatal("an invalid task_id must surface as an error result")
	}

	msg := resultText(t, result)
	if !strings.HasPrefix(msg, "invalid task_id:") {
		t.Errorf("error message = %q, want it to start with %q (not blame status_slug, "+
			"which was never even looked at)", msg, "invalid task_id:")
	}
	if strings.HasPrefix(msg, "invalid status_slug:") {
		t.Errorf("error message = %q — regressed to blaming status_slug for a task_id failure", msg)
	}
}

// TestMoveTask_InvalidStatusSlug_StillNamesStatusSlug is the companion negative
// control: when task_id resolves fine and the slug genuinely doesn't exist in the
// project's status list, the error must still name status_slug. This is the one
// case statusSlugNotFoundError is for, and the fix must not swing the label the
// other way for it.
func TestMoveTask_InvalidStatusSlug_StillNamesStatusSlug(t *testing.T) {
	taskID := uuid.New().String()
	todoID, doneID := uuid.New().String(), uuid.New().String()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+taskID+"/statuses":
			_ = json.NewEncoder(w).Encode(statusFixture(todoID, doneID))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())

	result, err := server.handleMoveTask(ctx, buildMoveTaskRequest(taskID, "does-not-exist"))
	if err != nil {
		t.Fatalf("handleMoveTask returned a transport error: %v", err)
	}
	if !result.IsError {
		t.Fatal("a status_slug absent from the project's status list must surface as an error result")
	}

	msg := resultText(t, result)
	if !strings.HasPrefix(msg, "invalid status_slug:") {
		t.Errorf("error message = %q, want it to start with %q (task_id resolved fine; "+
			"the slug is the actual problem)", msg, "invalid status_slug:")
	}
}
