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

// A synthetic marker, never a live capability. Failures report only field names.
const leaseOutputMarker = "synthetic-lease-output-marker"

func assertNoLeaseCapability(t *testing.T, result *mcpsdk.CallToolResult) {
	t.Helper()
	if result == nil || result.IsError {
		t.Fatal("expected successful tool result")
	}
	wire, err := json.Marshal(result) // Includes content AND structuredContent.
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(wire), leaseOutputMarker) {
		t.Fatal("serialized tool result contains lease capability")
	}
}

func leaseOutputFixture(id string) map[string]any {
	return map[string]any{
		"id": id, "title": "lease fixture", "version": 1,
		"checkout_token": leaseOutputMarker, "checked_out_by": "owner",
		"checkout_generation": 7, "checkout_expires": "2099-01-01T00:00:00Z",
		"checkout_session_id": "session", "checkout_state": "active",
	}
}

func TestLeaseOutputNestedAndTyped(t *testing.T) {
	type typedTask struct {
		Token      string `json:"checkout_token"`
		Generation int64  `json:"checkout_generation"`
	}
	input := map[string]any{
		"task":        leaseOutputFixture("task"),
		"nested":      []any{map[string]any{"previous_instance": typedTask{leaseOutputMarker, 9007199254740993}}},
		"raw":         json.RawMessage(`{"checkout_token":"synthetic-lease-output-marker","checkout_state":"active"}`),
		"lease_token": leaseOutputMarker, "fencing_token": leaseOutputMarker,
		"body": "ordinary prose mentioning checkout_token", "tokens_in": 42,
	}
	result, err := jsonResult(input)
	if err != nil {
		t.Fatal(err)
	}
	assertNoLeaseCapability(t, result)
	text := resultText(t, result)
	for _, expected := range []string{"9007199254740993", "ordinary prose mentioning checkout_token", `"tokens_in":42`, `"checkout_state":"active"`, `"checked_out_by":"owner"`} {
		if !strings.Contains(text, expected) {
			t.Fatal("non-capability field changed")
		}
	}
	if input["task"].(map[string]any)["checkout_token"] != leaseOutputMarker {
		t.Fatal("output filtering mutated internal REST object")
	}
}

func TestLeaseOutputReadTools(t *testing.T) {
	id, project, schedule := uuid.NewString(), uuid.NewString(), uuid.NewString()
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fixture := leaseOutputFixture(id)
		var out any
		switch r.URL.Path {
		case "/api/v1/tasks/" + id:
			out = fixture
		case "/api/v1/tasks/" + id + "/context":
			out = map[string]any{"task": fixture, "dependencies": []any{map[string]any{"task": fixture}}, "subtasks": []any{fixture}}
		case "/api/v1/agents/me/tasks", "/api/v1/agents/me/tasks/poll":
			out = map[string]any{"tasks": []any{fixture}, "count": 1, "total_count": 1, "has_more": false}
		case "/api/v1/projects/" + project + "/tasks", "/api/v1/recurring/" + schedule + "/history":
			out = map[string]any{"items": []any{fixture}, "total_count": 1}
		case "/api/v1/projects/" + project + "/events":
			out = map[string]any{"items": []any{map[string]any{"payload": map[string]any{"task": fixture}}}}
		default:
			out = map[string]any{"items": []any{}}
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer api.Close()
	s := &Server{restClient: NewRESTClient(api.URL, "test-key"), session: &AgentSession{AgentID: uuid.New()}, tracker: NewSessionTracker()}
	tests := []struct {
		name    string
		handler func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error)
		args    map[string]any
	}{
		{"get_task", s.handleGetTask, map[string]any{"task_id": id}},
		{"get_task/full", s.handleGetTask, map[string]any{"task_id": id, "full": true}},
		{"get_task_context", s.handleGetTaskContext, map[string]any{"task_id": id}},
		{"list_tasks", s.handleListTasks, map[string]any{"project_id": project}},
		{"list_tasks/full", s.handleListTasks, map[string]any{"project_id": project, "full": true}},
		{"get_my_tasks", s.handleGetMyTasks, map[string]any{}},
		{"get_my_tasks/full", s.handleGetMyTasks, map[string]any{"full": true}},
		{"poll_tasks", s.handlePollTasks, map[string]any{"timeout": 1}},
		{"poll_tasks/full", s.handlePollTasks, map[string]any{"timeout": 1, "full": true}},
		{"get_recurring_history", s.handleGetRecurringHistory, map[string]any{"recurring_schedule_id": schedule}},
		{"get_context", s.handleGetContext, map[string]any{"project_id": project}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var req mcpsdk.CallToolRequest
			req.Params.Arguments = tt.args
			result, err := tt.handler(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			assertNoLeaseCapability(t, result)
			if tt.name == "get_task/full" {
				task := decodeToolResultJSON(t, result)["task"].(map[string]any)
				for _, key := range []string{"checked_out_by", "checkout_generation", "checkout_expires", "checkout_session_id", "checkout_state"} {
					if task[key] != leaseOutputFixture(id)[key] && key != "checkout_generation" {
						t.Fatalf("metadata missing: %s", key)
					}
				}
				if task["checkout_generation"] != float64(7) {
					t.Fatal("generation changed")
				}
			}
		})
	}
}

func TestLeaseOutputSnapshotDelta(t *testing.T) {
	fixture := revisionFixture()
	item := fixture["tasks"].([]any)[0].(map[string]any)
	item["checkout_token"] = leaseOutputMarker
	s := myTasksHarness(t, fixture)
	cold := callGetMyTasks(t, s, map[string]any{})
	if _, ok := cold["tasks"].([]any)[0].(map[string]any)["checkout_token"]; ok {
		t.Fatal("cold contains capability field")
	}
	revision := revisionOf(t, cold)
	item["version"], item["title"] = 2, "changed"
	delta := callGetMyTasks(t, s, map[string]any{"known_revision": revision, "accept_delta": true})
	if delta["delta"] != true {
		t.Fatal("expected delta")
	}
	if _, ok := delta["changed"].([]any)[0].(map[string]any)["checkout_token"]; ok {
		t.Fatal("delta contains capability field")
	}
	if item["checkout_token"] != leaseOutputMarker {
		t.Fatal("snapshot input mutated")
	}
}
