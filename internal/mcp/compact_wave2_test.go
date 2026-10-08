package mcp

// Wave 2 of the compact-responses work (card #dc719c63, parent #80bb6658):
// the compact view is the DEFAULT for recall, list_comments, get_task,
// list_tasks and get_team_directory; full=true restores the verbatim response
// everywhere. Red control: on wave-1 main every compact assertion here failed.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// --- recall -----------------------------------------------------------------

func TestRecallWave2Compact_DefaultFieldsOnly(t *testing.T) {
	item := map[string]any{
		"key": "solution-x", "content": strings.Repeat("д", 450), "score": 0.02,
		"tags":             []any{"kind:learning", "project:mesh-dev"},
		"scope":            "project",
		"importance_score": 0.7,
		"created_at":       "2026-10-01T00:00:00Z",
		"updated_at":       "2026-10-02T00:00:00Z",
		"agent_id":         "a", "content_simhash": 7,
	}
	server, closeFn := newRecallTestServer(t, []any{item}, nil)
	defer closeFn()

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "solution x"}
	result, err := server.handleRecall(context.Background(), req)
	if err != nil {
		t.Fatalf("handleRecall: %v", err)
	}
	out := decodeRecallResult(t, result)
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items = %d, want 1 (explanation: %v)", len(items), out["explanation"])
	}
	m, _ := items[0].(map[string]any)

	for _, f := range []string{"key", "tags", "score", "content"} {
		if _, has := m[f]; !has {
			t.Errorf("compact item lost %s", f)
		}
	}
	// Wave-2 drops: the write-path bookkeeping a reader does not act on.
	// created_at moved to full=true only — including the updated_at fallback.
	for _, f := range []string{
		"scope", "importance_score", "created_at", "updated_at",
		"content_chars", "content_truncated", "agent_id", "content_simhash",
	} {
		if _, has := m[f]; has {
			t.Errorf("compact item still carries %s (wave-2 drop)", f)
		}
	}
	c, _ := m["content"].(string)
	if n := len([]rune(c)); n != 300+1 {
		t.Errorf("content = %d runes, want 300 + ellipsis", n)
	}
	if !strings.HasSuffix(c, "…") {
		t.Error("a cut content must end with the ellipsis mark (no content_truncated field in wave 2)")
	}
}

func TestRecallWave2Full_Verbatim(t *testing.T) {
	item := map[string]any{
		"key": "solution-x", "content": strings.Repeat("a", 450), "score": 0.02,
		"tags": []any{"kind:learning"}, "scope": "project", "importance_score": 0.7,
		"created_at": "2026-10-01T00:00:00Z", "agent_id": "a",
	}
	server, closeFn := newRecallTestServer(t, []any{item}, nil)
	defer closeFn()

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "solution x", "full": true}
	result, err := server.handleRecall(context.Background(), req)
	if err != nil {
		t.Fatalf("handleRecall: %v", err)
	}
	out := decodeRecallResult(t, result)
	items, _ := out["items"].([]any)
	m, _ := items[0].(map[string]any)
	for _, f := range []string{"scope", "importance_score", "created_at", "agent_id"} {
		if _, has := m[f]; !has {
			t.Errorf("full=true lost %s", f)
		}
	}
	if c, _ := m["content"].(string); c != strings.Repeat("a", 450) {
		t.Error("full=true must return content verbatim")
	}
}

// --- list_comments ----------------------------------------------------------

func wave2Comment(id string, body string, internal bool) map[string]any {
	return map[string]any{
		"id": id, "task_id": "task-1",
		"author_id":   uuid.New().String(),
		"author_name": "Linus",
		"author_type": "agent",
		"body":        body,
		"created_at":  "2026-10-05T10:00:00Z",
		"updated_at":  "2026-10-05T10:00:00Z",
		"is_internal": internal,
		"metadata":    map[string]any{"x": 1},
		"url":         "https://mesh.entire.host/c/" + id,
	}
}

func newCommentsTestServer(t *testing.T, comments []map[string]any) *Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": comments, "page": 1, "page_size": len(comments),
			"total_count": len(comments), "total_pages": 1, "has_more": false,
		})
	}))
	t.Cleanup(srv.Close)
	return &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
}

