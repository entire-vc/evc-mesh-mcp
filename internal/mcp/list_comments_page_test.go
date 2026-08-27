package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestListCommentsTool_ExposesPageParam pins the schema half of the fix:
// list_comments' input schema had no `page` field at all, even though the
// handler-side bug (below) meant a caller-supplied page was silently dropped
// regardless. Both halves must be fixed or the parameter is unreachable.
func TestListCommentsTool_ExposesPageParam(t *testing.T) {
	server := NewServer(ServerConfig{})

	tool, ok := server.MCPServer().ListTools()["list_comments"]
	if !ok {
		t.Fatal("list_comments tool is not registered")
	}
	props := tool.Tool.InputSchema.Properties

	raw, present := props["page"]
	if !present {
		t.Fatal("list_comments must accept page, otherwise a thread longer than `limit` has no way to read past the first page")
	}
	spec, _ := raw.(map[string]any)
	desc, _ := spec["description"].(string)
	if strings.TrimSpace(desc) == "" {
		t.Error("page needs a description: an undocumented parameter is one nobody passes")
	}

	for _, req := range tool.Tool.InputSchema.Required {
		if req == "page" {
			t.Error("page must stay optional — a fresh read (page 1) has no prior page to send")
		}
	}
}

// TestListCommentsTool_PageReachesTheRequest is the actual repro from
// #3a8d3d9c: list_comments(task_id, limit=2, page=4) returned the FIRST page
// every time — the handler read `limit` into `page_size` but never read
// `page` at all, so the REST call always went out as `page_size=N` with no
// `page` query param, and the server default (page 1) silently won. The
// response's own `has_more`/`total_pages` looked like a working pager, which
// is what made the loss invisible.
//
// This asserts on the OUTGOING REST query, not just on the returned page
// number — the response could coincidentally read "page": 1 on a genuine
// first page too. What must never happen again is a page=4 request leaving
// with no page param.
func TestListCommentsTool_PageReachesTheRequest(t *testing.T) {
	var gotQueries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQueries = append(gotQueries, r.URL.RawQuery)
		page := r.URL.Query().Get("page")
		if page == "" {
			page = "1"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items":       []map[string]any{{"id": "page-" + page}},
			"total_count": 8,
			"page":        page,
			"has_more":    page != "4",
		})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	ctx := context.Background()

	// Call 1: no page at all — the ordinary, never-broken path. Must NOT
	// regress into always sending page=1 explicitly or anything else that
	// changes today's default-page behavior.
	firstResult, err := server.handleListComments(ctx, requestWith(map[string]any{
		"task_id": "task-1", "limit": 2,
	}))
	if err != nil {
		t.Fatalf("call without page: transport error: %v", err)
	}
	if firstResult.IsError {
		t.Fatalf("call without page: unexpected error result: %s", resultText(t, firstResult))
	}
	if strings.Contains(gotQueries[0], "page=") {
		t.Errorf("call without page: query = %q, must not contain page= when the caller sent none", gotQueries[0])
	}

	// Call 2: the actual repro — page=4 must reach the REST request as
	// page=4, and the response must reflect page 4's item, not page 1's.
	secondResult, err := server.handleListComments(ctx, requestWith(map[string]any{
		"task_id": "task-1", "limit": 2, "page": 4,
	}))
	if err != nil {
		t.Fatalf("call with page=4: transport error: %v", err)
	}
	if secondResult.IsError {
		t.Fatalf("call with page=4: unexpected error result: %s", resultText(t, secondResult))
	}
	if !strings.Contains(gotQueries[1], "page=4") {
		t.Fatalf("call with page=4: expected page=4 in the outgoing REST query, got %q — the parameter is being dropped again", gotQueries[1])
	}

	var page4 struct {
		Items []struct {
			ID string `json:"id"`
		} `json:"items"`
		Page string `json:"page"`
	}
	if err := json.Unmarshal([]byte(resultText(t, secondResult)), &page4); err != nil {
		t.Fatalf("call with page=4: could not decode response JSON: %v", err)
	}
	if page4.Page != "4" || len(page4.Items) != 1 || page4.Items[0].ID != "page-4" {
		t.Fatalf("call with page=4: got page=%q items=%v, want page 4's own item — a caller asking for page 4 must not silently receive page 1", page4.Page, page4.Items)
	}
}
