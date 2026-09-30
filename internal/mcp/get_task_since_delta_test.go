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

// getTaskSinceDeltaHarness serves GET /tasks/:id (with the given updated_at)
// and GET /tasks/:id/comments (three comments at fixed timestamps), so tests
// can place `since` before/after each boundary.
func getTaskSinceDeltaHarness(t *testing.T, taskID, taskUpdatedAt string) *Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/tasks/"+taskID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":             taskID,
				"title":          "delta test task",
				"description":    "a long static description that should not be repeated when nothing changed",
				"status_id":      "status-1",
				"assignee_id":    "agent-1",
				"assignee_name":  "Linus",
				"checked_out_by": "agent-1",
				"human_gate":     false,
				"updated_at":     taskUpdatedAt,
			})
		case r.URL.Path == "/api/v1/tasks/"+taskID+"/comments":
			// GetTaskComments requests sort_dir=desc and reverses the page back to
			// chronological order client-side — the fixture mirrors the real
			// server's newest-first wire order so that reversal round-trips to the
			// same ascending order a real call would produce.
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"id": "c3", "body": "newest", "created_at": "2026-09-30T14:00:00Z"},
					{"id": "c2", "body": "middle", "created_at": "2026-09-30T12:00:00Z"},
					{"id": "c1", "body": "oldest", "created_at": "2026-09-30T10:00:00Z"},
				},
				"total_count": 3,
				"has_more":    false,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
}

// TestHandleGetTask_NoSince_UnaffectedByNewParam is the negative control:
// omitting `since` must behave exactly as before — full task, no
// task_changed key, no filtering.
func TestHandleGetTask_NoSince_UnaffectedByNewParam(t *testing.T) {
	taskID := uuid.New().String()
	server := getTaskSinceDeltaHarness(t, taskID, "2026-09-30T13:00:00Z")

	_, decoded := callGetTask(t, server, map[string]any{"task_id": taskID, "include_comments": true})

	if _, present := decoded["task_changed"]; present {
		t.Errorf("task_changed must not appear when since is omitted, got %v", decoded["task_changed"])
	}
	task, ok := decoded["task"].(map[string]any)
	if !ok {
		t.Fatalf("task = %#v, want an object", decoded["task"])
	}
	if task["description"] == nil {
		t.Error("full task (no since) must keep description")
	}
	comments, _ := decoded["comments"].([]any)
	if len(comments) != 3 {
		t.Errorf("expected all 3 comments without since, got %d", len(comments))
	}
}

// TestHandleGetTask_SinceBeforeUpdate_ReturnsFullTaskChanged: the task's
// updated_at is after `since`, so the caller must get the full task and
// task_changed=true.
func TestHandleGetTask_SinceBeforeUpdate_ReturnsFullTaskChanged(t *testing.T) {
	taskID := uuid.New().String()
	server := getTaskSinceDeltaHarness(t, taskID, "2026-09-30T13:00:00Z")

	_, decoded := callGetTask(t, server, map[string]any{"task_id": taskID, "since": "2026-09-30T11:00:00Z"})

	if decoded["task_changed"] != true {
		t.Errorf("task_changed = %v, want true (task updated after since)", decoded["task_changed"])
	}
	task, ok := decoded["task"].(map[string]any)
	if !ok || task["description"] == nil {
		t.Errorf("changed task must keep full content including description, got %#v", decoded["task"])
	}
}

// TestHandleGetTask_SinceAfterUpdate_ReturnsStubUnchanged: the task's
// updated_at is before (or equal to) `since`, so the caller must get
// task_changed=false and a trimmed stub, not the full task.
func TestHandleGetTask_SinceAfterUpdate_ReturnsStubUnchanged(t *testing.T) {
	taskID := uuid.New().String()
	server := getTaskSinceDeltaHarness(t, taskID, "2026-09-30T13:00:00Z")

	_, decoded := callGetTask(t, server, map[string]any{"task_id": taskID, "since": "2026-09-30T15:00:00Z"})

	if decoded["task_changed"] != false {
		t.Errorf("task_changed = %v, want false (task not updated since)", decoded["task_changed"])
	}
	task, ok := decoded["task"].(map[string]any)
	if !ok {
		t.Fatalf("task = %#v, want an object", decoded["task"])
	}
	if _, has := task["description"]; has {
		t.Error("unchanged task must not repeat description")
	}
	if task["id"] != taskID {
		t.Errorf("stub must still carry id, got %v", task["id"])
	}
	if task["status_id"] != "status-1" {
		t.Errorf("stub must carry status_id, got %v", task["status_id"])
	}
}

