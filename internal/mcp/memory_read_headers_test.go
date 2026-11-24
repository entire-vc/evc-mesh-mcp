package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// headerCapture is an httptest mesh-api that answers every request with a body
// each client method accepts and records the attribution headers it got.
type headerCapture struct {
	mu   sync.Mutex
	seen []http.Header
}

func newHeaderCapture(t *testing.T) (*headerCapture, *RESTClient) {
	t.Helper()
	hc := &headerCapture{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hc.mu.Lock()
		hc.seen = append(hc.seen, r.Header.Clone())
		hc.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"final_recall_version":1,"items":[],"id":"x"}`))
	}))
	t.Cleanup(srv.Close)
	return hc, NewRESTClient(srv.URL, "test-key")
}

// take returns the headers of the only request made since the previous call.
func (hc *headerCapture) take(t *testing.T, name string) http.Header {
	t.Helper()
	hc.mu.Lock()
	defer hc.mu.Unlock()
	if len(hc.seen) != 1 {
		t.Fatalf("%s: want exactly 1 request, got %d", name, len(hc.seen))
	}
	h := hc.seen[0]
	hc.seen = nil
	return h
}

const (
	testTaskID    = "eb575f5e-80b2-4902-90d7-8148e4cbb28f"
	testSessionID = "sess-eb575f5e-1"
)

// memoryReadCalls is every read the contract names, one client call each.
func memoryReadCalls(c *RESTClient) []struct {
	name string
	call func(context.Context) error
} {
	type rc = struct {
		name string
		call func(context.Context) error
	}
	p := RecallMemoriesParams{Query: "q", WorkspaceID: "ws"}
	return []rc{
		{"recall", func(ctx context.Context) error { _, err := c.RecallMemories(ctx, p); return err }},
		{"recall-final", func(ctx context.Context) error { _, err := c.RecallFinal(ctx, p, false); return err }},
		{"recall-final graph", func(ctx context.Context) error { _, err := c.RecallFinal(ctx, p, true); return err }},
		{"recall_graph", func(ctx context.Context) error {
			_, err := c.RecallWithGraph(ctx, RecallWithGraphParams{Query: "q"})
			return err
		}},
		{"get_memory by-key", func(ctx context.Context) error {
			_, err := c.GetMemoriesByKey(ctx, MemoryByKeyParams{Key: "k"})
			return err
		}},
		{"get_project_knowledge", func(ctx context.Context) error {
			_, err := c.GetProjectKnowledge(ctx, "p", 10, 0, 0, "", false)
			return err
		}},
		{"get_project_updates", func(ctx context.Context) error { _, err := c.ListProjectUpdates(ctx, "p", 1); return err }},
		{"list_comments", func(ctx context.Context) error { _, err := c.ListComments(ctx, testTaskID, nil); return err }},
		{"get_task comments", func(ctx context.Context) error { _, err := c.GetTaskComments(ctx, testTaskID, 5); return err }},
		{"get_task", func(ctx context.Context) error { _, err := c.GetTask(ctx, testTaskID); return err }},
		{"get_task short id", func(ctx context.Context) error { _, err := c.GetTask(ctx, "eb575f5e"); return err }},
		{"get_task_context", func(ctx context.Context) error { _, err := c.GetTaskContext(ctx, testTaskID); return err }},
	}
}

// memoryWriteCalls are writes on the same resources: they carry no attribution.
func memoryWriteCalls(c *RESTClient) []struct {
	name string
	call func(context.Context) error
} {
	type rc = struct {
		name string
		call func(context.Context) error
	}
	return []rc{
		{"remember", func(ctx context.Context) error { _, err := c.Remember(ctx, map[string]any{"key": "k"}); return err }},
		{"set_project_knowledge", func(ctx context.Context) error {
			_, err := c.SetProjectKnowledge(ctx, "p", map[string]any{"key": "k"})
			return err
		}},
		{"archive_memory", func(ctx context.Context) error { _, err := c.SetMemoryArchived(ctx, "m", true, "r"); return err }},
		{"forget", func(ctx context.Context) error { return c.ForgetMemory(ctx, "m") }},
		{"add_comment", func(ctx context.Context) error {
			_, err := c.AddComment(ctx, testTaskID, map[string]any{"body": "b"})
			return err
		}},
		{"update_task", func(ctx context.Context) error {
			_, err := c.UpdateTask(ctx, testTaskID, map[string]any{"title": "t"})
			return err
		}},
		{"post_project_update", func(ctx context.Context) error {
			_, err := c.CreateProjectUpdate(ctx, "p", map[string]any{"body": "b"})
			return err
		}},
	}
}

// isolateAttributionEnv clears every input of the attribution and of the
// checkout fence, so the lane this test runs in cannot leak its own values.
func isolateAttributionEnv(t *testing.T) {
	t.Helper()
	t.Setenv(runnerTaskEnv, "")
	t.Setenv(runnerCheckoutSessionEnv, "")
	t.Setenv("FIDDLER_STATE_FILE", "")
	t.Setenv(checkoutFenceDirEnv, t.TempDir())
}

func TestMemoryReadHeaders_WithRunnerEnv(t *testing.T) {
	isolateAttributionEnv(t)
	t.Setenv(runnerTaskEnv, testTaskID)
	t.Setenv(runnerCheckoutSessionEnv, testSessionID)
	hc, c := newHeaderCapture(t)

	for _, tc := range memoryReadCalls(c) {
		if err := tc.call(context.Background()); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		h := hc.take(t, tc.name)
		if got := h.Get("X-Mesh-Caller"); got != "agent_tool" {
			t.Errorf("%s: X-Mesh-Caller = %q, want agent_tool", tc.name, got)
		}
		if got := h.Get("X-Mesh-Task-Id"); got != testTaskID {
			t.Errorf("%s: X-Mesh-Task-Id = %q, want %q", tc.name, got, testTaskID)
		}
		if got := h.Get("X-Mesh-Session-Id"); got != testSessionID {
			t.Errorf("%s: X-Mesh-Session-Id = %q, want %q", tc.name, got, testSessionID)
		}
	}
}

func TestMemoryReadHeaders_WithoutEnvOnlyCaller(t *testing.T) {
	isolateAttributionEnv(t)
	hc, c := newHeaderCapture(t)

	for _, tc := range memoryReadCalls(c) {
		if err := tc.call(context.Background()); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		h := hc.take(t, tc.name)
		if got := h.Get("X-Mesh-Caller"); got != "agent_tool" {
			t.Errorf("%s: X-Mesh-Caller = %q, want agent_tool", tc.name, got)
		}
		for _, name := range []string{"X-Mesh-Task-Id", "X-Mesh-Session-Id"} {
			if _, present := h[name]; present {
				t.Errorf("%s: %s sent without env: %q", tc.name, name, h.Get(name))
			}
		}
	}
}

func TestMemoryReadHeaders_TaskFallsBackToFiddlerState(t *testing.T) {
	isolateAttributionEnv(t)
	path := filepath.Join(t.TempDir(), "fiddler.json")
	if err := os.WriteFile(path, []byte(`{"task_id":"`+testTaskID+`","thread_id":"th"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIDDLER_STATE_FILE", path)
	hc, c := newHeaderCapture(t)

	if _, err := c.RecallMemories(context.Background(), RecallMemoriesParams{Query: "q"}); err != nil {
		t.Fatal(err)
	}
	h := hc.take(t, "recall")
	if got := h.Get("X-Mesh-Task-Id"); got != testTaskID {
		t.Errorf("X-Mesh-Task-Id = %q, want the fiddler task %q", got, testTaskID)
	}
	if _, present := h["X-Mesh-Session-Id"]; present {
		t.Errorf("X-Mesh-Session-Id sent without %s", runnerCheckoutSessionEnv)
	}

	// The Runner's own variable wins over the side-channel file.
	t.Setenv(runnerTaskEnv, "11111111-2222-3333-4444-555555555555")
	if _, err := c.RecallMemories(context.Background(), RecallMemoriesParams{Query: "q"}); err != nil {
		t.Fatal(err)
	}
	if got := hc.take(t, "recall").Get("X-Mesh-Task-Id"); got != "11111111-2222-3333-4444-555555555555" {
		t.Errorf("X-Mesh-Task-Id = %q, want %s to win over the fiddler file", got, runnerTaskEnv)
	}
}

func TestMemoryReadHeaders_NotOnWrites(t *testing.T) {
	isolateAttributionEnv(t)
	t.Setenv(runnerTaskEnv, testTaskID)
	t.Setenv(runnerCheckoutSessionEnv, testSessionID)
	hc, c := newHeaderCapture(t)

	for _, tc := range memoryWriteCalls(c) {
		if err := tc.call(context.Background()); err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		h := hc.take(t, tc.name)
		for _, name := range []string{"X-Mesh-Caller", "X-Mesh-Task-Id", "X-Mesh-Session-Id"} {
			if _, present := h[name]; present {
				t.Errorf("%s: write carries %s = %q", tc.name, name, h.Get(name))
			}
		}
	}
}
