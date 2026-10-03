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

// listTasksEnvelopeFixture is one item shaped like the live REST list_tasks
// response, carrying every envelope field the lean default view must drop.
// The description reuses longTaskDescription (>200 chars, short first line)
// from the get_my_tasks trim tests: the same item shape, the same trim rule.
func listTasksEnvelopeFixture() map[string]any {
	item := map[string]any{
		"id": "56b00943-cdbb-4598-b1e2-de928e82661c", "title": "T", "status_id": "st-1",
		"priority": "medium", "labels": []any{"mcp"}, "assignee_name": "Linus",
		"due_date": nil, "updated_at": "2026-10-03T11:18:59Z", "project_id": "p-1",
		"description": longTaskDescription, "has_description": true, "human_gate": false,
		"url": "https://mesh.entire.host/t/x", "parent_task_id": nil,
		"assignee_id": "a-1", "assignee_type": "agent", "assigned_by": "",
		"created_by": "a-2", "created_by_name": "Garfield", "created_by_type": "agent",
		"created_at": "2026-10-03T11:18:59Z", "completed_at": nil,
		"artifact_count": float64(0), "vcs_link_count": float64(0), "completion_signal": false,
		"delegation_level": "auto", "position": float64(0), "human_gate_class": "hard",
		"is_shipped": false, "custom_fields": map[string]any{}, "dod_checks": map[string]any{},
		"estimated_hours": nil, "start_after": nil, "subtask_count": float64(0),
	}
	return map[string]any{
		"items": []any{item}, "total_count": 1, "page": float64(1), "total_pages": float64(1),
	}
}

// listTasksHarness serves the fixture on whichever REST route the handler
// takes: the project listing (/api/v1/projects/<id>/tasks) or the workspace
// search (/api/v1/workspaces/<id>/tasks) — the lean view must hold on both.
func listTasksHarness(t *testing.T, route string, fixture map[string]any) *Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != route {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fixture)
	}))
	t.Cleanup(srv.Close)
	return &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
}

