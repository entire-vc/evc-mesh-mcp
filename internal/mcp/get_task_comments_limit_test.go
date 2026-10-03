package mcp

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

// get_task(include_comments=true) inline-returned the newest DefaultPageSize
// (50) comments whole — on a busy card that is tens of KB re-read on every
// status check. The default drops to the last comments_limit (5); the full
// thread stays one explicit comments_limit away. These tests pin the outgoing
// comments query and the returned shape.
func TestGetTask_IncludeComments_DefaultPageSizeFive(t *testing.T) {
	taskID := uuid.New().String()
	var commentsQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/comments"):
			commentsQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{
					{"id": "c5", "body": "newest", "created_at": "2026-10-03T12:00:00Z"},
					{"id": "c4", "body": "x", "created_at": "2026-10-02T12:00:00Z"},
					{"id": "c3", "body": "x", "created_at": "2026-10-01T12:00:00Z"},
					{"id": "c2", "body": "x", "created_at": "2026-09-30T12:00:00Z"},
					{"id": "c1", "body": "oldest-shown", "created_at": "2026-09-29T12:00:00Z"},
				},
				"total_count": 40, "has_more": true,
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID, "title": "t", "updated_at": "2026-10-03T12:00:00Z"})
		}
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID, "include_comments": true}

	result, err := server.handleGetTask(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetTask: %v", err)
	}
	if !strings.Contains(commentsQuery, "page_size=5") {
		t.Errorf("include_comments must default to page_size=5; query was %q", commentsQuery)
	}
	if !strings.Contains(commentsQuery, "sort_dir=desc") {
		t.Errorf("must still request the newest page (sort_dir=desc); query was %q", commentsQuery)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(resultText(t, result)), &resp); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	comments, ok := resp["comments"].([]any)
	if !ok || len(comments) != 5 {
		t.Fatalf("comments = %#v, want 5 items", resp["comments"])
	}
	// Chronological order preserved (D1): first = oldest shown, last = newest.
	first, _ := comments[0].(map[string]any)
	last, _ := comments[4].(map[string]any)
	if first["id"] != "c1" || last["id"] != "c5" {
		t.Errorf("order = %v..%v, want c1..c5 (chronological)", first["id"], last["id"])
	}
	if tc, _ := resp["comments_total_count"].(float64); tc != 40 {
		t.Errorf("comments_total_count = %v, want 40", resp["comments_total_count"])
	}
	if hm, _ := resp["comments_has_more"].(bool); !hm {
		t.Error("comments_has_more must be true when the thread is longer than the limit")
	}
}

func TestGetTask_IncludeComments_ExplicitLimitForwarded(t *testing.T) {
	taskID := uuid.New().String()
	var commentsQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/comments") {
			commentsQuery = r.URL.RawQuery
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []any{}, "total_count": 0, "has_more": false,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID, "updated_at": "2026-10-03T12:00:00Z"})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID, "include_comments": true, "comments_limit": 42}

	if _, err := server.handleGetTask(context.Background(), req); err != nil {
		t.Fatalf("handleGetTask: %v", err)
	}
	if !strings.Contains(commentsQuery, "page_size=42") {
		t.Errorf("comments_limit=42 must forward page_size=42; query was %q", commentsQuery)
	}
}

// The whole-thread escape hatch is a bounded explicit value, not a magic flag:
// refuse out-of-range limits locally so a typo (comments_limit=0 / 5000) can't
// silently fall back to the server default and quietly re-fatten the response.
func TestGetTask_IncludeComments_RejectsOutOfRangeLimit(t *testing.T) {
	for _, bad := range []int{0, -1, 201, 5000} {
		t.Run(strconv.Itoa(bad), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"id": uuid.New().String()})
			}))
			defer srv.Close()

			server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = map[string]any{
				"task_id": uuid.New().String(), "include_comments": true, "comments_limit": bad,
			}
			result, err := server.handleGetTask(context.Background(), req)
			if err != nil {
				t.Fatalf("handleGetTask: %v", err)
			}
			if !result.IsError {
				t.Fatalf("comments_limit=%d must be refused, got a success result", bad)
			}
			if !strings.Contains(resultText(t, result), "comments_limit") {
				t.Errorf("refusal must name comments_limit; got %q", resultText(t, result))
			}
		})
	}
}
