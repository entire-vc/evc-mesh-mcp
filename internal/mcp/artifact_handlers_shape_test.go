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

// End-to-end through the real handlers against a fake REST API that serves the
// artifact JSON exactly as the backend does: download_path plus a
// metadata.tr_public_url page for a human's browser.

func artifactBackendJSON(id string) map[string]any {
	return map[string]any{
		"id":            id,
		"name":          "out.txt",
		"download_path": "/api/v1/artifacts/" + id + "/download",
		"metadata":      map[string]any{"tr_public_url": "https://docs.entire.vc/spark/t/out.txt"},
	}
}

func artifactHarness(t *testing.T, id, taskID string) *Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/v1/artifacts/"+id:
			_ = json.NewEncoder(w).Encode(artifactBackendJSON(id))
		case r.URL.Path == "/api/v1/tasks/"+taskID+"/artifacts":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{artifactBackendJSON(id)}, "total_count": 1, "has_more": false})
		case r.URL.Path == "/api/v1/tasks/"+taskID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID, "title": "t"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
}

func callArtifactTool(t *testing.T, h func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error), args map[string]any) string {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("tool failed: %v %v", err, res)
	}
	return res.Content[0].(mcpsdk.TextContent).Text
}

func assertBrowserOnly(t *testing.T, body string) {
	t.Helper()
	if strings.Contains(body, `"metadata":{"tr_public_url"`) || strings.Contains(body, `"tr_public_url":`) {
		t.Errorf("tr_public_url must not appear under metadata in agent output:\n%s", body)
	}
	if !strings.Contains(body, `"browser_only_url"`) || !strings.Contains(body, "docs.entire.vc/spark/t/out.txt") {
		t.Errorf("the human URL must remain, labelled browser_only_url:\n%s", body)
	}
	if !strings.Contains(body, `"download_path":"/api/v1/artifacts/`) {
		t.Errorf("download_path (the API path) must be present:\n%s", body)
	}
}

func TestArtifactHandlers_SeparateTheAPIPathFromTheBrowserURL(t *testing.T) {
	id, taskID := uuid.New().String(), uuid.New().String()
	s := artifactHarness(t, id, taskID)

	assertBrowserOnly(t, callArtifactTool(t, s.handleGetArtifact, map[string]any{"artifact_id": id}))
	assertBrowserOnly(t, callArtifactTool(t, s.handleListArtifacts, map[string]any{"task_id": taskID}))
	assertBrowserOnly(t, callArtifactTool(t, s.handleGetTask, map[string]any{"task_id": taskID, "include_artifacts": true}))
}

func TestHandleGetArtifact_IncludeContentNamesTheStepOneURL(t *testing.T) {
	id, taskID := uuid.New().String(), uuid.New().String()
	s := artifactHarness(t, id, taskID)
	body := callArtifactTool(t, s.handleGetArtifact, map[string]any{"artifact_id": id, "include_content": true})
	if strings.Contains(body, `"download_url"`) {
		t.Errorf("the old download_url key read as the bytes' URL and is not; it must be gone:\n%s", body)
	}
	if !strings.Contains(body, `"download_api_url":"`) || !strings.Contains(body, "/api/v1/artifacts/"+id+"/download") {
		t.Errorf("expected download_api_url pointing at step 1:\n%s", body)
	}
}
