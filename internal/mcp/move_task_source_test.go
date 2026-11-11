package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// TestMoveTask_DeclaresMCPSource: the move body names this server as the
// producer, so the activity log can tell an MCP move from the Runner or a
// script acting under the same agent key.
func TestMoveTask_DeclaresMCPSource(t *testing.T) {
	taskID := uuid.New().String()
	todoID, doneID := uuid.New().String(), uuid.New().String()
	var moveBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+taskID+"/statuses":
			_ = json.NewEncoder(w).Encode(statusFixture(todoID, doneID))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/"+taskID+"/move":
			_ = json.NewDecoder(r.Body).Decode(&moveBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+taskID:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID, "status_id": doneID})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())

	result, err := server.handleMoveTask(ctx, buildMoveTaskRequest(taskID, "done"))
	if err != nil || result.IsError {
		t.Fatalf("handleMoveTask failed: err=%v result=%+v", err, result)
	}
	if moveBody == nil {
		t.Fatal("move route was never called")
	}
	if got := moveBody["source"]; got != "mcp" {
		t.Errorf("move body source = %v, want \"mcp\"; body: %v", got, moveBody)
	}
}
