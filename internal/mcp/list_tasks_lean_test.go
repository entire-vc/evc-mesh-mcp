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
// Full mode preserves this long description; compact mode omits it.
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
		if strings.HasSuffix(r.URL.Path, "/statuses") {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "st-1", "slug": "todo"}})
			return
		}
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

// Compact mode retains only the documented routing fields.
func TestHandleListTasks_Default_CompactFields(t *testing.T) {
	projectID := uuid.New().String()
	server := listTasksHarness(t, "/api/v1/projects/"+projectID+"/tasks", listTasksEnvelopeFixture())
	out := callListTasks(t, server, map[string]any{"project_id": projectID})
	if out["total_count"] != float64(1) || out["page"] != float64(1) {
		t.Fatalf("page envelope changed: %v", out)
	}
	item := listTasksItem(t, out)
	want := map[string]any{"id": "56b00943-cdbb-4598-b1e2-de928e82661c", "title": "T", "status": "todo", "priority": "medium", "labels": []any{"mcp"}, "assignee_name": "Linus", "updated_at": "2026-10-03T11:18:59Z"}
	if !reflect.DeepEqual(item, want) {
		t.Fatalf("compact fields: got %v, want %v", item, want)
	}
	b, _ := json.Marshal(item)
	if len(b) > 250 {
		t.Errorf("minimal compact row is %d bytes, budget250", len(b))
	}
}

// Details outside the compact contract remain available through full=true.
func TestHandleListTasks_Default_OmitsDetails(t *testing.T) {
	fx := listTasksEnvelopeFixture()
	src := fx["items"].([]any)[0].(map[string]any)
	for _, k := range []string{"due_date", "artifact_count", "vcs_link_count", "estimated_hours", "human_gate"} {
		src[k] = "set"
	}
	projectID := uuid.New().String()
	server := listTasksHarness(t, "/api/v1/projects/"+projectID+"/tasks", fx)
	item := listTasksItem(t, callListTasks(t, server, map[string]any{"project_id": projectID}))
	for _, k := range []string{"due_date", "artifact_count", "vcs_link_count", "estimated_hours", "human_gate"} {
		if _, ok := item[k]; ok {
			t.Errorf("detail %q leaked into compact row", k)
		}
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

// Workspace compact rows retain their project identity.
func TestHandleListTasks_WorkspaceSearch_Compact(t *testing.T) {
	wsID := uuid.New().String()
	server := listTasksHarness(t, "/api/v1/workspaces/"+wsID+"/tasks", listTasksEnvelopeFixture())
	item := listTasksItem(t, callListTasks(t, server, map[string]any{"workspace_id": wsID, "search": "reconnect"}))
	if item["project_id"] != "p-1" || item["status"] != "todo" {
		t.Fatalf("workspace routing fields: %v", item)
	}
	for _, k := range []string{"description", "description_truncated", "has_description", "status_id", "url"} {
		if _, ok := item[k]; ok {
			t.Errorf("detail %q leaked", k)
		}
	}
}
