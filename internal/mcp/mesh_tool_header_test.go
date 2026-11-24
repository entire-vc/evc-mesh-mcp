package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// toolHeaderAPI is an httptest mesh-api that records, per request, the path
// and the X-Mesh-Tool value (present or not). It answers a task GET with the
// task and a comments GET with one comment.
type toolHeaderAPI struct {
	mu   sync.Mutex
	seen []toolHeaderSeen
}

type toolHeaderSeen struct {
	path    string
	tool    string
	present bool
}

func newToolHeaderAPI(t *testing.T, taskID string) (*toolHeaderAPI, *Server) {
	t.Helper()
	api := &toolHeaderAPI{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, present := r.Header[http.CanonicalHeaderKey("X-Mesh-Tool")]
		api.mu.Lock()
		api.seen = append(api.seen, toolHeaderSeen{path: r.URL.Path, tool: r.Header.Get("X-Mesh-Tool"), present: present})
		api.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/comments") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":       []map[string]any{{"id": uuid.New().String(), "body": "b", "created_at": "2026-11-20T10:00:00Z"}},
				"total_count": 1, "has_more": false,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID, "title": "t", "updated_at": "2026-11-20T10:00:00Z"})
	}))
	t.Cleanup(srv.Close)
	return api, &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
}

func (a *toolHeaderAPI) take() []toolHeaderSeen {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := a.seen
	a.seen = nil
	return out
}

func callToolOK(t *testing.T, name string, h func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error), args map[string]any) {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	if res.IsError {
		t.Fatalf("%s: tool error: %s", name, resultText(t, res))
	}
}

// get_task(include_comments=true) is one tool call made of two requests. The
// task GET is marked get_task+comments (mesh-api skips it in the daily
// counter), the comments GET is marked get_task (books the single call).
func TestMeshToolHeader_GetTaskWithComments(t *testing.T) {
	taskID := uuid.New().String()
	api, s := newToolHeaderAPI(t, taskID)

	callToolOK(t, "get_task include_comments", s.handleGetTask, map[string]any{"task_id": taskID, "include_comments": true})

	seen := api.take()
	if len(seen) != 2 {
		t.Fatalf("want 2 requests (task, comments), got %d: %+v", len(seen), seen)
	}
	task, comments := seen[0], seen[1]
	if task.path != "/api/v1/tasks/"+taskID {
		t.Fatalf("first request path = %q, want the task GET", task.path)
	}
	if task.tool != "get_task+comments" {
		t.Errorf("task GET X-Mesh-Tool = %q (present=%v), want get_task+comments", task.tool, task.present)
	}
	if !strings.HasSuffix(comments.path, "/comments") {
		t.Fatalf("second request path = %q, want the comments GET", comments.path)
	}
	if comments.tool != "get_task" {
		t.Errorf("comments GET X-Mesh-Tool = %q (present=%v), want get_task", comments.tool, comments.present)
	}
}

// Every other read carries no X-Mesh-Tool: a plain get_task, a plain
// list_comments and the list_comments `after` walk (compact_read.go).
func TestMeshToolHeader_AbsentElsewhere(t *testing.T) {
	taskID := uuid.New().String()
	api, s := newToolHeaderAPI(t, taskID)

	cases := []struct {
		name string
		h    func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error)
		args map[string]any
	}{
		{"get_task", s.handleGetTask, map[string]any{"task_id": taskID}},
		{"get_task include_comments=false", s.handleGetTask, map[string]any{"task_id": taskID, "include_comments": false}},
		{"list_comments", s.handleListComments, map[string]any{"task_id": taskID}},
		{"list_comments after", s.handleListComments, map[string]any{"task_id": taskID, "after": "2026-11-19T00:00:00Z"}},
	}
	for _, tc := range cases {
		callToolOK(t, tc.name, tc.h, tc.args)
		seen := api.take()
		if len(seen) == 0 {
			t.Fatalf("%s: no request reached the API", tc.name)
		}
		for _, r := range seen {
			if r.present {
				t.Errorf("%s: %s carries X-Mesh-Tool = %q, want none", tc.name, r.path, r.tool)
			}
		}
	}
}

// A comments_limit out of range is refused before any request: a task GET
// marked get_task+comments without the comments GET after it would drop the
// call from the daily counter.
func TestMeshToolHeader_BadCommentsLimitSendsNothing(t *testing.T) {
	taskID := uuid.New().String()
	api, s := newToolHeaderAPI(t, taskID)

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID, "include_comments": true, "comments_limit": 0}
	res, err := s.handleGetTask(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || !strings.Contains(resultText(t, res), "comments_limit must be between") {
		t.Fatalf("want the comments_limit refusal, got %s", resultText(t, res))
	}
	if seen := api.take(); len(seen) != 0 {
		t.Errorf("refused call still sent %d request(s): %+v", len(seen), seen)
	}
}
