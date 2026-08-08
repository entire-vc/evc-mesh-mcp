package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// routeRecorder records which API paths a handler actually reached.
type routeRecorder struct {
	mu    sync.Mutex
	paths []string
}

func (r *routeRecorder) record(path string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.paths = append(r.paths, path)
}

func (r *routeRecorder) hit(path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, p := range r.paths {
		if p == path {
			return true
		}
	}
	return false
}

func buildMoveTaskRequest(taskID, statusSlug string) mcpsdk.CallToolRequest {
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"task_id":     taskID,
		"status_slug": statusSlug,
	}
	return req
}

// statusFixture is the status list both routes serve in these tests.
func statusFixture(todoID, doneID string) []map[string]any {
	return []map[string]any{
		{"id": todoID, "slug": "todo", "name": "Todo", "category": "todo"},
		{"id": doneID, "slug": "done", "name": "Done", "category": "done"},
	}
}

// TestMoveTask_ResolvesSlugViaTaskScopedRoute pins the client half of the gate-parity
// fix: move_task must resolve its status slug through GET /tasks/:task_id/statuses,
// which carries the same workspace gate as the move, and must not touch the
// project-gated route at all when that succeeds.
//
// Asserting only "the move worked" would not distinguish the two routes, since both
// return the same shape — so the test asserts which door was used, not just the outcome.
func TestMoveTask_ResolvesSlugViaTaskScopedRoute(t *testing.T) {
	taskID := uuid.New().String()
	projectID := uuid.New().String()
	todoID, doneID := uuid.New().String(), uuid.New().String()

	rec := &routeRecorder{}
	var moveBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+taskID+"/statuses":
			_ = json.NewEncoder(w).Encode(statusFixture(todoID, doneID))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/"+taskID+"/move":
			_ = json.NewDecoder(r.Body).Decode(&moveBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+taskID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID, "project_id": projectID, "status_id": doneID})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())

	result, err := server.handleMoveTask(ctx, buildMoveTaskRequest(taskID, "done"))
	if err != nil {
		t.Fatalf("handleMoveTask returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handleMoveTask returned an error result: %+v", result.Content)
	}

	if !rec.hit("/api/v1/tasks/" + taskID + "/statuses") {
		t.Errorf("move_task did not read the task-scoped status route; paths hit: %v", rec.paths)
	}
	if rec.hit("/api/v1/projects/" + projectID + "/statuses") {
		t.Errorf("move_task read the project-gated status route even though the task-scoped "+
			"one answered — that is the gate mismatch this change removes; paths hit: %v", rec.paths)
	}
	if got := moveBody["status_id"]; got != doneID {
		t.Errorf("move body carried status_id %v, want the resolved done id %v", got, doneID)
	}
}

// TestMoveTask_DoesNotFallBackOnForbidden is the important negative control on the
// fallback. A 403 means the server refused this caller; it must surface, not be
// retried through the project-scoped route — that route is strictly more restrictive,
// so retrying there cannot succeed where this failed, and would only restore the
// original confusing "not a member of this project" error.
//
// Put generally: "I could not look" must never be converted into "I looked somewhere else".
func TestMoveTask_DoesNotFallBackOnForbidden(t *testing.T) {
	taskID := uuid.New().String()
	projectID := uuid.New().String()

	rec := &routeRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+taskID+"/statuses":
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 403, "message": "workspace access denied"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+taskID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID, "project_id": projectID})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())

	result, err := server.handleMoveTask(ctx, buildMoveTaskRequest(taskID, "done"))
	if err != nil {
		t.Fatalf("handleMoveTask returned a transport error: %v", err)
	}
	if !result.IsError {
		t.Fatal("a 403 on the status lookup must surface as an error result, not be papered over")
	}
	if rec.hit("/api/v1/projects/" + projectID + "/statuses") {
		t.Errorf("a 403 was retried against the project-gated route; paths hit: %v", rec.paths)
	}
}

