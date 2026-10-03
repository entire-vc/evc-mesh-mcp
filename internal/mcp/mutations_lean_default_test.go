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

// Mutations returned the whole card — description and all (~1.5k chars per
// call, Jacques 03.10) — for a confirmation the caller usually needs as
// "id + status + assignee". The default response is now that lean projection;
// full=true restores the verbatim pre-change shape (the same contract as
// get_my_tasks / list_tasks / recall).
//
// fullTaskFixture is what the API returns for a task: identity + the fat
// static fields a mutation caller just paid to write and does not need echoed.
func fullTaskFixture(id string) map[string]any {
	return map[string]any{
		"id": id, "title": "some title",
		"description": "a long description that costs real tokens every echo",
		"status_id":   "st-1", "assignee_id": "ag-1", "assignee_type": "agent",
		"assignee_name": "Somebody", "priority": "high", "labels": []any{"x"},
		"created_at": "2026-01-01T00:00:00Z", "updated_at": "2026-10-03T12:00:00Z",
		"custom_fields": map[string]any{"k": "v"}, "position": 3.0,
	}
}

func decodeResultMap(t *testing.T, result *mcpsdk.CallToolResult) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(resultText(t, result)), &m); err != nil {
		t.Fatalf("decode result JSON: %v", err)
	}
	return m
}

// assertLeanTaskShape checks the lean contract: identity/status/assignee/
// updated_at present, the fat static fields (description above all) absent.
func assertLeanTaskShape(t *testing.T, m map[string]any) {
	t.Helper()
	for _, k := range []string{"id", "status_id", "assignee_id", "updated_at"} {
		if _, ok := m[k]; !ok {
			t.Errorf("lean task missing %q", k)
		}
	}
	for _, k := range []string{"description", "title", "labels", "custom_fields", "created_at", "position"} {
		if _, ok := m[k]; ok {
			t.Errorf("lean task must not echo %q", k)
		}
	}
}

func TestUpdateTask_LeanByDefault(t *testing.T) {
	taskID := uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fullTaskFixture(taskID))
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID, "title": "renamed"}

	res, herr := server.handleUpdateTask(context.Background(), req)
	m := decodeResultMap(t, mustOK(t, res, herr))
	assertLeanTaskShape(t, m)
}

func TestUpdateTask_FullReturnsVerbatimTask(t *testing.T) {
	taskID := uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fullTaskFixture(taskID))
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID, "title": "renamed", "full": true}

	res, herr := server.handleUpdateTask(context.Background(), req)
	m := decodeResultMap(t, mustOK(t, res, herr))
	if _, ok := m["description"]; !ok {
		t.Error("full=true must return the task verbatim, description included")
	}
	if _, ok := m["labels"]; !ok {
		t.Error("full=true must return the task verbatim, labels included")
	}
}

func TestAssignTask_LeanByDefault(t *testing.T) {
	taskID := uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fullTaskFixture(taskID))
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID, "assignee_id": "ag-2"}

	res, herr := server.handleAssignTask(ctx, req)
	m := decodeResultMap(t, mustOK(t, res, herr))
	assertLeanTaskShape(t, m)
	if m["assignee_id"] != "ag-1" {
		t.Errorf("assignee_id = %v, want the post-assign value from the response", m["assignee_id"])
	}
}

func TestAssignTask_FullReturnsVerbatimTask(t *testing.T) {
	taskID := uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fullTaskFixture(taskID))
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID, "assignee_id": "ag-2", "full": true}

	res, herr := server.handleAssignTask(ctx, req)
	m := decodeResultMap(t, mustOK(t, res, herr))
	if _, ok := m["description"]; !ok {
		t.Error("full=true must return the task verbatim")
	}
}

// add_comment's fat part is the body echo — the caller just wrote it and has
// it in context. The lean shape keeps the mention-delivery contract (delivery
// / hint) and threading/authorship, drops body and metadata.
func TestAddComment_LeanByDefault(t *testing.T) {
	taskID := uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cm-1", "task_id": taskID, "body": "the whole body echoed back",
			"author_id": "ag-1", "author_name": "Somebody", "author_type": "agent",
			"is_internal": false, "created_at": "2026-10-03T12:00:00Z",
			"metadata": map[string]any{"x": "y"},
			"delivery": []any{map[string]any{"handle": "@bob", "reached": true}},
		})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID, "body": "the whole body echoed back"}

	res, herr := server.handleAddComment(ctx, req)
	m := decodeResultMap(t, mustOK(t, res, herr))
	for _, k := range []string{"id", "task_id", "author_id", "created_at", "delivery"} {
		if _, ok := m[k]; !ok {
			t.Errorf("lean comment missing %q", k)
		}
	}
	for _, k := range []string{"body", "metadata"} {
		if _, ok := m[k]; ok {
			t.Errorf("lean comment must not echo %q", k)
		}
	}
}

func TestAddComment_FullReturnsVerbatimComment(t *testing.T) {
	taskID := uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "cm-1", "task_id": taskID, "body": "the whole body echoed back",
			"created_at": "2026-10-03T12:00:00Z",
		})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID, "body": "the whole body echoed back", "full": true}

	res, herr := server.handleAddComment(ctx, req)
	m := decodeResultMap(t, mustOK(t, res, herr))
	if m["body"] != "the whole body echoed back" {
		t.Error("full=true must return the comment verbatim, body included")
	}
}

// move_task keeps its reload (the post-move assignee — auto-reassign on
// review — is otherwise unknowable: the move endpoint answers only
// {"status":"ok"}), but projects the reloaded card to the lean shape instead
// of echoing it whole.
func TestMoveTask_LeanByDefault(t *testing.T) {
	taskID := uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "st-1", "slug": "done", "name": "Done", "category": "done"},
			})
		case strings.HasSuffix(r.URL.Path, "/move"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		case strings.HasSuffix(r.URL.Path, "/comments"):
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		default:
			_ = json.NewEncoder(w).Encode(fullTaskFixture(taskID))
		}
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())
	req := buildMoveTaskRequest(taskID, "done")

	res, herr := server.handleMoveTask(ctx, req)
	m := decodeResultMap(t, mustOK(t, res, herr))
	task, ok := m["task"].(map[string]any)
	if !ok {
		t.Fatalf("move result has no task object: %#v", m["task"])
	}
	assertLeanTaskShape(t, task)
	ns, ok := m["new_status"].(map[string]any)
	if !ok || ns["slug"] != "done" {
		t.Errorf("new_status = %#v, want slug=done (auto-reassign/status context must survive)", m["new_status"])
	}
}

func TestMoveTask_FullReturnsVerbatimTask(t *testing.T) {
	taskID := uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": "st-1", "slug": "done", "name": "Done", "category": "done"},
			})
		case strings.HasSuffix(r.URL.Path, "/move"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		default:
			_ = json.NewEncoder(w).Encode(fullTaskFixture(taskID))
		}
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())
	req := buildMoveTaskRequest(taskID, "done")
	req.Params.Arguments.(map[string]any)["full"] = true

	res, herr := server.handleMoveTask(ctx, req)
	m := decodeResultMap(t, mustOK(t, res, herr))
	task, ok := m["task"].(map[string]any)
	if !ok {
		t.Fatalf("move result has no task object: %#v", m["task"])
	}
	if _, ok := task["description"]; !ok {
		t.Error("full=true must return the reloaded task verbatim")
	}
}

func mustOK(t *testing.T, result *mcpsdk.CallToolResult, err error) *mcpsdk.CallToolResult {
	t.Helper()
	if err != nil {
		t.Fatalf("handler transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handler returned error result: %s", resultText(t, result))
	}
	return result
}
