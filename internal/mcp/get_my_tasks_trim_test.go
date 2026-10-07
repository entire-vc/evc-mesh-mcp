package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// longTaskDescription is >200 chars with a short first line, the shape that
// made a 50-task get_my_tasks page measure ~162k chars live (2026-10-03,
// task a183d6f5): every item carried its full multi-paragraph description.
const longTaskDescription = "Fix the retry backoff in the SSE reconnect path.\n" +
	"Body: the reconnect loop resets the backoff on every streamed keepalive, so a flapping " +
	"endpoint hammers the server. Repro: kill the NATS connection mid-stream, watch the " +
	"reconnect interval stay at 1s. Fix in internal/sse/reconnect.go, add a test pinning the " +
	"exponential schedule, and check the fleet log for the reconnect storm signature afterwards."

// cyrillicFirstLineLong has a first line of 300 Cyrillic runes: cutting it at
// 200 must be rune-safe, not byte-safe (a byte cut mid-rune yields mojibake).
var cyrillicFirstLineLong = strings.Repeat("ф", 300) + "\nsecond line"

// myTasksFixture mirrors the live /api/v1/agents/me/tasks response shape
// (top-level count/total_count/has_more/tasks) with the four description
// cases the trimmer must distinguish.
func myTasksFixture() map[string]any {
	return map[string]any{
		"count":       4,
		"total_count": 4,
		"has_more":    false,
		"tasks": []any{
			map[string]any{
				"id":              "task-1",
				"title":           "Fix SSE reconnect backoff",
				"status_id":       "st-1",
				"priority":        "high",
				"labels":          []any{"backend", "sse"},
				"assignee_name":   "Linus",
				"assignee_id":     "agent-1",
				"assignee_type":   "agent",
				"description":     longTaskDescription,
				"has_description": true,
			},
			map[string]any{
				"id":              "task-2",
				"title":           "Short one-liner",
				"status_id":       "st-1",
				"priority":        "medium",
				"labels":          []any{"chore"},
				"assignee_name":   "Linus",
				"description":     "Already compact: fits in one short line.",
				"has_description": true,
			},
			map[string]any{
				"id":              "task-3",
				"title":           "Cyrillic long first line",
				"status_id":       "st-1",
				"priority":        "low",
				"assignee_name":   "Linus",
				"description":     cyrillicFirstLineLong,
				"has_description": true,
			},
			map[string]any{
				"id":              "task-4",
				"title":           "No description (server-blanked page)",
				"status_id":       "st-1",
				"priority":        "none",
				"assignee_name":   "Linus",
				"description":     "",
				"has_description": false,
			},
		},
	}
}

func myTasksHarness(t *testing.T, fixture map[string]any) *Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fixture)
	}))
	t.Cleanup(srv.Close)
	return &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
}

