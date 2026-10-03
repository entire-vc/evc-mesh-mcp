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

// TestHandleGetMyTasks_Default_TrimsLongDescriptions is the primary
// acceptance check: no `full` param → a >200-char multi-paragraph description
// collapses to its first line with a truncation marker, while routing fields
// (id/title/status/priority/labels/assignee) survive untouched.
func TestHandleGetMyTasks_Default_TrimsLongDescriptions(t *testing.T) {
	server := myTasksHarness(t, myTasksFixture())
	out := callGetMyTasks(t, server, map[string]any{})

	if out["count"] != float64(4) || out["total_count"] != float64(4) {
		t.Fatalf("top-level count/total_count must not change: %v", out)
	}

	item := myTasksItem(t, out, 0)
	if got := item["description"]; got != "Fix the retry backoff in the SSE reconnect path." {
		t.Fatalf("expected description trimmed to its first line, got: %q", got)
	}
	if item["description_truncated"] != true {
		t.Fatalf("trimmed item must carry description_truncated=true: %v", item)
	}
	if item["has_description"] != true {
		t.Fatalf("has_description must stay true on a trimmed item: %v", item)
	}
	for _, key := range []string{"id", "title", "status_id", "priority", "labels", "assignee_name", "assignee_id", "assignee_type"} {
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

// TestHandleGetMyTasks_Default_RuneSafeCut checks the Cyrillic case: the cut
// at 200 chars is by runes, so a 300-rune first line yields exactly 200 valid
// runes, never a byte-sliced mojibake tail.
func TestHandleGetMyTasks_Default_RuneSafeCut(t *testing.T) {
	server := myTasksHarness(t, myTasksFixture())
	out := callGetMyTasks(t, server, map[string]any{})

	item := myTasksItem(t, out, 2)
	desc, _ := item["description"].(string)
	if n := len([]rune(desc)); n != 200 {
		t.Fatalf("expected exactly 200 runes after the cut, got %d: %q", n, desc)
	}
	if strings.ContainsRune(desc, 0xFFFD) {
		t.Fatalf("rune-unsafe cut produced replacement characters: %q", desc)
	}
	if item["description_truncated"] != true {
		t.Fatalf("cut item must carry description_truncated=true: %v", item)
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