func callListTasks(t *testing.T, server *Server, args map[string]any) map[string]any {
	t.Helper()
	result, err := server.handleListTasks(context.Background(), requestWith(args))
	if err != nil {
		t.Fatalf("handleListTasks returned error: %v", err)
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

func listTasksItem(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %v", out["items"])
	}
	item, ok := items[0].(map[string]any)
	if !ok {
		t.Fatalf("item 0 is not an object: %T", items[0])
	}
	return item
}

// TestListTasksTool_ExposesFullParam pins the schema half: without a declared
// `full` field an agent cannot ask for the old view at all (undeclared
// arguments are ignored), and the lean default would be the only view.
func TestListTasksTool_ExposesFullParam(t *testing.T) {
	server := NewServer(ServerConfig{})

	tool, ok := server.MCPServer().ListTools()["list_tasks"]
	if !ok {
		t.Fatal("list_tasks tool is not registered")
	}
	props := tool.Tool.InputSchema.Properties

	raw, present := props["full"]
	if !present {
		t.Fatal("list_tasks must declare a full parameter, otherwise the pre-lean view is unreachable")
	}
	spec, _ := raw.(map[string]any)
	if kind, _ := spec["type"].(string); kind != "boolean" {
		t.Errorf("full must be a boolean, got %q", kind)
	}
	if desc, _ := spec["description"].(string); strings.TrimSpace(desc) == "" {
		t.Error("full needs a description: an undocumented parameter is one nobody passes")
	}
	for _, req := range tool.Tool.InputSchema.Required {
		if req == "full" {
			t.Error("full must stay optional — the lean view is the default, not a requirement")
		}
	}
}

// TestHandleListTasks_Default_TrimsDescriptionAndStripsEnvelope is the primary
// acceptance case: no `full` param → the multi-paragraph description collapses
// to its first line with a truncation marker, every envelope field the card
// names is gone, and the routing fields (id/title/status_id/priority/labels/
// assignee_name/updated_at/project_id/has_description) survive untouched.
// Red on the pre-2026-10-03 handler, which returned the REST page verbatim.
func TestHandleListTasks_Default_TrimsDescriptionAndStripsEnvelope(t *testing.T) {
	projectID := uuid.New().String()
	server := listTasksHarness(t, "/api/v1/projects/"+projectID+"/tasks",
		listTasksEnvelopeFixture())
	out := callListTasks(t, server, map[string]any{"project_id": projectID})

	if out["total_count"] != float64(1) || out["page"] != float64(1) {
		t.Fatalf("page envelope (total_count/page/total_pages) must not change: %v", out)
	}

	item := listTasksItem(t, out)
	if got := item["description"]; got != "Fix the retry backoff in the SSE reconnect path." {
		t.Fatalf("expected description trimmed to its first line, got: %q", got)
	}
	if item["description_truncated"] != true {
		t.Fatalf("trimmed item must carry description_truncated=true: %v", item)
	}
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

// TestHandleListTasks_Default_KeepsRealValues: the value-conditional drops
// (due_date, counts, estimate, custom_fields, armed gate) hide only their
// empty forms — a set deadline, a nonzero artifact/vcs count (the done-evidence
// gate reads vcs links) and a real estimate must stay. Same semantics as
// get_my_tasks: zero-count fields vanish from live pages, nonzero ones are
// routing signal, and both survive the lean view.
func TestHandleListTasks_Default_KeepsRealValues(t *testing.T) {
	fx := listTasksEnvelopeFixture()
	src := fx["items"].([]any)[0].(map[string]any)
	src["due_date"] = "2026-10-10T12:00:00Z"
	src["artifact_count"] = float64(2)
	src["vcs_link_count"] = float64(1)
	src["estimated_hours"] = float64(3)
	src["human_gate"] = true
	projectID := uuid.New().String()
	server := listTasksHarness(t, "/api/v1/projects/"+projectID+"/tasks", fx)
	item := listTasksItem(t, callListTasks(t, server, map[string]any{"project_id": projectID}))
	for k := range map[string]bool{"due_date": true, "artifact_count": true,
		"vcs_link_count": true, "estimated_hours": true, "human_gate": true} {
		if _, ok := item[k]; !ok {
			t.Errorf("real value of %q was dropped by the lean view: %v", k, item)
		}
	}
	if item["due_date"] != "2026-10-10T12:00:00Z" {
		t.Fatalf("a set due_date must survive the lean view, got: %v", item["due_date"])
	}
}

// TestHandleListTasks_FullTrue_ReturnsItemVerbatim is the backward-compat
// escape hatch: full=true must return the fixture item VERBATIM — keys AND
// values, not a key count — exactly what the pre-lean handler returned.
func TestHandleListTasks_FullTrue_ReturnsItemVerbatim(t *testing.T) {
	fx := listTasksEnvelopeFixture()
	want := fx["items"].([]any)[0].(map[string]any)
	projectID := uuid.New().String()
	server := listTasksHarness(t, "/api/v1/projects/"+projectID+"/tasks", fx)
	got := listTasksItem(t, callListTasks(t, server, map[string]any{"project_id": projectID, "full": true}))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("full=true must return the fixture item verbatim:\n got: %v\nwant: %v", got, want)
	}
	if jsonHasKeyAnywhere(t, got, "description_truncated") {
		t.Fatalf("full=true response must not carry description_truncated markers: %v", got)
	}
}

// TestHandleListTasks_WorkspaceSearch_SameLeanView: the workspace_id search
// exit goes through the same lean view — both list_tasks exits share the
// items shape, so they must share the trim, or a global search would still
// hand back 3k-char descriptions per item.
func TestHandleListTasks_WorkspaceSearch_SameLeanView(t *testing.T) {
	wsID := uuid.New().String()
	server := listTasksHarness(t, "/api/v1/workspaces/"+wsID+"/tasks",
		listTasksEnvelopeFixture())
	item := listTasksItem(t, callListTasks(t, server, map[string]any{
		"workspace_id": wsID, "search": "reconnect",
	}))
	if got := item["description"]; got != "Fix the retry backoff in the SSE reconnect path." {
		t.Fatalf("workspace search must trim descriptions too, got: %q", got)
	}
	if _, ok := item["url"]; ok {
		t.Errorf("workspace search must strip envelope fields too: %v", item)
	}
}
