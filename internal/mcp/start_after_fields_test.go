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

// start_after mirrors due_date field-for-field in all three task-creation/update
// tools (create_task, update_task, create_subtask): the API accepted the field
// (evc-mesh #246b8fcc) before any of these three MCP tools carried a parameter to
// set it — the class this repo calls create_task_status_param_silently_ignored,
// except here the parameter was never in the schema at all, so a caller could not
// even attempt it. These tests pin that all three now forward it, validate it the
// same way due_date is validated, and leave it untouched when omitted.

// captureCreateTaskBody spins up a fake API, runs handleCreateTask, and returns the
// JSON body POSTed to /api/v1/projects/:id/tasks — asserting on the wire body, not
// the tool result, is what tells "forwarded" from "silently dropped".
func captureCreateTaskBody(t *testing.T, args map[string]any) (map[string]any, *mcpsdk.CallToolResult) {
	t.Helper()
	projectID, _ := args["project_id"].(string)
	var captured map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/tasks" {
			_ = json.NewDecoder(r.Body).Decode(&captured)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": uuid.New().String(), "project_id": projectID})
	}))
	t.Cleanup(srv.Close)

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args

	ctx := withTestSession(context.Background(), server, uuid.New())
	result, err := server.handleCreateTask(ctx, req)
	if err != nil {
		t.Fatalf("handleCreateTask returned error: %v", err)
	}
	return captured, result
}

func TestCreateTask_ForwardsStartAfter(t *testing.T) {
	body, result := captureCreateTaskBody(t, map[string]any{
		"project_id":  uuid.New().String(),
		"title":       "Scheduled retry",
		"start_after": "2026-09-15T00:00:00Z",
	})
	if result.IsError {
		t.Fatalf("unexpected error result: %+v", result.Content)
	}
	if got := body["start_after"]; got != "2026-09-15T00:00:00Z" {
		t.Errorf("start_after not forwarded: got %v (body: %v)", got, body)
	}
}

func TestCreateTask_RejectsMalformedStartAfter(t *testing.T) {
	body, result := captureCreateTaskBody(t, map[string]any{
		"project_id":  uuid.New().String(),
		"title":       "Bad date",
		"start_after": "15 September 2026",
	})
	if !result.IsError {
		t.Fatal("a malformed start_after must be rejected by the tool")
	}
	if body != nil {
		t.Errorf("no request should have been sent, but body was %v", body)
	}
}

// due_date must keep working unchanged alongside the new field — the two are
// independent, and adding start_after must not disturb the existing param.
func TestCreateTask_DueDateAndStartAfterAreIndependent(t *testing.T) {
	body, result := captureCreateTaskBody(t, map[string]any{
		"project_id":  uuid.New().String(),
		"title":       "Both dates",
		"due_date":    "2026-09-20T00:00:00Z",
		"start_after": "2026-09-15T00:00:00Z",
	})
	if result.IsError {
		t.Fatalf("unexpected error result: %+v", result.Content)
	}
	if got := body["due_date"]; got != "2026-09-20T00:00:00Z" {
		t.Errorf("due_date not forwarded: %v", got)
	}
	if got := body["start_after"]; got != "2026-09-15T00:00:00Z" {
		t.Errorf("start_after not forwarded: %v", got)
	}
}

// --- update_task --------------------------------------------------------------

func TestUpdateTask_ForwardsStartAfter(t *testing.T) {
	body := captureUpdateBody(t, map[string]any{
		"task_id": uuid.New().String(), "start_after": "2026-09-15T00:00:00Z",
	})
	if got := body["start_after"]; got != "2026-09-15T00:00:00Z" {
		t.Errorf("start_after not forwarded: %v (body %v)", got, body)
	}
}

func TestUpdateTask_RejectsMalformedStartAfter(t *testing.T) {
	taskID := uuid.New().String()
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/tasks/"+taskID {
			reached = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID, "start_after": "not a date"}

	result, err := server.handleUpdateTask(context.Background(), req)
	if err != nil {
		t.Fatalf("handleUpdateTask: %v", err)
	}
	if !result.IsError {
		t.Error("a malformed start_after must be rejected by the tool")
	}
	if reached {
		t.Error("no request should have been sent for a malformed start_after")
	}
}

// TestUpdateTask_OmittedStartAfterIsNotSent extends the existing PATCH-semantics
// guard (TestUpdateTask_OmittedFieldsAreNotSent) to the new field: inventing a
// value here would overwrite real data on every unrelated update.
func TestUpdateTask_OmittedStartAfterIsNotSent(t *testing.T) {
	body := captureUpdateBody(t, map[string]any{
		"task_id": uuid.New().String(), "title": "only the title",
	})
	if _, present := body["start_after"]; present {
		t.Errorf("start_after was sent although the caller never supplied it: %v", body)
	}
}

// --- create_subtask -------------------------------------------------------------

func TestCreateSubtask_ForwardsStartAfter(t *testing.T) {
	parentID := uuid.New().String()

	body, result := captureSubtaskBody(t, map[string]any{
		"parent_task_id": parentID,
		"title":          "Scheduled subtask",
		"start_after":    "2026-09-15T00:00:00Z",
	})
	if result.IsError {
		t.Fatalf("unexpected error result: %+v", result.Content)
	}
	if got := body["start_after"]; got != "2026-09-15T00:00:00Z" {
		t.Errorf("start_after not forwarded: got %v (body: %v)", got, body)
	}
}

func TestCreateSubtask_RejectsMalformedStartAfter(t *testing.T) {
	parentID := uuid.New().String()

	body, result := captureSubtaskBody(t, map[string]any{
		"parent_task_id": parentID,
		"title":          "Bad date",
		"start_after":    "15 September 2026",
	})
	if !result.IsError {
		t.Fatal("a malformed start_after must be rejected by the tool")
	}
	if body != nil {
		t.Errorf("no request should have been sent, but body was %v", body)
	}
}

// --- schema advertises the field --------------------------------------------

func TestStartAfter_SchemaAdvertisedOnAllThreeTools(t *testing.T) {
	server := NewServer(ServerConfig{})
	tools := server.MCPServer().ListTools()
	for _, name := range []string{"create_task", "update_task", "create_subtask"} {
		tool, ok := tools[name]
		if !ok {
			t.Fatalf("%s tool is not registered", name)
		}
		if _, present := tool.Tool.InputSchema.Properties["start_after"]; !present {
			t.Errorf("%s schema must advertise start_after, otherwise no caller can send it", name)
		}
	}
}