// TestMoveTask_FallsBackToProjectRouteOn404 covers the one case the fallback is for:
// a server that predates GET /tasks/:task_id/statuses. Without it, deploying this
// client ahead of the API would break every move_task at once, so the change must be
// safe in either deploy order.
func TestMoveTask_FallsBackToProjectRouteOn404(t *testing.T) {
	taskID := uuid.New().String()
	projectID := uuid.New().String()
	todoID, doneID := uuid.New().String(), uuid.New().String()

	rec := &routeRecorder{}
	var moveBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		// The task-scoped route is deliberately absent — an older server.
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+taskID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID, "project_id": projectID})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID+"/statuses":
			_ = json.NewEncoder(w).Encode(statusFixture(todoID, doneID))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/"+taskID+"/move":
			_ = json.NewDecoder(r.Body).Decode(&moveBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())

	result, err := server.handleMoveTask(ctx, buildMoveTaskRequest(taskID, "done"))
	if err != nil {
		t.Fatalf("handleMoveTask returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("fallback path failed: %+v", result.Content)
	}
	if !rec.hit("/api/v1/projects/" + projectID + "/statuses") {
		t.Errorf("expected the project-scoped fallback to be used; paths hit: %v", rec.paths)
	}
	if got := moveBody["status_id"]; got != doneID {
		t.Errorf("move body carried status_id %v, want %v", got, doneID)
	}
}

// TestCreateSubtask_ResolvesSlugViaTaskScopedRoute checks the second call site.
// POST /tasks/:task_id/subtasks is workspace-gated exactly like the move, so fixing
// only move_task would have left an identical dead end reachable through the standard
// decomposition route.
func TestCreateSubtask_ResolvesSlugViaTaskScopedRoute(t *testing.T) {
	parentID := uuid.New().String()
	projectID := uuid.New().String()
	todoID, doneID := uuid.New().String(), uuid.New().String()

	rec := &routeRecorder{}
	var createBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+parentID+"/statuses":
			_ = json.NewEncoder(w).Encode(statusFixture(todoID, doneID))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/"+parentID+"/subtasks":
			_ = json.NewDecoder(r.Body).Decode(&createBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": uuid.New().String(), "project_id": projectID})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	ctx := context.Background()

	result, err := server.handleCreateSubtask(ctx, buildCreateSubtaskRequest(parentID, "Child", "todo"))
	if err != nil {
		t.Fatalf("handleCreateSubtask returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handleCreateSubtask returned an error result: %+v", result.Content)
	}

	if !rec.hit("/api/v1/tasks/" + parentID + "/statuses") {
		t.Errorf("create_subtask did not read the task-scoped status route; paths hit: %v", rec.paths)
	}
	if rec.hit("/api/v1/projects/" + projectID + "/statuses") {
		t.Errorf("create_subtask read the project-gated status route; paths hit: %v", rec.paths)
	}
	if got := createBody["status_id"]; got != todoID {
		t.Errorf("subtask body carried status_id %v, want %v", got, todoID)
	}
}

// TestCreateTask_StillResolvesViaProjectRoute pins what must NOT change. create_task
// writes to POST /projects/:proj_id/tasks, which is itself project-gated, so its slug
// lookup is already gate-matched and has no reason to move. The rule is "match the
// gate of the write you serve", not "always prefer the task route".
func TestCreateTask_StillResolvesViaProjectRoute(t *testing.T) {
	projectID := uuid.New().String()
	todoID, doneID := uuid.New().String(), uuid.New().String()

	rec := &routeRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec.record(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID+"/statuses":
			_ = json.NewEncoder(w).Encode(statusFixture(todoID, doneID))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": uuid.New().String(), "project_id": projectID})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project_id":  projectID,
		"title":       "A task",
		"status_slug": "todo",
	}

	result, err := server.handleCreateTask(ctx, req)
	if err != nil {
		t.Fatalf("handleCreateTask returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("handleCreateTask returned an error result: %+v", result.Content)
	}
	if !rec.hit("/api/v1/projects/" + projectID + "/statuses") {
		t.Errorf("create_task must keep resolving against its own project route; paths hit: %v", rec.paths)
	}
}

// TestAPIError_PreservesMessageFormat guards the refactor that made the status code
// available to callers. The typed error must render exactly as the fmt.Errorf it
// replaced, or callers matching on error text — including tests elsewhere in this
// package — would break silently.
func TestAPIError_PreservesMessageFormat(t *testing.T) {
	err := &APIError{StatusCode: http.StatusForbidden, Message: "agent is not a member of this project"}
	want := "Forbidden: agent is not a member of this project"
	if got := err.Error(); got != want {
		t.Errorf("APIError.Error() = %q, want %q", got, want)
	}
	if !strings.Contains(err.Error(), "Forbidden") {
		t.Errorf("status text dropped from %q", err.Error())
	}
}
