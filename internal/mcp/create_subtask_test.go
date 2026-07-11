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

func buildCreateSubtaskRequest(parentTaskID, title, statusSlug string) mcpsdk.CallToolRequest {
	args := map[string]any{
		"parent_task_id": parentTaskID,
		"title":          title,
	}
	if statusSlug != "" {
		args["status_slug"] = statusSlug
	}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

// TestHandleCreateSubtask_StatusSlugOverride verifies that an explicit status_slug
// is resolved against the PARENT's project and threaded through as status_id on
// the POST body — never left for the REST API to fall back to the project default.
// Regression coverage for #60921c52 (explicit status_slug silently ignored).
func TestHandleCreateSubtask_StatusSlugOverride(t *testing.T) {
	parentID := uuid.New().String()
	projectID := uuid.New().String()
	inProgressID := uuid.New().String()
	todoID := uuid.New().String()

	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+parentID:
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         parentID,
				"project_id": projectID,
				"status_id":  inProgressID, // parent is in_progress — must not leak into the child
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID+"/statuses":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": todoID, "slug": "todo", "name": "Todo", "category": "todo"},
				{"id": inProgressID, "slug": "in_progress", "name": "In Progress", "category": "in_progress"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/"+parentID+"/subtasks":
			if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         uuid.New().String(),
				"project_id": projectID,
				"status_id":  capturedBody["status_id"],
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
	}
	ctx := context.Background()

	result, err := server.handleCreateSubtask(ctx, buildCreateSubtaskRequest(parentID, "Child task", "in_progress"))
	if err != nil {
		t.Fatalf("handleCreateSubtask returned error: %v", err)
	}
	if result != nil && result.IsError {
		t.Fatalf("handleCreateSubtask returned tool error: %v", result.Content)
	}

	if capturedBody["status_id"] != inProgressID {
		t.Errorf("POST /subtasks body status_id = %v, want %s (in_progress) — explicit status_slug override was not threaded through", capturedBody["status_id"], inProgressID)
	}

	out := decodeToolResultJSON(t, result)
	if out["status_id"] != inProgressID {
		t.Errorf("returned subtask status_id = %v, want %s", out["status_id"], inProgressID)
	}
}

// TestHandleCreateSubtask_NoStatusSlug verifies that omitting status_slug leaves
// status_id out of the POST body entirely, letting the REST API apply the
// project's default status rather than the MCP layer guessing one.
func TestHandleCreateSubtask_NoStatusSlug(t *testing.T) {
	parentID := uuid.New().String()
	projectID := uuid.New().String()

	var capturedBody map[string]any
	var sawStatusID bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/"+parentID+"/subtasks":
			if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			_, sawStatusID = capturedBody["status_id"]
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         uuid.New().String(),
				"project_id": projectID,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
	}
	ctx := context.Background()

	result, err := server.handleCreateSubtask(ctx, buildCreateSubtaskRequest(parentID, "Child task", ""))
	if err != nil {
		t.Fatalf("handleCreateSubtask returned error: %v", err)
	}
	if result != nil && result.IsError {
		t.Fatalf("handleCreateSubtask returned tool error: %v", result.Content)
	}

	if sawStatusID {
		t.Errorf("POST /subtasks body should omit status_id when status_slug is not provided, got %v", capturedBody["status_id"])
	}
}
