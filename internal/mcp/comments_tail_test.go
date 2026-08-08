package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestRESTClient_GetTaskComments_RequestsNewestPageAndRestoresChronologicalOrder
// pins D1 (ported from entire-vc/evc-mesh task 4222c17d): get_task
// (include_comments=true) used to request the server's untouched default
// (oldest DefaultPageSize, ASC) with no way to reach the tail of a long
// thread. The fix requests sort_dir=desc (the newest page) and reverses it
// back to chronological order for display — this test asserts both halves:
// the outgoing request shape, and the returned order.
func TestRESTClient_GetTaskComments_RequestsNewestPageAndRestoresChronologicalOrder(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"items": []map[string]any{
				{"id": "c3", "body": "newest", "created_at": "2026-08-05T00:00:00Z"},
				{"id": "c2", "body": "middle", "created_at": "2026-08-01T00:00:00Z"},
				{"id": "c1", "body": "oldest", "created_at": "2026-06-21T00:00:00Z"},
			},
			"total_count": 106,
			"has_more":    true,
			"page":        1,
			"page_size":   50,
		})
	}))
	defer srv.Close()

	c := NewRESTClient(srv.URL, "test-key")
	result, err := c.GetTaskComments(context.Background(), "8a98ddd3-566c-4e81-b2d1-b92103d2ef03")
	if err != nil {
		t.Fatalf("GetTaskComments returned error: %v", err)
	}

	if want := "include_internal=true&sort_dir=desc"; gotQuery != want {
		t.Errorf("query = %q, want %q (must request the newest page, not the server default)", gotQuery, want)
	}

	items, ok := result["items"].([]any)
	if !ok || len(items) != 3 {
		t.Fatalf("items = %#v, want 3 items", result["items"])
	}
	idOf := func(v any) string {
		m, _ := v.(map[string]any)
		id, _ := m["id"].(string)
		return id
	}
	if got := idOf(items[0]); got != "c1" {
		t.Errorf("items[0].id = %q, want %q (oldest of the returned page must come first)", got, "c1")
	}
	if got := idOf(items[2]); got != "c3" {
		t.Errorf("items[2].id = %q, want %q (newest of the returned page must be last)", got, "c3")
	}

	if tc, _ := result["total_count"].(float64); tc != 106 {
		t.Errorf("total_count = %v, want 106 (envelope must survive)", result["total_count"])
	}
	if hm, _ := result["has_more"].(bool); !hm {
		t.Errorf("has_more = %v, want true (envelope must survive)", result["has_more"])
	}
}

// TestRESTClient_GetTaskComments_EmptyAndSingleItem guards the reverse-in-place
// two-pointer swap against its classic off-by-one failure modes.
func TestRESTClient_GetTaskComments_EmptyAndSingleItem(t *testing.T) {
	t.Run("empty", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": []map[string]any{}, "total_count": 0, "has_more": false,
			})
		}))
		defer srv.Close()
		c := NewRESTClient(srv.URL, "test-key")
		result, err := c.GetTaskComments(context.Background(), "no-comments-task")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		items, ok := result["items"].([]any)
		if !ok || len(items) != 0 {
			t.Errorf("items = %#v, want empty slice", result["items"])
		}
	})

	t.Run("single item", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items":       []map[string]any{{"id": "only", "body": "hi"}},
				"total_count": 1, "has_more": false,
			})
		}))
		defer srv.Close()
		c := NewRESTClient(srv.URL, "test-key")
		result, err := c.GetTaskComments(context.Background(), "one-comment-task")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		items, ok := result["items"].([]any)
		if !ok || len(items) != 1 {
			t.Fatalf("items = %#v, want 1 item", result["items"])
		}
		m, _ := items[0].(map[string]any)
		if m["id"] != "only" {
			t.Errorf("items[0].id = %v, want %q", m["id"], "only")
		}
	})
}

// TestRESTClient_GetTaskComments_PropagatesError ensures a failed request
// surfaces as an error rather than being swallowed.
func TestRESTClient_GetTaskComments_PropagatesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "task not found"})
	}))
	defer srv.Close()

	c := NewRESTClient(srv.URL, "test-key")
	result, err := c.GetTaskComments(context.Background(), "missing-task")
	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if result != nil {
		t.Errorf("result = %#v, want nil on error", result)
	}
}
