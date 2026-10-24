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
	"exponential schedule, and check the team log for the reconnect storm signature afterwards."

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
				"assignee_name":   "Alice",
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
				"assignee_name":   "Alice",
				"description":     "Already compact: fits in one short line.",
				"has_description": true,
			},
			map[string]any{
				"id":              "task-3",
				"title":           "Cyrillic long first line",
				"status_id":       "st-1",
				"priority":        "low",
				"assignee_name":   "Alice",
				"description":     cyrillicFirstLineLong,
				"has_description": true,
			},
			map[string]any{
				"id":              "task-4",
				"title":           "No description (server-blanked page)",
				"status_id":       "st-1",
				"priority":        "none",
				"assignee_name":   "Alice",
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
	client := NewRESTClient(srv.URL, "test-key")
	client.SetForwardedOrigin("https://mesh.example.invalid")
	return &Server{
		restClient: client,
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

// Compact cold: descriptions are absent (not silently cut), flagged per item
// and in the envelope; routing fields stay.
func TestHandleGetMyTasks_Default_OmitsDescriptions(t *testing.T) {
	server := myTasksHarness(t, myTasksFixture())
	out := callGetMyTasks(t, server, map[string]any{})

	if out["count"] != float64(4) || out["total_count"] != float64(4) {
		t.Fatalf("top-level count/total_count must not change: %v", out)
	}

	item := myTasksItem(t, out, 0)
	if _, has := item["description"]; has {
		t.Fatalf("compact cold must not carry description: %v", item)
	}
	if jsonHasKeyAnywhere(t, out, "description_truncated") {
		t.Fatalf("absence must not be reported as truncation: %v", out)
	}
	if item["has_description"] != true {
		t.Fatalf("has_description must be true on an item whose text was omitted: %v", item)
	}
	if note, _ := out["description_omitted"].(string); !strings.Contains(note, "get_task") || !strings.Contains(note, "full=true") {
		t.Fatalf("envelope must say how to get descriptions: %v", out["description_omitted"])
	}
	for _, key := range []string{"id", "title", "status_id", "priority", "labels", "assignee_name"} {
		if _, ok := item[key]; !ok {
			t.Fatalf("routing field %q was dropped from the trimmed item: %v", key, item)
		}
	}
	if item["id"] != "task-1" || item["title"] != "Fix SSE reconnect backoff" || item["priority"] != "high" {
		t.Fatalf("routing field values changed: %v", item)
	}

	short := myTasksItem(t, out, 1)
	if _, has := short["description"]; has {
		t.Fatalf("short description must be omitted too: %v", short)
	}
}

// The rollback view is the previous lean envelope: no url either.
func TestHandleGetMyTasks_ColdFullRollback_PreviousLeanShape(t *testing.T) {
	t.Setenv(envMyTasksCold, "full")
	item := leanItem(t, callGetMyTasks(t, myTasksHarness(t, leanEnvelopeFixture()), map[string]any{}))
	for _, k := range []string{"url", "parent_task_id"} {
		if _, ok := item[k]; ok {
			t.Errorf("rollback must keep the previous lean shape without %q: %v", k, item)
		}
	}
}

// Rollback switch: MESH_MCP_MY_TASKS_COLD=full returns the previous cold
// envelope with complete descriptions.
func TestHandleGetMyTasks_ColdFullRollback(t *testing.T) {
	t.Setenv(envMyTasksCold, "full")
	out := callGetMyTasks(t, myTasksHarness(t, myTasksFixture()), map[string]any{})
	if got := myTasksItem(t, out, 0)["description"]; got != longTaskDescription {
		t.Fatalf("rollback must restore complete descriptions, got: %q", got)
	}
	if _, has := out["description_omitted"]; has {
		t.Fatalf("rollback must not carry the omission note: %v", out)
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
	if _, has := item["description"]; has {
		t.Fatalf("compact cold must not carry description: %v", item)
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
		"id": "550e8400-e29b-41d4-a716-446655440000", "title": "T", "status_id": "st-1",
		"priority": "medium", "labels": []any{"mcp"}, "assignee_name": "Bob",
		"due_date": nil, "updated_at": "2026-10-03T00:51:35Z", "project_id": "p-1",
		"description": "d", "has_description": true, "human_gate": false,
		"url": "https://mesh.example.invalid/t/x", "parent_task_id": nil,
		"assignee_id": "a-1", "assignee_type": "agent", "assigned_by": "",
		"created_by": "a-2", "created_by_name": "Alice", "created_by_type": "agent",
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
	// url is rebuilt from id (see TestHandleGetMyTasks_Cold_PerCardBudget); a url
	// that does not follow the <prefix><id> rule must still survive.
	if item["url"] != "https://mesh.example.invalid/t/x" {
		t.Errorf("non-derivable url must stay in the compact line: %v", item)
	}
	for _, k := range []string{
		"parent_task_id", "assignee_id", "assignee_type", "assigned_by", "created_by",
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

// liveShapedColdPage is 50 cards as the live REST page returns them after the
// lean pass: UUID id/project/status, url == prefix+id, default checkout_generation
// and null vcs_pipeline_status (measured 2026-10-20, task 6371c9e4: 28.4k chars).
func liveShapedColdPage(n int) map[string]any {
	tasks := make([]any, 0, n)
	for i := 0; i < n; i++ {
		id := uuid.NewString()
		tasks = append(tasks, map[string]any{
			"id": id, "title": "[Mesh MCP] get_my_tasks: компактный cold — разбор по полям",
			"status_id": uuid.NewString(), "project_id": uuid.NewString(), "priority": "high",
			"labels": []any{"mesh", "mcp"}, "assignee_name": "Linus", "has_description": true,
			"description": "d", "updated_at": "2026-10-20T12:01:19.064215Z", "version": float64(3),
			"url": "https://mesh.example.invalid/t/" + id, "checkout_generation": float64(0),
			"vcs_pipeline_status": nil,
		})
	}
	return map[string]any{"count": n, "total_count": n, "has_more": false, "tasks": tasks}
}

// Cold per-card budget: red when url/default fields ride on every card.
func TestHandleGetMyTasks_Cold_PerCardBudget(t *testing.T) {
	const n, budget = 50, 420
	out := callGetMyTasks(t, myTasksHarness(t, liveShapedColdPage(n)), map[string]any{})
	b, _ := json.Marshal(out["tasks"])
	if per := len(b) / n; per > budget {
		t.Errorf("cold card is %d chars, budget %d: %s", per, budget, b[:min(len(b), 600)])
	}
	if out["url_template"] != "https://mesh.example.invalid/t/{id}" {
		t.Errorf("url_template missing: %v", out["url_template"])
	}
	item := out["tasks"].([]any)[0].(map[string]any)
	for _, k := range []string{"url", "checkout_generation", "vcs_pipeline_status"} {
		if _, ok := item[k]; ok {
			t.Errorf("%q must be omitted when derivable/default: %v", k, item)
		}
	}
}

// A real value is never hidden by the default-drop.
func TestHandleGetMyTasks_Cold_KeepsRealCheckoutAndPipeline(t *testing.T) {
	fx := liveShapedColdPage(1)
	it := fx["tasks"].([]any)[0].(map[string]any)
	it["checkout_generation"] = float64(2)
	it["vcs_pipeline_status"] = "failed"
	got := leanItem(t, callGetMyTasks(t, myTasksHarness(t, fx), map[string]any{}))
	if got["checkout_generation"] != float64(2) || got["vcs_pipeline_status"] != "failed" {
		t.Errorf("real values dropped: %v", got)
	}
}

// A page where one card has no derivable url gets no template at all, so a
// reader never rebuilds a wrong link (codex-review P2 on !182).
func TestHandleGetMyTasks_Cold_MixedUrlPageHasNoTemplate(t *testing.T) {
	for name, mutate := range map[string]func(map[string]any){
		"missing": func(m map[string]any) { delete(m, "url") },
		"foreign": func(m map[string]any) { m["url"] = "https://other.example/x" },
		"bare-id": func(m map[string]any) { m["url"] = m["id"] },
	} {
		fx := liveShapedColdPage(2)
		mutate(fx["tasks"].([]any)[0].(map[string]any))
		out := callGetMyTasks(t, myTasksHarness(t, fx), map[string]any{})
		if _, ok := out["url_template"]; ok {
			t.Errorf("%s: template emitted for a mixed page: %v", name, out["url_template"])
		}
		other := out["tasks"].([]any)[1].(map[string]any)
		if _, ok := other["url"]; !ok {
			t.Errorf("%s: derivable url dropped without a template: %v", name, other)
		}
	}
}

// A page where every url is <foreign-host>/<id> is not rebuilt from a template:
// only the Mesh task prefix is hoisted, so the urls stay on the cards (A1, !182).
func TestHandleGetMyTasks_Cold_ForeignHostPageHasNoTemplate(t *testing.T) {
	const host = "https://other.example/t/"
	fx := liveShapedColdPage(3)
	for _, v := range fx["tasks"].([]any) {
		m := v.(map[string]any)
		m["url"] = host + m["id"].(string)
	}
	out := callGetMyTasks(t, myTasksHarness(t, fx), map[string]any{})
	if tpl, ok := out["url_template"]; ok {
		t.Errorf("foreign-host page got url_template %v", tpl)
	}
	for _, v := range out["tasks"].([]any) {
		m := v.(map[string]any)
		if got, _ := m["url"].(string); got != host+m["id"].(string) {
			t.Errorf("foreign url not kept intact: %v", m)
		}
	}
}
