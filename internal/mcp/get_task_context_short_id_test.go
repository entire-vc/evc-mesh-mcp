package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// callGetTaskContext invokes handleGetTaskContext and decodes its JSON payload,
// failing the test on any tool error — the get_task_context counterpart of
// callGetTask.
func callGetTaskContext(t *testing.T, server *Server, args map[string]any) (*mcpsdk.CallToolResult, map[string]any) {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	result, err := server.handleGetTaskContext(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetTaskContext returned error: %v", err)
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
	return result, decoded
}

// TestHandleGetTaskContext_ShortIDMatchesUUID is the core acceptance: a
// short-ID get_task_context and a full-UUID get_task_context must return
// identical data, recurring-history enrichment included. Red on the old code:
// the short-ID call dies with the live error from #e143d391 ("failed to get
// task context: Bad Request: invalid task_id") because the /tasks/:id/context
// route parses the path id as a strict UUID, while the UUID call succeeds.
func TestHandleGetTaskContext_ShortIDMatchesUUID(t *testing.T) {
	fx := newShortIDFixture()
	fx.scheduleID = uuid.New().String()
	server, _ := getTaskShortIDHarness(t, fx, nil)

	_, byUUID := callGetTaskContext(t, server, map[string]any{"task_id": fx.fullID})
	_, byShort := callGetTaskContext(t, server, map[string]any{"task_id": fx.shortID})

	if !reflect.DeepEqual(byUUID, byShort) {
		t.Fatalf("short-ID response differs from UUID response:\nUUID:  %#v\nshort: %#v", byUUID, byShort)
	}

	// The comparison above only means something if the response actually
	// carries the bundled collections and the recurring enrichment — assert
	// presence, not just equality of two equally-empty maps.
	for _, key := range []string{"task", "comments", "artifacts", "dependencies", "activity", "recurring"} {
		if _, present := byShort[key]; !present {
			t.Errorf("response is missing %q: %#v", key, byShort)
		}
	}
	if task, ok := byShort["task"].(map[string]any); !ok || task["id"] != fx.fullID {
		t.Errorf("task.id = %#v, want the resolved full UUID %s", byShort["task"], fx.fullID)
	}
	recurring, _ := byShort["recurring"].(map[string]any)
	if recurring["schedule_id"] != fx.scheduleID {
		t.Errorf("recurring.schedule_id = %#v, want %s", byShort["recurring"], fx.scheduleID)
	}
	prev, _ := recurring["previous_instance"].(map[string]any)
	if prev == nil || prev["summary"] != "previous instance" {
		t.Errorf("recurring.previous_instance = %#v, want the instance-1 history item", byShort["recurring"])
	}
}

// TestHandleGetTaskContext_ShortIDUsesResolvedUUIDOnWire ratchets the fix on
// the wire: after the by-short-id resolution, the /context read must hit the
// resolved full-UUID path (never the raw prefix), and the recurring-history
// enrichment must read the schedule by its own id.
func TestHandleGetTaskContext_ShortIDUsesResolvedUUIDOnWire(t *testing.T) {
	fx := newShortIDFixture()
	fx.scheduleID = uuid.New().String()
	server, recordedPaths := getTaskShortIDHarness(t, fx, nil)

	_, decoded := callGetTaskContext(t, server, map[string]any{"task_id": fx.shortID})
	if task, ok := decoded["task"].(map[string]any); !ok || task["id"] != fx.fullID {
		t.Fatalf("task.id = %#v, want the resolved full UUID %s", decoded["task"], fx.fullID)
	}

	wantSeen := map[string]bool{
		"GET /api/v1/tasks/by-short-id/" + fx.shortID:         false,
		"GET /api/v1/tasks/" + fx.fullID + "/context":         false,
		"GET /api/v1/recurring/" + fx.scheduleID + "/history": false,
	}
	for _, p := range recordedPaths() {
		if strings.Contains(p, "/"+fx.shortID+"/context") {
			t.Errorf("context read used the raw short id instead of the resolved UUID: %s", p)
		}
		if _, ok := wantSeen[p]; ok {
			wantSeen[p] = true
		}
	}
	for p, seen := range wantSeen {
		if !seen {
			t.Errorf("expected request never fired: %s (recorded: %v)", p, recordedPaths())
		}
	}
}

// TestHandleGetTaskContext_ShortIDUnknownPrefixIsToolError controls the
// failure paths of resolution itself: a valid hex prefix that matches nothing
// (404) and one that matches several tasks (ambiguous, 400) must each come
// back as a normal tool error carrying the resolver's OWN verdict — and the
// /context read must never fire: a prefix that resolves to nothing reads
// nobody's task. Both controls are VALID 6-hex prefixes per the tool contract,
// so they exercise the resolver's decision, not input validation.
func TestHandleGetTaskContext_ShortIDUnknownPrefixIsToolError(t *testing.T) {
	fx := newShortIDFixture()
	server, recordedPaths := getTaskShortIDHarness(t, fx, func(short string) (map[string]any, int) {
		if short == "abcdef" {
			return map[string]any{"code": 400, "message": "ambiguous task id: 2 matches"}, http.StatusBadRequest
		}
		return map[string]any{"code": 404, "message": "task not found"}, http.StatusNotFound
	})

	// Each prefix must surface its OWN resolver verdict, not one generic
	// error: deadbe resolves nothing (404 not found), abcdef matches two
	// tasks (400 ambiguous). Same control as its get_task counterpart in
	// get_task_short_id_test.go.
	wantSubstring := map[string]string{
		"deadbe": "not found",
		"abcdef": "ambiguous",
	}
	for _, short := range []string{"deadbe", "abcdef"} {
		req := mcpsdk.CallToolRequest{}
		req.Params.Arguments = map[string]any{"task_id": short}
		result, err := server.handleGetTaskContext(context.Background(), req)
		if err != nil {
			t.Fatalf("handleGetTaskContext(%q) returned a Go error instead of a tool error: %v", short, err)
		}
		if !result.IsError {
			t.Fatalf("get_task_context(%q) succeeded (%#v), want a tool error", short, result.Content)
		}
		text := resultText(t, result)
		if want := wantSubstring[short]; !strings.Contains(text, want) {
			t.Errorf("get_task_context(%q) error = %q, want the resolver's own verdict containing %q", short, text, want)
		}
	}

	for _, p := range recordedPaths() {
		if strings.Contains(p, "/context") {
			t.Errorf("context read fired for an unresolvable prefix: %s", p)
		}
	}
}
