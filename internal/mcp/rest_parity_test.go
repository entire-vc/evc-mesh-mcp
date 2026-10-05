package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// Ratchet: an agent must see every field the REST API serves for an entity, except
// those dropped on purpose. The audit of 05.10.2026 compared REST and MCP key sets
// on prod and found the two sets equal for task, project and comment, doc short by
// storage_key (internal), and artifact with metadata.tr_public_url moved into
// browser_only_url (intentional, see artifact_urls.go). This pins that result: a
// handler that starts dropping a field, or an allowlist entry that stops being true,
// fails here instead of surfacing months later as a "field is missing" report.
//
// Fields that exist on the REST side only and are listed here are the ENTIRE set of
// intentional differences. Adding a name to a list below is a decision to hide that
// field from agents; say why next to it.

// restEntities are REST payloads with every field the real API serializes, so a
// dropped key shows up as a missing one.
func restEntities(taskID, projectID, docID, artifactID string) map[string]map[string]any {
	return map[string]map[string]any{
		"task": {
			"id": taskID, "project_id": projectID, "title": "t", "description": "d", "status_id": "s",
			"priority": "low", "assignee_id": nil, "assignee_type": "agent", "labels": []any{"a"},
			"custom_fields": map[string]any{}, "dod_checks": map[string]any{}, "due_date": nil,
			"human_gate": false, "url": "https://mesh.example/t/" + taskID, "artifact_count": 0,
			"vcs_link_count": 0, "subtask_count": 0, "completion_signal": false,
		},
		"project": {
			"id": projectID, "workspace_id": "w", "name": "p", "slug": "p", "description": "d",
			"settings": map[string]any{}, "url": "https://mesh.example/p/" + projectID,
		},
		"comment": {
			"id": "c1", "task_id": taskID, "body": "b", "author_id": "a", "author_type": "agent",
			"is_internal": false, "metadata": map[string]any{}, "url": "https://mesh.example/t/" + taskID + "?comment=c1",
		},
		"doc": {
			"id": docID, "project_id": projectID, "parent_id": nil, "slug": "d", "title": "D", "version": 1,
			"updated_at": "2026-10-05T00:00:00Z", "storage_key": "internal/key",
			"url": "https://mesh.example/d/" + docID,
		},
		"artifact": {
			"id": artifactID, "task_id": taskID, "name": "out.txt", "mime_type": "text/plain",
			"size_bytes": 1, "storage_url": "", "download_path": "/api/v1/artifacts/" + artifactID + "/download",
			"metadata": map[string]any{"tr_public_url": "https://docs.entire.vc/x"},
		},
	}
}

func sortedKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func missingFrom(rest, mcp map[string]any) []string {
	var out []string
	for _, k := range sortedKeys(rest) {
		if _, ok := mcp[k]; !ok {
			out = append(out, k)
		}
	}
	return out
}

func TestRESTvsMCP_EntityKeysMatchExceptIntentionalDrops(t *testing.T) {
	taskID, projectID, docID, artifactID := uuid.NewString(), uuid.NewString(), uuid.NewString(), uuid.NewString()
	ent := restEntities(taskID, projectID, docID, artifactID)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		enc := json.NewEncoder(w)
		switch r.URL.Path {
		case "/api/v1/tasks/" + taskID:
			_ = enc.Encode(ent["task"])
		case "/api/v1/projects/" + projectID:
			_ = enc.Encode(ent["project"])
		case "/api/v1/projects/" + projectID + "/task-statuses", "/api/v1/projects/" + projectID + "/statuses",
			"/api/v1/projects/" + projectID + "/custom-fields":
			_ = enc.Encode([]any{})
		case "/api/v1/tasks/" + taskID + "/comments":
			_ = enc.Encode(map[string]any{"items": []any{ent["comment"]}, "total_count": 1, "has_more": false})
		case "/api/v1/projects/" + projectID + "/documents":
			_ = enc.Encode(map[string]any{"items": []any{ent["doc"]}, "total_count": 1, "has_more": false})
		case "/api/v1/artifacts/" + artifactID:
			_ = enc.Encode(ent["artifact"])
		default:
			_ = enc.Encode([]any{})
		}
	}))
	t.Cleanup(srv.Close)
	s := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}

	call := func(h func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error), args map[string]any) map[string]any {
		return callDocsTool(t, h, args)
	}
	first := func(v any) map[string]any { return v.([]any)[0].(map[string]any) }

	got := map[string]map[string]any{
		"task":     call(s.handleGetTask, map[string]any{"task_id": taskID, "full": true})["task"].(map[string]any),
		"project":  call(s.handleGetProject, map[string]any{"project_id": projectID})["project"].(map[string]any),
		"comment":  first(call(s.handleListComments, map[string]any{"task_id": taskID, "full": true})["items"]),
		"doc":      first(call(s.handleListDocs, map[string]any{"project_id": projectID})["items"]),
		"artifact": call(s.handleGetArtifact, map[string]any{"artifact_id": artifactID})["artifact"].(map[string]any),
	}

	// entity -> REST fields an agent does not get, and why.
	intentional := map[string][]string{
		"task":     nil,
		"project":  nil,
		"comment":  nil,
		"doc":      {"storage_key"}, // internal object-storage address, never useful to an agent
		"artifact": nil,             // metadata.tr_public_url moves to browser_only_url, checked below
	}
	for name, rest := range ent {
		missing := missingFrom(rest, got[name])
		want := intentional[name]
		if len(missing) != len(want) {
			t.Errorf("%s: REST fields missing from MCP output = %v, intentional = %v", name, missing, want)
			continue
		}
		for i := range want {
			if missing[i] != want[i] {
				t.Errorf("%s: REST fields missing from MCP output = %v, intentional = %v", name, missing, want)
			}
		}
	}

	meta, _ := got["artifact"]["metadata"].(map[string]any)
	if _, still := meta["tr_public_url"]; still {
		t.Errorf("artifact: metadata.tr_public_url must move to browser_only_url")
	}
	if _, ok := got["artifact"]["browser_only_url"]; !ok {
		t.Errorf("artifact: browser_only_url missing, the human URL was lost rather than moved")
	}
}