// TestHandleGetTask_SinceFiltersComments covers requirement 1: only comments
// created after `since` come back.
func TestHandleGetTask_SinceFiltersComments(t *testing.T) {
	taskID := uuid.New().String()
	server := getTaskSinceDeltaHarness(t, taskID, "2026-09-30T13:00:00Z")

	_, decoded := callGetTask(t, server, map[string]any{
		"task_id":          taskID,
		"since":            "2026-09-30T11:00:00Z", // between c1 (10:00) and c2 (12:00)
		"include_comments": true,
	})

	comments, ok := decoded["comments"].([]any)
	if !ok {
		t.Fatalf("comments = %#v, want an array", decoded["comments"])
	}
	if len(comments) != 2 {
		t.Fatalf("expected 2 comments after since, got %d: %#v", len(comments), comments)
	}
	first := comments[0].(map[string]any)
	if first["id"] != "c2" {
		t.Errorf("comments[0].id = %v, want c2 (oldest comment after since)", first["id"])
	}
	second := comments[1].(map[string]any)
	if second["id"] != "c3" {
		t.Errorf("comments[1].id = %v, want c3", second["id"])
	}
}

// TestHandleGetTask_InvalidSince_Errors: a malformed since must be refused,
// not silently ignored — the same shape as every other RFC3339 param in this
// codebase (deadline, due_date, start_after).
func TestHandleGetTask_InvalidSince_Errors(t *testing.T) {
	taskID := uuid.New().String()
	server := getTaskSinceDeltaHarness(t, taskID, "2026-09-30T13:00:00Z")

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID, "since": "not-a-timestamp"}
	result, err := server.handleGetTask(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetTask returned a Go error instead of a tool error: %v", err)
	}
	if !result.IsError {
		t.Fatal("expected an error result for a malformed since, got success")
	}
}

// TestHandleGetTask_MissingTaskUpdatedAt_FailsOpen pins taskChangedSince's
// documented fail-open behavior: a task whose own updated_at is missing or
// unparsable can't be judged stale-vs-fresh, so it must be treated as
// changed (full task, task_changed:true) rather than silently downgraded to
// the trimmed stub. Mutation testing during review showed flipping this
// branch produced zero failures — this test closes that gap.
func TestHandleGetTask_MissingTaskUpdatedAt_FailsOpen(t *testing.T) {
	taskID := uuid.New().String()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/tasks/"+taskID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          taskID,
				"title":       "task with no updated_at",
				"description": "static body",
				"status_id":   "status-1",
				// updated_at intentionally omitted.
			})
		case r.URL.Path == "/api/v1/tasks/"+taskID+"/comments":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []map[string]any{}, "total_count": 0, "has_more": false})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	s := &Server{
		restClient: NewRESTClient(server.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}

	_, out := callGetTask(t, s, map[string]any{"task_id": taskID, "since": "2026-09-30T13:00:00Z"})
	if changed, _ := out["task_changed"].(bool); !changed {
		t.Fatalf("expected task_changed=true (fail open on missing updated_at), got %v", out["task_changed"])
	}
	task, _ := out["task"].(map[string]any)
	if _, ok := task["description"]; !ok {
		t.Fatalf("expected the full task (with description) on fail-open, got stub: %v", task)
	}
}

// TestHandleGetTask_MissingCommentCreatedAt_FailsOpen pins commentsSince's
// documented fail-open behavior: a comment with a missing or unparsable
// created_at can't be judged old-vs-new, so it must be kept rather than
// silently dropped. Same mutation-testing gap as above, for the comment
// filter instead of the task filter.
func TestHandleGetTask_MissingCommentCreatedAt_FailsOpen(t *testing.T) {
	taskID := uuid.New().String()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/tasks/"+taskID:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":          taskID,
				"title":       "task with a timestamp-less comment",
				"description": "static body",
				"status_id":   "status-1",
				"updated_at":  "2026-09-30T13:00:00Z",
			})
		case r.URL.Path == "/api/v1/tasks/"+taskID+"/comments":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"id": "c-no-ts", "body": "no timestamp at all"},
				},
				"total_count": 1,
				"has_more":    false,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)

	s := &Server{
		restClient: NewRESTClient(server.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}

	_, out := callGetTask(t, s, map[string]any{"task_id": taskID, "since": "2026-09-30T14:00:00Z", "include_comments": true})
	comments, _ := out["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("expected the timestamp-less comment to survive the filter (fail open), got %d comments: %v", len(comments), comments)
	}
}
