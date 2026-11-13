package mcp

import (
	"context"

	"encoding/json"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// readCacheFixture is a mutable fake Mesh API: one task and its comment list,
// which a test can extend between get_task calls.
type readCacheFixture struct {
	mu        sync.Mutex
	task      map[string]any
	comments  []map[string]any // chronological
	taskReads int
}

func (f *readCacheFixture) addComment(id, body, at string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, map[string]any{"id": id, "body": body, "author_name": "A", "created_at": at})
	f.task["updated_at"] = at // a new comment bumps the task like the real API may
}

func readCacheServer(t *testing.T, cache bool) (*Server, *readCacheFixture, string) {
	t.Helper()
	taskID := uuid.New().String()
	f := &readCacheFixture{task: map[string]any{
		"id":            taskID,
		"title":         "read cache task",
		"description":   strings.Repeat("a long static card description. ", 200), // ~6.4k
		"status_id":     "status-1",
		"assignee_name": "Agent A",
		"labels":        []string{"kind:change"},
		"updated_at":    "2026-11-12T10:00:00Z",
	}}
	f.comments = []map[string]any{
		{"id": "c1", "body": strings.Repeat("first comment ", 40), "author_name": "A", "created_at": "2026-11-12T09:00:00Z"},
		{"id": "c2", "body": strings.Repeat("second comment ", 40), "author_name": "A", "created_at": "2026-11-12T09:30:00Z"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/tasks/" + taskID:
			f.taskReads++
			_ = json.NewEncoder(w).Encode(f.task)
		case "/api/v1/tasks/" + taskID + "/comments":
			items := make([]map[string]any, 0, len(f.comments))
			for i := len(f.comments) - 1; i >= 0; i-- { // newest first on the wire
				items = append(items, f.comments[i])
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total_count": len(items), "has_more": false})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	s := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
	if cache {
		s.readCache = newTaskReadCache()
	}
	return s, f, taskID
}

func getTaskRaw(t *testing.T, s *Server, args map[string]any) (string, map[string]any) {
	t.Helper()
	res, decoded := callGetTask(t, s, args)
	return resultText(t, res), decoded
}

func commentIDs(resp map[string]any) []string {
	arr, _ := resp["comments"].([]any)
	ids := make([]string, 0, len(arr))
	for _, c := range arr {
		m, _ := c.(map[string]any)
		id, _ := m["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// A second get_task of an unchanged card
// in the same session comes back without the description (< 1k); after a new
// comment only that comment is returned; full=true still returns everything.
func TestGetTaskReadCache_RepeatReadIsDelta(t *testing.T) {
	s, f, id := readCacheServer(t, true)
	args := map[string]any{"task_id": id, "include_comments": true}

	firstRaw, first := getTaskRaw(t, s, args)
	task, _ := first["task"].(map[string]any)
	if task["description"] == nil {
		t.Fatal("first read must carry the description")
	}
	if got := commentIDs(first); strings.Join(got, ",") != "c1,c2" {
		t.Fatalf("first read comments = %v, want c1,c2", got)
	}
	if len(firstRaw) < 6000 {
		t.Fatalf("fixture too small to prove the trim: first read %d chars", len(firstRaw))
	}

	secondRaw, second := getTaskRaw(t, s, args)
	if len(secondRaw) >= 1000 {
		t.Errorf("unchanged repeat read = %d chars, want < 1000: %s", len(secondRaw), secondRaw)
	}
	if second["task_changed"] != false {
		t.Errorf("task_changed = %v, want false", second["task_changed"])
	}
	stub, _ := second["task"].(map[string]any)
	if _, has := stub["description"]; has {
		t.Error("unchanged repeat read must not repeat the description")
	}
	if stub["id"] != id || stub["status_id"] != "status-1" {
		t.Errorf("stub must keep id and status, got %v", stub)
	}
	if got := commentIDs(second); len(got) != 0 {
		t.Errorf("repeat read must not repeat shown comments, got %v", got)
	}
	if second["comments_already_shown"] != float64(2) {
		t.Errorf("comments_already_shown = %v, want 2", second["comments_already_shown"])
	}

	f.addComment("c3", "a new comment", "2026-11-12T11:00:00Z")
	thirdRaw, third := getTaskRaw(t, s, args)
	if got := commentIDs(third); strings.Join(got, ",") != "c3" {
		t.Errorf("after a new comment got comments %v, want only c3", got)
	}
	t3, _ := third["task"].(map[string]any)
	if _, has := t3["description"]; has {
		t.Error("new comment must not resend the unchanged description")
	}
	if third["description_unchanged"] != true {
		t.Errorf("description_unchanged = %v, want true", third["description_unchanged"])
	}
	if len(thirdRaw) >= 1000 {
		t.Errorf("read after a new comment = %d chars, want < 1000", len(thirdRaw))
	}

	_, full := getTaskRaw(t, s, map[string]any{"task_id": id, "include_comments": true, "full": true})
	ft, _ := full["task"].(map[string]any)
	if ft["description"] == nil {
		t.Error("full=true must return the description")
	}
	if got := commentIDs(full); strings.Join(got, ",") != "c1,c2,c3" {
		t.Errorf("full=true comments = %v, want c1,c2,c3", got)
	}
	if _, has := full["task_changed"]; has {
		t.Error("full=true must not be a delta")
	}
}

// A changed description is returned again.
func TestGetTaskReadCache_ChangedDescriptionReturned(t *testing.T) {
	s, f, id := readCacheServer(t, true)
	getTaskRaw(t, s, map[string]any{"task_id": id})
	f.mu.Lock()
	f.task["description"] = "rewritten"
	f.task["updated_at"] = "2026-11-12T12:00:00Z"
	f.mu.Unlock()
	_, resp := getTaskRaw(t, s, map[string]any{"task_id": id})
	task, _ := resp["task"].(map[string]any)
	if task["description"] != "rewritten" || resp["task_changed"] != true {
		t.Errorf("changed description must come back whole, got %v", resp)
	}
}

// Negative control: without the session cache (SSE/HTTP), the repeat read is
// the whole card, as before.
func TestGetTaskReadCache_OffRepeatsEverything(t *testing.T) {
	s, _, id := readCacheServer(t, false)
	args := map[string]any{"task_id": id, "include_comments": true}
	getTaskRaw(t, s, args)
	raw, second := getTaskRaw(t, s, args)
	task, _ := second["task"].(map[string]any)
	if task["description"] == nil || len(commentIDs(second)) != 2 || len(raw) < 6000 {
		t.Errorf("cache off: repeat read must be whole, got %d chars", len(raw))
	}
}

// An explicit since keeps its own documented semantics with the cache on.
func TestGetTaskReadCache_ExplicitSinceBypasses(t *testing.T) {
	s, _, id := readCacheServer(t, true)
	getTaskRaw(t, s, map[string]any{"task_id": id, "include_comments": true})
	_, resp := getTaskRaw(t, s, map[string]any{"task_id": id, "include_comments": true, "since": "2026-11-12T09:15:00Z"})
	if got := commentIDs(resp); strings.Join(got, ",") != "c2" {
		t.Errorf("since must filter by time only, got %v", got)
	}
	if resp["task_changed"] != true {
		t.Errorf("since before updated_at: task_changed = %v, want true", resp["task_changed"])
	}
}

// Comments the session never received (an earlier read without comments, or
// a larger comments_limit) are not hidden.
func TestGetTaskReadCache_UnseenCommentsStillReturned(t *testing.T) {
	s, _, id := readCacheServer(t, true)
	getTaskRaw(t, s, map[string]any{"task_id": id})
	_, resp := getTaskRaw(t, s, map[string]any{"task_id": id, "include_comments": true})
	if got := commentIDs(resp); strings.Join(got, ",") != "c1,c2" {
		t.Errorf("comments never shown must be returned, got %v", got)
	}
}

// Review finding P1: a comment returned in the summary representation
// (include_auto=false) is not "seen" for an include_auto=true read that asks
// for whole automation bodies; a fully returned comment is seen for both.
func TestGetTaskReadCache_IncludeAutoNotHiddenBySummaryRead(t *testing.T) {
	s, _, id := readCacheServer(t, true)
	getTaskRaw(t, s, map[string]any{"task_id": id, "include_comments": true})
	_, auto := getTaskRaw(t, s, map[string]any{"task_id": id, "include_comments": true, "include_auto": true})
	if got := commentIDs(auto); strings.Join(got, ",") != "c1,c2" {
		t.Errorf("include_auto=true after a summary read must return the bodies, got %v", got)
	}
	_, again := getTaskRaw(t, s, map[string]any{"task_id": id, "include_comments": true})
	if got := commentIDs(again); len(got) != 0 {
		t.Errorf("comments already returned whole must not repeat in summary mode, got %v", got)
	}
}

// Review finding P2: a call that ends in an error records nothing, so the
// next successful read still returns the description and comments.
func TestGetTaskReadCache_FailedCallRecordsNothing(t *testing.T) {
	s, _, id := readCacheServer(t, true)
	res, _ := callGetTaskAllowError(t, s, map[string]any{"task_id": id, "include_comments": true, "comments_limit": 999})
	if !res.IsError {
		t.Fatal("comments_limit=999 must be refused")
	}
	_, resp := getTaskRaw(t, s, map[string]any{"task_id": id, "include_comments": true})
	task, _ := resp["task"].(map[string]any)
	if task["description"] == nil {
		t.Error("description must come back: the failed call never returned it")
	}
	if got := commentIDs(resp); strings.Join(got, ",") != "c1,c2" {
		t.Errorf("comments must come back after a failed call, got %v", got)
	}
}

func callGetTaskAllowError(t *testing.T, s *Server, args map[string]any) (*mcpsdk.CallToolResult, error) {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	return s.handleGetTask(context.Background(), req)
}