func callListComments(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	res, err := s.handleListComments(context.Background(), requestWith(args))
	if err != nil {
		t.Fatalf("handleListComments: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return out
}

func TestListCommentsWave2Compact_DefaultFieldsOnly(t *testing.T) {
	comments := []map[string]any{
		wave2Comment("c1", strings.Repeat("b", 700), false), // long body preserved
		wave2Comment("c2", "short body", true),              // internal flag is full-only
	}
	s := newCommentsTestServer(t, comments)

	out := callListComments(t, s, map[string]any{"task_id": "task-1", "limit": 10})
	items, _ := out["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items = %d, want 2", len(items))
	}

	// Envelope pagination fields stay: paging is how a longer thread is read.
	for _, f := range []string{"page", "page_size", "total_count", "total_pages", "has_more"} {
		if _, has := out[f]; !has {
			t.Errorf("compact response lost envelope field %s", f)
		}
	}

	first, _ := items[0].(map[string]any)
	for _, f := range []string{"id", "author_name", "created_at", "body"} {
		if _, has := first[f]; !has {
			t.Errorf("compact comment lost %s", f)
		}
	}
	for _, f := range []string{
		"author_id", "author_type", "task_id", "url", "metadata",
		"parent_comment_id", "updated_at",
	} {
		if _, has := first[f]; has {
			t.Errorf("compact comment still carries %s (wave-2 drop)", f)
		}
	}
	if first["body"] != strings.Repeat("b", 700) {
		t.Error("compact body must be complete")
	}
	if _, has := first["body_truncated"]; has {
		t.Error("obsolete body marker")
	}
	second := items[1].(map[string]any)
	if _, has := second["is_internal"]; has {
		t.Error("is_internal is full-only")
	}

}

// The pager must survive the compact view exactly when there IS a next
// page: has_more=true without page/total_pages/list_revision means a caller
// can no longer continue the thread or detect a stale page.
func TestListCommentsWave2Compact_KeepsPagingWhenHasMore(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []any{wave2Comment("c21", "page two body", false)},
			"page":  2, "page_size": 10, "total_count": 42,
			"total_pages": 5, "has_more": true, "list_revision": 7,
			"order": "desc", // not on the envelope allow-list: must drop
		})
	}))
	t.Cleanup(srv.Close)
	s := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}

	out := callListComments(t, s, map[string]any{"task_id": "task-1", "limit": 10})
	if out["has_more"] != true {
		t.Errorf("has_more = %v, want true in the compact view", out["has_more"])
	}
	if out["page"] != float64(2) {
		t.Errorf("page = %v, want 2 (the continuation position)", out["page"])
	}
	if out["page_size"] != float64(10) {
		t.Errorf("page_size = %v, want 10", out["page_size"])
	}
	if out["total_count"] != float64(42) {
		t.Errorf("total_count = %v, want 42", out["total_count"])
	}
	if out["total_pages"] != float64(5) {
		t.Errorf("total_pages = %v, want 5", out["total_pages"])
	}
	if out["list_revision"] != float64(7) {
		t.Errorf("list_revision = %v, want 7 (stale-page detection needs it)", out["list_revision"])
	}
	if _, has := out["order"]; has {
		t.Error("order is not on the envelope allow-list and must be dropped")
	}
	if n := len(out["items"].([]any)); n != 1 {
		t.Errorf("items = %d, want 1", n)
	}
}

func TestListCommentsWave2Full_Verbatim(t *testing.T) {
	comments := []map[string]any{wave2Comment("c1", strings.Repeat("b", 700), false)}
	s := newCommentsTestServer(t, comments)

	out := callListComments(t, s, map[string]any{"task_id": "task-1", "full": true})
	items, _ := out["items"].([]any)
	first, _ := items[0].(map[string]any)
	for _, f := range []string{"author_id", "author_type", "task_id", "url", "metadata", "updated_at"} {
		if _, has := first[f]; !has {
			t.Errorf("full=true lost %s", f)
		}
	}
	if b, _ := first["body"].(string); len([]rune(b)) != 700 {
		t.Error("full=true must return the body verbatim")
	}
}

// --- get_task ---------------------------------------------------------------