func callGetMyTasks(t *testing.T, server *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	result, err := server.handleGetMyTasks(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetMyTasks returned error: %v", err)
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
	return decoded
}

func myTasksItem(t *testing.T, out map[string]any, idx int) map[string]any {
	t.Helper()
	tasks, ok := out["tasks"].([]any)
	if !ok || len(tasks) != 4 {
		t.Fatalf("expected 4 tasks in response, got %v", out["tasks"])
	}
	item, ok := tasks[idx].(map[string]any)
	if !ok {
		t.Fatalf("task %d is not an object: %T", idx, tasks[idx])
	}
	return item
}

// Complete default descriptions retain every paragraph and routing field.
func TestHandleGetMyTasks_Default_PreservesLongDescriptions(t *testing.T) {
	server := myTasksHarness(t, myTasksFixture())
	out := callGetMyTasks(t, server, map[string]any{})

	if out["count"] != float64(4) || out["total_count"] != float64(4) {
		t.Fatalf("top-level count/total_count must not change: %v", out)
	}

	item := myTasksItem(t, out, 0)
	if got := item["description"]; got != longTaskDescription {
		t.Fatalf("expected complete description, got: %q", got)
	}
	if item["description_truncated"] == true {
		t.Fatalf("complete item must not carry description_truncated=true: %v", item)
	}
	if item["has_description"] != true {
		t.Fatalf("has_description must stay true on a trimmed item: %v", item)
	}
	for _, key := range []string{"id", "title", "status_id", "priority", "labels", "assignee_name"} {
		if _, ok := item[key]; !ok {
			t.Fatalf("routing field %q was dropped from the trimmed item: %v", key, item)
		}
	}
	if item["id"] != "task-1" || item["title"] != "Fix SSE reconnect backoff" || item["priority"] != "high" {
		t.Fatalf("routing field values changed: %v", item)
	}

	// A description that is already one short line must pass through verbatim,
	// with no truncation marker invented for it.
	short := myTasksItem(t, out, 1)
	if got := short["description"]; got != "Already compact: fits in one short line." {
		t.Fatalf("short description must be untouched, got: %q", got)
	}
	if _, hasMarker := short["description_truncated"]; hasMarker {
		t.Fatalf("short description must not carry description_truncated: %v", short)
	}
}

// Default Unicode descriptions retain all original runes and newlines.
func TestHandleGetMyTasks_Default_PreservesUnicode(t *testing.T) {
	server := myTasksHarness(t, myTasksFixture())
	out := callGetMyTasks(t, server, map[string]any{})

	item := myTasksItem(t, out, 2)
	desc, _ := item["description"].(string)
	if n := len([]rune(desc)); n != len([]rune(cyrillicFirstLineLong)) {
		t.Fatalf("expected every original Unicode rune, got %d: %q", n, desc)
	}
	if strings.ContainsRune(desc, 0xFFFD) {
		t.Fatalf("rune-unsafe cut produced replacement characters: %q", desc)
	}
	if item["description_truncated"] == true {
		t.Fatalf("complete item must not carry description_truncated=true: %v", item)
	}
}

// TestHandleGetMyTasks_Default_LeavesEmptyDescriptionsAlone pins the
// interaction with the server-side page trimmer: an item whose description
// was already blanked (empty string, has_description reflecting real content)
// is passed through untouched — the MCP layer must not resurrect or re-mark it.
func TestHandleGetMyTasks_Default_LeavesEmptyDescriptionsAlone(t *testing.T) {
	server := myTasksHarness(t, myTasksFixture())
	out := callGetMyTasks(t, server, map[string]any{})

	item := myTasksItem(t, out, 3)
	if got := item["description"]; got != "" {
		t.Fatalf("empty description must stay empty, got: %q", got)
	}
	if item["has_description"] != false {
		t.Fatalf("has_description must keep the server's value (false): %v", item)
	}
	if _, hasMarker := item["description_truncated"]; hasMarker {
		t.Fatalf("empty description must not carry description_truncated: %v", item)
	}
}

// TestHandleGetMyTasks_FullTrue_ReturnsFullDescriptions is the backward-compat
// escape hatch: full=true must return each description exactly as the REST
// API gave it, with no truncation markers anywhere in the response.
func TestHandleGetMyTasks_FullTrue_ReturnsFullDescriptions(t *testing.T) {
	server := myTasksHarness(t, myTasksFixture())
	out := callGetMyTasks(t, server, map[string]any{"full": true})

	item := myTasksItem(t, out, 0)
	if got := item["description"]; got != longTaskDescription {
		t.Fatalf("full=true must return the description verbatim, got: %q", got)
	}
	long := myTasksItem(t, out, 2)
	if got, _ := long["description"].(string); got != cyrillicFirstLineLong {
		t.Fatalf("full=true must return the Cyrillic description verbatim, got: %q", got)
	}
	if jsonHasKeyAnywhere(t, out, "description_truncated") {
		t.Fatalf("full=true response must not carry description_truncated markers: %v", out)
	}
}

// leanEnvelopeFixture is one item shaped like the live REST response, with
// every envelope field the lean default view must drop.
func leanEnvelopeFixture() map[string]any {
	item := map[string]any{
		"id": "82b388a0-81b9-4296-a3d5-614149865a75", "title": "T", "status_id": "st-1",
		"priority": "medium", "labels": []any{"mcp"}, "assignee_name": "Garfield",
		"due_date": nil, "updated_at": "2026-10-03T00:51:35Z", "project_id": "p-1",
		"description": "d", "has_description": true, "human_gate": false,
		"url": "https://mesh.entire.host/t/x", "parent_task_id": nil,
		"assignee_id": "a-1", "assignee_type": "agent", "assigned_by": "",
		"created_by": "a-2", "created_by_name": "Linus", "created_by_type": "agent",
		"created_at": "2026-09-24T00:18:46Z", "completed_at": nil,
		"artifact_count": float64(0), "vcs_link_count": float64(0), "completion_signal": false,
		"delegation_level": "auto", "position": float64(0), "human_gate_class": "hard",
		"is_shipped": false, "custom_fields": map[string]any{}, "dod_checks": map[string]any{},
		"estimated_hours": nil, "start_after": nil, "subtask_count": float64(0),
	}
	return map[string]any{"count": 1, "total_count": 1, "has_more": false, "tasks": []any{item}}
}

func leanItem(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	tasks, _ := out["tasks"].([]any)
	if len(tasks) != 1 {
		t.Fatalf("expected 1 task, got %v", out["tasks"])
	}
	return tasks[0].(map[string]any)
}

// TestHandleGetMyTasks_Default_LeanEnvelope: the default view omits the
// envelope fields but keeps every routing field; red before leanTaskSummaries.
func TestHandleGetMyTasks_Default_LeanEnvelope(t *testing.T) {
	out := callGetMyTasks(t, myTasksHarness(t, leanEnvelopeFixture()), map[string]any{})
	item := leanItem(t, out)
	// due_date/human_gate ride the value-conditional list: nil/false serialize
	// to nothing (live pages carried 49-50 nulls), a real value stays.
	for _, k := range []string{
		"url", "parent_task_id", "assignee_id", "assignee_type", "assigned_by", "created_by",
		"created_by_name", "created_by_type", "created_at", "completed_at", "artifact_count",
		"vcs_link_count", "completion_signal", "delegation_level", "position", "human_gate_class",
		"is_shipped", "custom_fields", "dod_checks", "estimated_hours", "start_after", "subtask_count",
		"due_date", "human_gate",
	} {
		if _, ok := item[k]; ok {
			t.Errorf("lean default view must not carry %q: %v", k, item)
		}
	}
	for _, k := range []string{"id", "title", "status_id", "priority", "labels", "assignee_name", "updated_at", "project_id", "has_description"} {
		if _, ok := item[k]; !ok {
			t.Errorf("routing field %q dropped from lean view: %v", k, item)
		}
	}
	b, _ := json.Marshal(item)
	if len(b) > 600 {
		t.Errorf("lean item is %d chars, budget 600: %s", len(b), b)
	}
}

// TestHandleGetMyTasks_Default_LeanKeepsRealValues: empty-only drops must not
// hide a real estimate, subtask count, custom field, deadline or armed gate.
func TestHandleGetMyTasks_Default_LeanKeepsRealValues(t *testing.T) {
	fx := leanEnvelopeFixture()
	item := fx["tasks"].([]any)[0].(map[string]any)
	item["estimated_hours"] = float64(3)
	item["subtask_count"] = float64(2)
	item["custom_fields"] = map[string]any{"k": "v"}
	item["artifact_count"] = float64(1)
	item["vcs_link_count"] = float64(2)
	item["completion_signal"] = true
	item["due_date"] = "2026-10-10T12:00:00Z"
	item["human_gate"] = true
	got := leanItem(t, callGetMyTasks(t, myTasksHarness(t, fx), map[string]any{}))
	for _, k := range []string{"estimated_hours", "subtask_count", "custom_fields", "artifact_count", "vcs_link_count", "completion_signal", "due_date", "human_gate"} {
		if _, ok := got[k]; !ok {
			t.Errorf("real value of %q was dropped: %v", k, got)
		}
	}
}

// TestHandleGetMyTasks_FullTrue_KeepsEnvelope: full=true is the old shape —
// the fixture item verbatim, keys AND values. A key-count check alone would
// let a dropped-plus-added field slip through (codex-review P2, 2026-10-03).
func TestHandleGetMyTasks_FullTrue_KeepsEnvelope(t *testing.T) {
	fx := leanEnvelopeFixture()
	want := fx["tasks"].([]any)[0].(map[string]any)
	got := leanItem(t, callGetMyTasks(t, myTasksHarness(t, fx), map[string]any{"full": true}))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("full=true must return the fixture item verbatim:\n got: %v\nwant: %v", got, want)
	}
}
