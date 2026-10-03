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

// list_comments defaulted to the server's own default (sort_dir=asc), so
// list_comments(task_id, limit=2) returned the OLDEST two comments of a thread
// (live red 03.10, Riker: first 2 of 17). An agent wanting "the last 3" had no
// way to ask — the natural call returned the wrong end. The default flips to
// newest-first; these tests pin the outgoing query, schema_gap-style, because a
// result-only assertion cannot tell "forwarded" from "dropped".
func TestListComments_DefaultOrderIsNewestFirst(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []any{}, "total_count": 0, "has_more": false,
		})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": uuid.New().String()}

	if _, err := server.handleListComments(context.Background(), req); err != nil {
		t.Fatalf("handleListComments: %v", err)
	}
	if !strings.Contains(gotQuery, "sort_dir=desc") {
		t.Errorf("default order must request the NEWEST comments (sort_dir=desc); query was %q", gotQuery)
	}
}

func TestListComments_ExplicitOrderForwarded(t *testing.T) {
	cases := []struct {
		order string
		want  string
	}{
		{"asc", "sort_dir=asc"},
		{"desc", "sort_dir=desc"},
		// A garbage value is forwarded and REFUSED by the API (the list_tasks
		// `order` precedent) — not silently normalized to a direction the
		// caller never asked for.
		{"sideways", "sort_dir=sideways"},
	}
	for _, tc := range cases {
		t.Run(tc.order, func(t *testing.T) {
			var gotQuery string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotQuery = r.URL.RawQuery
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"items": []any{}, "total_count": 0, "has_more": false,
				})
			}))
			defer srv.Close()

			server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = map[string]any{"task_id": uuid.New().String(), "order": tc.order}

			if _, err := server.handleListComments(context.Background(), req); err != nil {
				t.Fatalf("handleListComments: %v", err)
			}
			if !strings.Contains(gotQuery, tc.want) {
				t.Errorf("order=%q must forward %q; query was %q", tc.order, tc.want, gotQuery)
			}
		})
	}
}

// An EXPLICIT order="" must be refused HERE, not forwarded (codex-review !143,
// 03.10). Forwarding it cannot work: at the HTTP boundary `sort_dir=` is
// indistinguishable from an absent sort_dir, and the backend gate
// (evc-mesh rejectBadSortDir) deliberately reads an empty direction as "caller
// declined" and defaults to asc — the OLDEST comments, silently, for a caller
// who named a direction. Only this layer can still see the difference between
// absent and explicit-empty (hasArgument), so the refusal lives here — the
// same local-refusal precedent as get_task's out-of-range comments_limit.
func TestListComments_ExplicitEmptyOrderRefusedLocally(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []any{}, "total_count": 0, "has_more": false,
		})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": uuid.New().String(), "order": ""}

	res, err := server.handleListComments(context.Background(), req)
	if err != nil {
		t.Fatalf("handleListComments: %v", err)
	}
	if !res.IsError {
		t.Errorf("explicit order=%q must be refused locally, got a success result", "")
	}
	if called {
		t.Errorf("the refusal must happen before any HTTP call; the API was hit anyway")
	}
}

// The order flip must not regress the limit→page_size mapping the pager relies on.
func TestListComments_LimitStillForwardedAsPageSize(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []any{}, "total_count": 0, "has_more": false,
		})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": uuid.New().String(), "limit": 7}

	if _, err := server.handleListComments(context.Background(), req); err != nil {
		t.Fatalf("handleListComments: %v", err)
	}
	if !strings.Contains(gotQuery, "page_size=7") {
		t.Errorf("limit must still map to page_size; query was %q", gotQuery)
	}
}
