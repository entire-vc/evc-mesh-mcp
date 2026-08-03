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

// captureSubtaskBody spins up a fake API, runs handleCreateSubtask with the given
// arguments, and returns the JSON body the handler actually POSTed.
//
// Asserting on the forwarded body rather than on the tool's result is the whole
// point: the API answers 201 whether or not a field survived the handler, so a
// result-only assertion cannot tell "forwarded" from "silently dropped" — which is
// exactly how assignee_id went missing unnoticed.
func captureSubtaskBody(t *testing.T, args map[string]any) (map[string]any, *mcpsdk.CallToolResult) {
	t.Helper()
	parentID, _ := args["parent_task_id"].(string)
	projectID := uuid.New().String()

	var captured map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+parentID+"/statuses":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				{"id": uuid.New().String(), "slug": "todo", "name": "Todo", "category": "todo"},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/"+parentID+"/subtasks":
			_ = json.NewDecoder(r.Body).Decode(&captured)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": uuid.New().String(), "project_id": projectID})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args

	result, err := server.handleCreateSubtask(context.Background(), req)
	if err != nil {
		t.Fatalf("handleCreateSubtask returned error: %v", err)
	}
	return captured, result
}

// TestCreateSubtask_ForwardsAssignee is the regression test for the reported bug:
// a caller-supplied assignee_id must reach the API instead of being discarded.
//
// This matters beyond a missing field. We have a working convention that a subtask
// is owned by whoever will do the work, not by whoever split the parent task up.
// While this argument was dropped, following that convention produced precisely the
// outcome it prevents — every subtask owned by the splitter — with no error to notice.
func TestCreateSubtask_ForwardsAssignee(t *testing.T) {
	parentID := uuid.New().String()
	assignee := uuid.New().String()

	body, result := captureSubtaskBody(t, map[string]any{
		"parent_task_id": parentID,
		"title":          "Owned by the doer, not the splitter",
		"assignee_id":    assignee,
		"assignee_type":  "agent",
	})
	if result.IsError {
		t.Fatalf("unexpected error result: %+v", result.Content)
	}
	if got := body["assignee_id"]; got != assignee {
		t.Errorf("assignee_id not forwarded: got %v, want %v (body: %v)", got, assignee, body)
	}
	if got := body["assignee_type"]; got != "agent" {
		t.Errorf("assignee_type not forwarded: got %v, want agent", got)
	}
}

// TestCreateSubtask_AssigneeTypeDefaultsToAgent covers the common call shape where a
// caller passes an agent id and omits the type.
func TestCreateSubtask_AssigneeTypeDefaultsToAgent(t *testing.T) {
	parentID := uuid.New().String()
	assignee := uuid.New().String()

	body, _ := captureSubtaskBody(t, map[string]any{
		"parent_task_id": parentID,
		"title":          "No explicit type",
		"assignee_id":    assignee,
	})
	if got := body["assignee_type"]; got != "agent" {
		t.Errorf("assignee_type should default to agent alongside an id, got %v", got)
	}
}

// TestCreateSubtask_NoAssigneeSendsNoAssigneeType guards the inverse. Emitting a bare
// assignee_type with no id would let the API type an unassigned subtask as an agent —
// trading a dropped field for a wrong one.
func TestCreateSubtask_NoAssigneeSendsNoAssigneeType(t *testing.T) {
	parentID := uuid.New().String()

	body, _ := captureSubtaskBody(t, map[string]any{
		"parent_task_id": parentID,
		"title":          "Unassigned subtask",
	})
	if _, present := body["assignee_id"]; present {
		t.Errorf("assignee_id must be absent when not supplied, got %v", body["assignee_id"])
	}
	if _, present := body["assignee_type"]; present {
		t.Errorf("assignee_type must not be sent without an assignee_id, got %v", body["assignee_type"])
	}
}

// TestCreateSubtask_ForwardsRemainingRestFields pins the other five fields the REST
// endpoint accepts and create_task already exposed. They were absent for the same
// reason as assignee_id, so fixing only the reported one would leave the identical
// defect in place for every other field.
func TestCreateSubtask_ForwardsRemainingRestFields(t *testing.T) {
	parentID := uuid.New().String()

	body, result := captureSubtaskBody(t, map[string]any{
		"parent_task_id":  parentID,
		"title":           "Everything the endpoint accepts",
		"description":     "desc",
		"labels":          []any{"mesh", "backend"},
		"due_date":        "2026-08-10T12:00:00Z",
		"estimated_hours": 3.5,
		"custom_fields":   map[string]any{"team": "mesh"},
	})
	if result.IsError {
		t.Fatalf("unexpected error result: %+v", result.Content)
	}

	if got, _ := body["labels"].([]any); len(got) != 2 {
		t.Errorf("labels not forwarded: %v", body["labels"])
	}
	if got := body["due_date"]; got != "2026-08-10T12:00:00Z" {
		t.Errorf("due_date not forwarded: %v", got)
	}
	if got, _ := body["estimated_hours"].(float64); got != 3.5 {
		t.Errorf("estimated_hours not forwarded: %v", body["estimated_hours"])
	}
	if cf, _ := body["custom_fields"].(map[string]any); cf["team"] != "mesh" {
		t.Errorf("custom_fields not forwarded: %v", body["custom_fields"])
	}
	if got := body["description"]; got != "desc" {
		t.Errorf("description not forwarded: %v", got)
	}
}

// TestCreateSubtask_RejectsMalformedDueDate keeps the validation create_task performs,
// so a bad timestamp fails at the tool rather than reaching the API as garbage.
func TestCreateSubtask_RejectsMalformedDueDate(t *testing.T) {
	parentID := uuid.New().String()

	body, result := captureSubtaskBody(t, map[string]any{
		"parent_task_id": parentID,
		"title":          "Bad date",
		"due_date":       "10 August 2026",
	})
	if !result.IsError {
		t.Fatal("a malformed due_date must be rejected by the tool")
	}
	if body != nil {
		t.Errorf("no request should have been sent, but body was %v", body)
	}
}
