package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The REST API computes a canonical deep-link on the entities it returns
// (Project.url `/p/<id>`, Comment.url `/t/<taskID>?comment=<id>`). This layer
// adds no such field of its own. get_project and list_comments(full=true)
// preserve the REST URL; compact comments intentionally omit this field.
func TestGetProjectTool_PassesProjectURLThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/statuses"),
			strings.HasSuffix(r.URL.Path, "/custom-fields"):
			_ = json.NewEncoder(w).Encode([]map[string]any{})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":   "proj-1",
				"slug": "demo",
				"url":  "https://mesh.entire.host/p/proj-1",
			})
		}
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}

	result, err := server.handleGetProject(context.Background(), requestWith(map[string]any{
		"project_id": "proj-1",
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", resultText(t, result))
	}

	var payload struct {
		Project struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"project"`
	}
	if err := json.Unmarshal([]byte(resultText(t, result)), &payload); err != nil {
		t.Fatalf("could not decode response JSON: %v", err)
	}
	if payload.Project.URL != "https://mesh.entire.host/p/proj-1" {
		t.Fatalf("project.url = %q, want the REST deep-link passed through verbatim", payload.Project.URL)
	}
}

func TestListCommentsTool_FullPassesCommentURLThrough(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"id": "c-1", "url": "https://mesh.entire.host/t/task-1?comment=c-1"},
			},
			"total_count": 1,
			"page":        1,
			"has_more":    false,
		})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}

	result, err := server.handleListComments(context.Background(), requestWith(map[string]any{
		"task_id": "task-1",
		"full":    true,
	}))
	if err != nil {
		t.Fatalf("transport error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", resultText(t, result))
	}

	var payload struct {
		Items []struct {
			ID  string `json:"id"`
			URL string `json:"url"`
		} `json:"items"`
	}
	if err := json.Unmarshal([]byte(resultText(t, result)), &payload); err != nil {
		t.Fatalf("could not decode response JSON: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0].URL != "https://mesh.entire.host/t/task-1?comment=c-1" {
		t.Fatalf("items = %+v, want the REST comment deep-link passed through verbatim", payload.Items)
	}
}