func TestGetTaskWave3_DescriptionWhole(t *testing.T) {
	id := uuid.New().String()
	long := strings.Repeat("я", 9000)
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": "t", "description": long})
	})

	res := callGetTaskCompact(t, s, map[string]any{"task_id": id})
	task, _ := res["task"].(map[string]any)
	if task["description"] != long {
		t.Error("compact description must be complete")
	}
	for _, k := range []string{"description_truncated", "description_chars", "description_hint"} {
		if _, ok := task[k]; ok {
			t.Errorf("obsolete marker %s", k)
		}
	}

	// full=true: verbatim, no markers.
	res = callGetTaskCompact(t, s, map[string]any{"task_id": id, "full": true})
	task, _ = res["task"].(map[string]any)
	if task["description"] != long {
		t.Error("full=true must return the description verbatim")
	}
}

func TestGetTaskWave2_EmptyServiceFieldsDropped(t *testing.T) {
	id := uuid.New().String()
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "title": "t", "description": "d",
			// zero counters / nulls / service fields: dropped in compact view
			"artifact_count": 0, "vcs_link_count": 0, "subtask_count": 0,
			"custom_fields": map[string]any{}, "dod_checks": map[string]any{},
			"estimated_hours": nil, "start_after": nil, "due_date": nil,
			"completed_at": nil, "completion_signal": false, "is_shipped": false,
			"position": 0, "assigned_by": "", "url": "https://mesh.entire.host/t/" + id,
			// real values: must survive
			"labels": []any{"mesh"}, "human_gate": false,
			"parent_task_id": "p-1", "priority": "urgent",
		})
	})

	res := callGetTaskCompact(t, s, map[string]any{"task_id": id})
	task, _ := res["task"].(map[string]any)
	for _, f := range []string{
		"artifact_count", "vcs_link_count", "subtask_count", "custom_fields", "dod_checks",
		"estimated_hours", "start_after", "due_date", "completed_at", "completion_signal",
		"is_shipped", "position", "assigned_by", "url", "human_gate",
	} {
		if _, has := task[f]; has {
			t.Errorf("compact get_task still carries empty/service field %s", f)
		}
	}
	for _, f := range []string{"labels", "parent_task_id", "priority", "id", "title"} {
		if _, has := task[f]; !has {
			t.Errorf("compact get_task lost real-value field %s", f)
		}
	}

	// full=true keeps the verbatim object, zeros included.
	res = callGetTaskCompact(t, s, map[string]any{"task_id": id, "full": true})
	task, _ = res["task"].(map[string]any)
	if _, has := task["artifact_count"]; !has {
		t.Error("full=true must keep artifact_count")
	}
	if _, has := task["url"]; !has {
		t.Error("full=true must keep url")
	}
}

func TestGetTaskWave3_CountersFullOnly(t *testing.T) {
	id := uuid.New().String()
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "title": "t", "description": "d",
			"artifact_count": 2, "subtask_count": 3, "due_date": "2026-10-06T00:00:00Z",
			"custom_fields": map[string]any{"tier": "B"},
		})
	})
	res := callGetTaskCompact(t, s, map[string]any{"task_id": id})
	task, _ := res["task"].(map[string]any)
	for _, f := range []string{"artifact_count", "subtask_count", "due_date"} {
		if _, ok := task[f]; ok {
			t.Errorf("compact contains %s", f)
		}
	}
	if task["custom_fields"] == nil {
		t.Error("populated custom fields must survive")
	}
	full := callGetTaskCompact(t, s, map[string]any{"task_id": id, "full": true})["task"].(map[string]any)
	for _, f := range []string{"artifact_count", "subtask_count", "due_date", "custom_fields"} {
		if full[f] == nil {
			t.Errorf("full lost %s", f)
		}
	}

}

func TestGetTaskWave2_InlineCommentsCompact(t *testing.T) {
	id := uuid.New().String()
	long := strings.Repeat("c", 800)
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":       []map[string]any{wave2Comment("c1", long, false)},
				"total_count": 1, "has_more": false,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": "t", "description": "d"})
	})

	res := callGetTaskCompact(t, s, map[string]any{"task_id": id, "include_comments": true})
	comments, _ := res["comments"].([]any)
	if len(comments) != 1 {
		t.Fatalf("comments = %d, want 1", len(comments))
	}
	c, _ := comments[0].(map[string]any)
	if c["body"] != long {
		t.Error("inline body must remain complete")
	}

	if _, has := c["author_id"]; has {
		t.Error("inline comments share the list_comments compact shape (no author_id)")
	}

	// full=true: inline comments verbatim.
	res = callGetTaskCompact(t, s, map[string]any{"task_id": id, "include_comments": true, "full": true})
	comments, _ = res["comments"].([]any)
	c, _ = comments[0].(map[string]any)
	if b, _ := c["body"].(string); b != long {
		t.Error("full=true must return inline comment bodies verbatim")
	}
}

