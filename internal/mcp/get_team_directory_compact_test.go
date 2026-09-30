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

// teamDirectoryFixture mirrors a real /workspaces/:id/team response closely
// enough to exercise trimming: one agent with computed_status set (the
// trustworthy field per the registry's documented semantics), one agent with only the
// raw (documented-unreliable) status field to check the fallback, and one
// human.
func teamDirectoryFixture() map[string]any {
	return map[string]any{
		"workspace": "Entire VC",
		"agents": []any{
			map[string]any{
				"id":                  "agent-1",
				"name":                "Agent A",
				"slug":                "agent-a",
				"role":                "developer",
				"responsibility_zone": "Mesh — бэкенд и MCP",
				"computed_status":     "online",
				"status":              "offline", // stale/misleading — computed_status must win
				"heartbeat_message":   "Waiting for tasks",
				"capabilities":        map[string]any{"go": true},
				"accepts_from":        []any{"lead-agent"},
				"is_home":             true,
				"is_stale":            false,
				"last_heartbeat":      "2026-09-30T18:41:57Z",
				"last_seen_at":        "2026-09-30T18:41:57Z",
				"working_hours":       "24/7",
				"escalation_to":       "lead-agent",
			},
			map[string]any{
				"id":                  "agent-2",
				"name":                "NoComputedStatus",
				"role":                "tester",
				"responsibility_zone": "Mesh — приёмка",
				"status":              "online", // only raw status present → fallback
			},
		},
		"humans": []any{
			map[string]any{
				"id":                  "human-1",
				"name":                "Owner",
				"role":                "owner",
				"responsibility_zone": "",
				"email":               "owner@example.com",
				"username":            "owner",
			},
		},
	}
}

func teamDirectoryHarness(t *testing.T, fixture map[string]any) *Server {
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

func callGetTeamDirectory(t *testing.T, server *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	result, err := server.handleGetTeamDirectory(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetTeamDirectory returned error: %v", err)
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

// TestHandleGetTeamDirectory_Default_ReturnsCompactTable is the primary
// acceptance check: no `full` param → row-array table, not the per-member
// object dump.
func TestHandleGetTeamDirectory_Default_ReturnsCompactTable(t *testing.T) {
	server := teamDirectoryHarness(t, teamDirectoryFixture())
	out := callGetTeamDirectory(t, server, map[string]any{})

	if humans, ok := out["humans"].([]any); ok && len(humans) > 0 {
		if _, isObj := humans[0].(map[string]any); isObj {
			t.Fatalf("expected compact row-arrays, got full member objects: %v", out)
		}
	}

	agents, ok := out["agents"].([]any)
	if !ok || len(agents) != 2 {
		t.Fatalf("expected 2 compact agent rows, got %v", out["agents"])
	}

	row0, ok := agents[0].([]any)
	if !ok || len(row0) != 5 {
		t.Fatalf("expected a 5-column row [id,name,role,project,status], got %v", agents[0])
	}
	if row0[0] != "agent-1" || row0[1] != "Agent A" || row0[2] != "developer" || row0[3] != "Mesh — бэкенд и MCP" {
		t.Fatalf("unexpected compact row content: %v", row0)
	}
	if row0[4] != "online" {
		t.Fatalf("expected computed_status (\"online\") to win over the stale raw status (\"offline\"), got %v", row0[4])
	}

	// Fields that must NOT survive trimming — the whole point of this task.
	for _, leaked := range []string{"heartbeat_message", "capabilities", "accepts_from", "is_home", "is_stale", "last_heartbeat", "last_seen_at", "working_hours", "escalation_to"} {
		if jsonHasKeyAnywhere(t, out, leaked) {
			t.Fatalf("compact response still leaks %q — trimming is incomplete: %v", leaked, out)
		}
	}
}

// TestHandleGetTeamDirectory_RawStatusFallback checks the second agent row,
// which has only the raw "status" field (no computed_status) — the fallback
// path, not just the preferred path exercised by the first test.
func TestHandleGetTeamDirectory_RawStatusFallback(t *testing.T) {
	server := teamDirectoryHarness(t, teamDirectoryFixture())
	out := callGetTeamDirectory(t, server, map[string]any{})

	agents, _ := out["agents"].([]any)
	if len(agents) != 2 {
		t.Fatalf("expected 2 agent rows, got %d", len(agents))
	}
	row1, ok := agents[1].([]any)
	if !ok || len(row1) != 5 {
		t.Fatalf("expected a 5-column compact row for agent-2, got %v", agents[1])
	}
	if row1[0] != "agent-2" || row1[4] != "online" {
		t.Fatalf("expected raw-status fallback to surface \"online\" for agent-2, got %v", row1)
	}
}

// TestHandleGetTeamDirectory_FullTrue_ReturnsUncompactedDump is the negative
// control / backward-compat path: full=true must return the response exactly
// as the REST API gave it, byte-for-byte reachable, not the compact table.
func TestHandleGetTeamDirectory_FullTrue_ReturnsUncompactedDump(t *testing.T) {
	fixture := teamDirectoryFixture()
	server := teamDirectoryHarness(t, fixture)
	out := callGetTeamDirectory(t, server, map[string]any{"full": true})

	agents, ok := out["agents"].([]any)
	if !ok || len(agents) != 2 {
		t.Fatalf("expected 2 full agent objects, got %v", out["agents"])
	}
	first, ok := agents[0].(map[string]any)
	if !ok {
		t.Fatalf("full=true must return full member objects, got %T: %v", agents[0], agents[0])
	}
	if _, ok := first["heartbeat_message"]; !ok {
		t.Fatalf("full=true dropped a field that must survive untouched: %v", first)
	}
	if first["capabilities"] == nil {
		t.Fatalf("full=true dropped capabilities: %v", first)
	}
}

// jsonHasKeyAnywhere is a shallow guard against accidentally leaving a
// trimmed field present anywhere in the top-level structure (used only to
// assert absence, so false positives on unrelated same-named keys are an
// acceptable trade for simplicity here).
func jsonHasKeyAnywhere(t *testing.T, v any, key string) bool {
	t.Helper()
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x[key]; ok {
			return true
		}
		for _, sub := range x {
			if jsonHasKeyAnywhere(t, sub, key) {
				return true
			}
		}
	case []any:
		for _, sub := range x {
			if jsonHasKeyAnywhere(t, sub, key) {
				return true
			}
		}
	}
	return false
}
