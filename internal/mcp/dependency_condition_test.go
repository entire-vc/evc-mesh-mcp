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

// fakeDependencyAPI serves the three endpoints the dependency tools touch and
// records the bodies of the POST and PATCH.
func fakeDependencyAPI(t *testing.T, blocked, blocker, depID string) (*Server, map[string]map[string]any) {
	t.Helper()
	bodies := map[string]map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/"+blocked+"/dependencies":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			bodies["post"] = b
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": depID, "warning": "blocker is labelled kind:monitor"})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/tasks/"+blocked+"/dependencies/"+depID:
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			bodies["patch"] = b
			_ = json.NewEncoder(w).Encode(map[string]any{"id": depID, "condition_satisfied": true})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+blocked+"/dependencies":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"outgoing": []map[string]any{{"id": depID, "depends_on_task_id": blocker}},
				"incoming": []map[string]any{},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+blocked:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": blocked})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}, bodies
}

func TestAddDependency_ForwardsConditionAndEvidence(t *testing.T) {
	blocked, blocker, depID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	server, bodies := fakeDependencyAPI(t, blocked, blocker, depID)

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"task_id": blocked, "depends_on_task_id": blocker,
		"condition": "access_ready", "evidence_url": "https://mesh.example/e",
	}
	res, err := server.handleAddDependency(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("add_dependency failed: %v %+v", err, res)
	}
	if bodies["post"]["condition"] != "access_ready" || bodies["post"]["evidence_url"] != "https://mesh.example/e" {
		t.Errorf("condition/evidence not forwarded: %v", bodies["post"])
	}

	// Without them the body is the old one: the server defaults to task_done.
	req.Params.Arguments = map[string]any{"task_id": blocked, "depends_on_task_id": blocker}
	if _, err := server.handleAddDependency(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if _, ok := bodies["post"]["condition"]; ok {
		t.Errorf("condition must be omitted when not given: %v", bodies["post"])
	}
}

func TestUpdateDependency_PatchesTheDirectionalEdge(t *testing.T) {
	blocked, blocker, depID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	server, bodies := fakeDependencyAPI(t, blocked, blocker, depID)

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": blocked, "depends_on_task_id": blocker, "evidence_url": "https://mesh.example/e"}
	res, err := server.handleUpdateDependency(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("update_dependency failed: %v %+v", err, res)
	}
	if bodies["patch"]["evidence_url"] != "https://mesh.example/e" {
		t.Errorf("evidence not patched: %v", bodies["patch"])
	}
	if _, ok := bodies["patch"]["condition"]; ok {
		t.Errorf("an omitted condition must stay omitted: %v", bodies["patch"])
	}

	// Wrong direction: the blocker has no outgoing edge to the blocked card.
	req.Params.Arguments = map[string]any{"task_id": blocked, "depends_on_task_id": uuid.NewString(), "condition": "deployed"}
	res, _ = server.handleUpdateDependency(context.Background(), req)
	if !res.IsError {
		t.Error("an edge that does not exist must be an error")
	}
	req.Params.Arguments = map[string]any{"task_id": blocked, "depends_on_task_id": blocker}
	res, _ = server.handleUpdateDependency(context.Background(), req)
	if !res.IsError {
		t.Error("an update with nothing to change must be an error")
	}
}

// The tools return the server's answer untouched, so the caller sees `warning`
// after a blocks edge onto a monitor and `condition_satisfied` after evidence.
func TestDependencyTools_ReturnServerFieldsVerbatim(t *testing.T) {
	blocked, blocker, depID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	server, bodies := fakeDependencyAPI(t, blocked, blocker, depID)

	text := func(res *mcpsdk.CallToolResult) string {
		t.Helper()
		if res.IsError || len(res.Content) == 0 {
			t.Fatalf("unexpected tool error: %+v", res)
		}
		return res.Content[0].(mcpsdk.TextContent).Text
	}

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": blocked, "depends_on_task_id": blocker}
	res, err := server.handleAddDependency(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var added map[string]any
	if err := json.Unmarshal([]byte(text(res)), &added); err != nil {
		t.Fatal(err)
	}
	if added["warning"] != "blocker is labelled kind:monitor" {
		t.Errorf("add_dependency dropped the server warning: %v", added)
	}

	// An empty evidence_url is sent as given: it re-arms the edge.
	req.Params.Arguments = map[string]any{"task_id": blocked, "depends_on_task_id": blocker, "evidence_url": ""}
	res, err = server.handleUpdateDependency(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if v, ok := bodies["patch"]["evidence_url"]; !ok || v != "" {
		t.Errorf("empty evidence_url must be forwarded to clear the edge: %v", bodies["patch"])
	}
	var updated map[string]any
	if err := json.Unmarshal([]byte(text(res)), &updated); err != nil {
		t.Fatal(err)
	}
	if updated["condition_satisfied"] != true {
		t.Errorf("update_dependency dropped condition_satisfied: %v", updated)
	}
}

// Clients may apply schema defaults before calling a tool: a default on
// condition would turn "not given" into a value and overwrite the edge on PATCH.
func TestDependencyConditionSchema_OptionalWithoutDefaults(t *testing.T) {
	tools := NewServer(ServerConfig{Profile: ProfileFull}).MCPServer().ListTools()

	for _, name := range []string{"add_dependency", "update_dependency"} {
		entry, ok := tools[name]
		if !ok {
			t.Fatalf("%s is not registered in the full profile", name)
		}
		schema := entry.Tool.InputSchema
		for _, field := range []string{"condition", "evidence_url"} {
			property, ok := schema.Properties[field].(map[string]any)
			if !ok {
				t.Fatalf("%s: missing %s in the schema", name, field)
			}
			if value, exists := property["default"]; exists {
				t.Errorf("%s: %s default=%v would override an omitted value", name, field, value)
			}
			for _, required := range schema.Required {
				if required == field {
					t.Errorf("%s: %s must be optional", name, field)
				}
			}
		}
	}

	required := map[string]bool{}
	for _, r := range tools["update_dependency"].Tool.InputSchema.Required {
		required[r] = true
	}
	if !required["task_id"] || !required["depends_on_task_id"] {
		t.Errorf("update_dependency must require both ends of the edge, got %v", tools["update_dependency"].Tool.InputSchema.Required)
	}
}