// --- list_tasks -------------------------------------------------------------

func TestListTasksWave2_LimitClampedInCompactView(t *testing.T) {
	var gotPageSize []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/statuses") {
			_ = json.NewEncoder(w).Encode([]map[string]any{{"id": "s", "slug": "todo"}})
			return
		}
		gotPageSize = append(gotPageSize, r.URL.Query().Get("page_size"))
		items := make([]any, 50)
		for i := range items {
			items[i] = map[string]any{"id": strconv.Itoa(i), "title": "t", "status_id": "s"}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total_count": 200, "total_pages": 4, "page": 1})
	}))
	defer srv.Close()
	s := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}

	res, err := s.handleListTasks(context.Background(), requestWith(map[string]any{
		"project_id": "p1", "limit": 200,
	}))
	if err != nil {
		t.Fatalf("handleListTasks: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if gotPageSize[0] != "50" {
		t.Errorf("compact limit=200 went out as page_size=%q, want 50", gotPageSize[0])
	}
	if n := len(out["items"].([]any)); n > 50 {
		t.Errorf("compact response carries %d items, ceiling is %d", n, 50)
	}
	if _, has := out["limit_clamped_to"]; !has {
		t.Error("a clamped call must say so (limit_clamped_to), or paging math silently breaks")
	}

	// full=true keeps the old ceiling.
	res, err = s.handleListTasks(context.Background(), requestWith(map[string]any{
		"project_id": "p1", "limit": 200, "full": true,
	}))
	if err != nil {
		t.Fatalf("handleListTasks full: %v", err)
	}
	out = map[string]any{}
	_ = json.Unmarshal([]byte(resultText(t, res)), &out)
	if gotPageSize[1] != "200" {
		t.Errorf("full=true limit=200 went out as page_size=%q, want 200", gotPageSize[1])
	}
	if _, has := out["limit_clamped_to"]; has {
		t.Error("full=true must not be clamped")
	}
}

func TestListTasksWave2_SmallLimitUntouched(t *testing.T) {
	var gotPageSize []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPageSize = append(gotPageSize, r.URL.Query().Get("page_size"))
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "total_count": 0})
	}))
	defer srv.Close()
	s := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}

	_, err := s.handleListTasks(context.Background(), requestWith(map[string]any{
		"project_id": "p1", "limit": 30,
	}))
	if err != nil {
		t.Fatalf("handleListTasks: %v", err)
	}
	if gotPageSize[0] != "30" {
		t.Errorf("limit=30 must pass through unchanged, got page_size=%q", gotPageSize[0])
	}
}

// --- get_team_directory -----------------------------------------------------

func TestGetTeamDirectoryWave2_ZoneOneLine(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"workspace": map[string]any{"id": "w"},
			"agents": []any{map[string]any{
				"id": "a1", "name": "Linus", "role": "developer",
				"responsibility_zone": strings.Repeat("з", 200),
				"computed_status":     "online",
				"capabilities":        map[string]any{"go": true},
			}},
			"humans": []any{},
		})
	}))
	defer srv.Close()
	s := &Server{
		restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker(),
		session: &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}

	res, err := s.handleGetTeamDirectory(context.Background(), requestWith(map[string]any{}))
	if err != nil {
		t.Fatalf("handleGetTeamDirectory: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	rows, _ := out["agents"].([]any)
	row, _ := rows[0].([]any)
	zone, _ := row[3].(string)
	if n := len([]rune(zone)); n > 60 {
		t.Errorf("zone = %d runes, want ≤ %d (one line)", n, 60)
	}
	if note, _ := out["note"].(string); !strings.Contains(note, "full=true") {
		t.Error("note must name full=true as the escape hatch")
	}
}
