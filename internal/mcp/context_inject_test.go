package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

func buildRememberRequest(args map[string]any) mcpsdk.CallToolRequest {
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

func buildSetProjectKnowledgeRequest(args map[string]any) mcpsdk.CallToolRequest {
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

func setupContextInjectServer(t *testing.T) (*Server, *httptest.Server, chan map[string]any) {
	t.Helper()
	received := make(chan map[string]any, 4)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		received <- body
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/memories":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": uuid.New().String()})
		case r.Method == http.MethodPost:
			// set_project_knowledge: POST /api/v1/projects/:id/knowledge
			_ = json.NewEncoder(w).Encode(map[string]any{"key": "test"})
		default:
			http.NotFound(w, r)
		}
	}))

	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
	}

	return server, srv, received
}

// writeFiddlerStateFile writes a side-channel file to a temp dir and sets FIDDLER_STATE_FILE.
func writeFiddlerStateFile(t *testing.T, taskID, threadID string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "current.json")
	data, _ := json.Marshal(map[string]string{
		"task_id":   taskID,
		"thread_id": threadID,
	})
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatalf("writeFiddlerStateFile: %v", err)
	}
	t.Setenv("FIDDLER_STATE_FILE", path)
	return path
}

func agentCtx(agentID uuid.UUID) context.Context {
	return ContextWithSession(context.Background(), &AgentSession{
		AgentID:     agentID,
		WorkspaceID: uuid.New(),
	})
}

// TestRemember_AutoPopulatesFromFiddlerFile verifies that source_task_id and thread_id
// are injected from the FIDDLER_STATE_FILE when the caller omits them.
func TestRemember_AutoPopulatesFromFiddlerFile(t *testing.T) {
	server, srv, received := setupContextInjectServer(t)
	defer srv.Close()

	taskID := uuid.New().String()
	threadID := uuid.New().String()
	writeFiddlerStateFile(t, taskID, threadID)

	ctx := agentCtx(uuid.New())
	req := buildRememberRequest(map[string]any{
		"key":     "test-key",
		"content": "test content",
	})
	if _, err := server.handleRemember(ctx, req); err != nil {
		t.Fatalf("handleRemember: %v", err)
	}

	body := <-received
	if got, ok := body["source_task_id"].(string); !ok || got != taskID {
		t.Errorf("source_task_id: want %q, got %q", taskID, body["source_task_id"])
	}
	if got, ok := body["thread_id"].(string); !ok || got != threadID {
		t.Errorf("thread_id: want %q, got %q", threadID, body["thread_id"])
	}
}

// TestRemember_ExplicitValueWins verifies that an explicitly-provided source_task_id
// and thread_id override the fiddler file content.
func TestRemember_ExplicitValueWins(t *testing.T) {
	server, srv, received := setupContextInjectServer(t)
	defer srv.Close()

	writeFiddlerStateFile(t, uuid.New().String(), uuid.New().String())

	explicitTaskID := uuid.New().String()
	explicitThreadID := uuid.New().String()

	ctx := agentCtx(uuid.New())
	req := buildRememberRequest(map[string]any{
		"key":            "test-key",
		"content":        "test content",
		"source_task_id": explicitTaskID,
		"thread_id":      explicitThreadID,
	})
	if _, err := server.handleRemember(ctx, req); err != nil {
		t.Fatalf("handleRemember: %v", err)
	}

	body := <-received
	if got := body["source_task_id"].(string); got != explicitTaskID {
		t.Errorf("source_task_id: want %q, got %q", explicitTaskID, got)
	}
	if got := body["thread_id"].(string); got != explicitThreadID {
		t.Errorf("thread_id: want %q, got %q", explicitThreadID, got)
	}
}

// TestRemember_AttachContextFalse verifies that attach_context=false suppresses auto-inject.
func TestRemember_AttachContextFalse(t *testing.T) {
	server, srv, received := setupContextInjectServer(t)
	defer srv.Close()

	writeFiddlerStateFile(t, uuid.New().String(), uuid.New().String())

	ctx := agentCtx(uuid.New())
	req := buildRememberRequest(map[string]any{
		"key":            "test-key",
		"content":        "test content",
		"attach_context": false,
	})
	if _, err := server.handleRemember(ctx, req); err != nil {
		t.Fatalf("handleRemember: %v", err)
	}

	body := <-received
	if _, present := body["source_task_id"]; present {
		t.Errorf("source_task_id should be absent when attach_context=false, got %q", body["source_task_id"])
	}
	if _, present := body["thread_id"]; present {
		t.Errorf("thread_id should be absent when attach_context=false, got %q", body["thread_id"])
	}
}

// TestSetProjectKnowledge_AutoPopulatesFromFiddlerFile verifies that source_task_id
// and thread_id are injected into set_project_knowledge calls from the side-channel.
func TestSetProjectKnowledge_AutoPopulatesFromFiddlerFile(t *testing.T) {
	server, srv, received := setupContextInjectServer(t)
	defer srv.Close()

	taskID := uuid.New().String()
	threadID := uuid.New().String()
	writeFiddlerStateFile(t, taskID, threadID)

	projectID := uuid.New().String()
	ctx := agentCtx(uuid.New())
	req := buildSetProjectKnowledgeRequest(map[string]any{
		"project_id": projectID,
		"key":        "test-fact",
		"value":      "test value",
	})
	if _, err := server.handleSetProjectKnowledge(ctx, req); err != nil {
		t.Fatalf("handleSetProjectKnowledge: %v", err)
	}

	body := <-received
	if got, ok := body["source_task_id"].(string); !ok || got != taskID {
		t.Errorf("source_task_id: want %q, got %q", taskID, body["source_task_id"])
	}
	if got, ok := body["thread_id"].(string); !ok || got != threadID {
		t.Errorf("thread_id: want %q, got %q", threadID, body["thread_id"])
	}
}

// TestRemember_FallbackToSyncMap verifies that when FIDDLER_STATE_FILE is absent,
// source_task_id is populated from the activeTaskIDs sync.Map (checkout-tracking).
func TestRemember_FallbackToSyncMap(t *testing.T) {
	server, srv, received := setupContextInjectServer(t)
	defer srv.Close()

	t.Setenv("FIDDLER_STATE_FILE", "") // ensure no file

	agentID := uuid.New()
	storedTaskID := uuid.New().String()
	server.activeTaskIDs.Store(agentID, storedTaskID)

	ctx := agentCtx(agentID)
	req := buildRememberRequest(map[string]any{
		"key":     "test-key",
		"content": "test content",
	})
	if _, err := server.handleRemember(ctx, req); err != nil {
		t.Fatalf("handleRemember: %v", err)
	}

	body := <-received
	if got, ok := body["source_task_id"].(string); !ok || got != storedTaskID {
		t.Errorf("source_task_id fallback: want %q, got %q", storedTaskID, body["source_task_id"])
	}
}
