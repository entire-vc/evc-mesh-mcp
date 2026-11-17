package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// feedAPI mimics the API's /agents/me/tasks feed: rows ordered by created_at
// ascending, an optional status_category filter (comma list) applied before the
// page_size limit, and total_count/has_more describing the filtered set.
type feedAPI struct {
	mu      sync.Mutex
	tasks   []map[string]any // created_at ascending
	queries []string
}

func (f *feedAPI) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if strings.HasSuffix(r.URL.Path, "/statuses") {
		var statuses []any
		for _, c := range coldCategories {
			statuses = append(statuses, map[string]any{"id": "st-" + c, "slug": c, "category": c})
		}
		_ = json.NewEncoder(w).Encode(statuses)
		return
	}
	f.mu.Lock()
	f.queries = append(f.queries, r.URL.RawQuery)
	f.mu.Unlock()
	q := r.URL.Query()
	want := map[string]bool{}
	if raw := q.Get("status_category"); raw != "" {
		for _, c := range strings.Split(raw, ",") {
			want[strings.TrimSpace(c)] = true
		}
	}
	var matched []any
	for _, t := range f.tasks {
		cat := strings.TrimPrefix(t["status_id"].(string), "st-")
		if len(want) == 0 || want[cat] {
			matched = append(matched, t)
		}
	}
	page := matched
	if n, err := strconv.Atoi(q.Get("page_size")); err == nil && n > 0 && n < len(page) {
		page = page[:n]
	}
	if page == nil {
		page = []any{}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"count": len(page), "total_count": len(matched), "has_more": len(page) < len(matched), "tasks": page,
	})
}

func (f *feedAPI) add(cat string, n int) {
	for i := 0; i < n; i++ {
		k := len(f.tasks)
		f.tasks = append(f.tasks, map[string]any{
			"id": fmt.Sprintf("00000000-0000-0000-0000-%012d", k), "title": fmt.Sprintf("%s %d", cat, i),
			"status_id": "st-" + cat, "project_id": "p-1", "priority": "medium",
			"created_at": fmt.Sprintf("2026-01-01T00:%02d:%02dZ", k/60, k%60),
			"version":    float64(1), "description": "d", "has_description": true,
		})
	}
}

func feedHarness(t *testing.T, f *feedAPI) *Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(srv.Close)
	client := NewRESTClient(srv.URL, "test-key")
	return &Server{restClient: client, tracker: NewSessionTracker(), session: &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()}}
}

// A lane with a long history: 60 done cards created before its 5 live
// in_progress ones. The unfiltered first page is 50 old done cards, so a cold
// view that filtered that page reported an empty queue.
func TestHandleGetMyTasks_Cold_LongHistoryStillShowsLiveCards(t *testing.T) {
	f := &feedAPI{}
	f.add("done", 60)
	f.add("in_progress", 5)
	out := callGetMyTasks(t, feedHarness(t, f), map[string]any{})

	got := coldCount(t, out)
	if got["in_progress"] != 5 || len(out["tasks"].([]any)) != 5 {
		t.Fatalf("cold get_my_tasks must list the 5 live in_progress cards, got count=%v by category %v", out["count"], got)
	}
	if out["count"] != float64(5) {
		t.Fatalf("count must match the listed cards: %v", out["count"])
	}
	// Same set as the explicit filter.
	explicit := callGetMyTasks(t, feedHarness(t, f), map[string]any{"status_category": "in_progress"})
	if len(explicit["tasks"].([]any)) != 5 {
		t.Fatalf("explicit status_category=in_progress must list 5, got %d", len(explicit["tasks"].([]any)))
	}
}

func TestHandleGetMyTasks_Cold_RequestsOpenCategoriesOnly(t *testing.T) {
	f := &feedAPI{}
	f.add("done", 3)
	srv := feedHarness(t, f)

	callGetMyTasks(t, srv, map[string]any{})
	callGetMyTasks(t, srv, map[string]any{"full": true})
	callGetMyTasks(t, srv, map[string]any{"status_category": "done"})

	if len(f.queries) != 3 {
		t.Fatalf("expected 3 feed calls, got %v", f.queries)
	}
	cold := f.queries[0]
	if !strings.Contains(cold, "status_category=in_progress%2Creview%2Ctodo") {
		t.Fatalf("cold view must ask the API for open categories, query=%q", cold)
	}
	if strings.Contains(f.queries[1], "status_category") {
		t.Fatalf("full=true must stay unfiltered, query=%q", f.queries[1])
	}
	if !strings.Contains(f.queries[2], "status_category=done") || strings.Contains(f.queries[2], "in_progress") {
		t.Fatalf("an explicit status_category must pass through unchanged, query=%q", f.queries[2])
	}
}

func TestHandleGetMyTasks_Cold_ColdFullRollbackStaysUnfiltered(t *testing.T) {
	t.Setenv(envMyTasksCold, "full")
	f := &feedAPI{}
	f.add("done", 3)
	callGetMyTasks(t, feedHarness(t, f), map[string]any{})
	if strings.Contains(f.queries[0], "status_category") {
		t.Fatalf("COLD=full rollback must keep the previous unfiltered request, query=%q", f.queries[0])
	}
}

// More open cards than the page holds: the cold view must not hide that.
func TestHandleGetMyTasks_Cold_HasMoreReachesCaller(t *testing.T) {
	f := &feedAPI{}
	f.add("in_progress", 4)
	out := callGetMyTasks(t, feedHarness(t, f), map[string]any{"limit": 3})
	if out["has_more"] != true || out["total_count"] != float64(4) {
		t.Fatalf("has_more/total_count must reach the caller: has_more=%v total_count=%v", out["has_more"], out["total_count"])
	}
}
