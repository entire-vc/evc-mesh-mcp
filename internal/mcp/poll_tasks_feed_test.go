package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

func callPollTasks(t *testing.T, server *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	result, err := server.handlePollTasks(context.Background(), req)
	if err != nil {
		t.Fatalf("handlePollTasks returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned an error result: %v", result.Content)
	}
	text, ok := result.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("content[0] is not TextContent, got %T", result.Content[0])
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(text.Text), &decoded); err != nil {
		t.Fatalf("response is not valid JSON: %v\nbody: %s", err, text.Text)
	}
	return decoded
}

// A lane with a long history: 60 done cards created before its 5 live
// in_progress ones. The unfiltered poll returned the whole feed (all 65, oldest
// first) on every wake-up and every timeout.
func TestHandlePollTasks_LongHistoryReturnsOpenCardsOnly(t *testing.T) {
	f := &feedAPI{}
	f.add("done", 60)
	f.add("in_progress", 5)
	out := callPollTasks(t, feedHarness(t, f), map[string]any{"timeout": 1})

	tasks, _ := out["tasks"].([]any)
	if len(tasks) != 5 {
		t.Fatalf("poll_tasks must return the 5 open cards, got %d (total_count=%v)", len(tasks), out["total_count"])
	}
	for _, it := range tasks {
		if st := it.(map[string]any)["status_id"]; st != "st-in_progress" {
			t.Fatalf("poll_tasks returned a closed card: status_id=%v", st)
		}
	}
	if len(f.queries) != 1 {
		t.Fatalf("expected one poll request, got %v", f.queries)
	}
	q := f.queries[0]
	for _, want := range []string{"status_category=in_progress%2Creview%2Ctodo", "page_size=50", "timeout=1"} {
		if !strings.Contains(q, want) {
			t.Fatalf("poll query must carry %q, got %q", want, q)
		}
	}
}

func TestHandlePollTasks_ExplicitFiltersPassThrough(t *testing.T) {
	f := &feedAPI{}
	f.add("done", 3)
	srv := feedHarness(t, f)

	callPollTasks(t, srv, map[string]any{"status_category": "done", "limit": 2, "project_id": "p-1"})
	callPollTasks(t, srv, map[string]any{"full": true})

	if len(f.queries) != 2 {
		t.Fatalf("expected 2 poll requests, got %v", f.queries)
	}
	explicit := f.queries[0]
	if !strings.Contains(explicit, "status_category=done") || strings.Contains(explicit, "in_progress") ||
		!strings.Contains(explicit, "page_size=2") || !strings.Contains(explicit, "project_id=p-1") {
		t.Fatalf("explicit status_category/limit/project_id must pass through unchanged, query=%q", explicit)
	}
	if strings.Contains(f.queries[1], "status_category") {
		t.Fatalf("full=true must not add the open-category default, query=%q", f.queries[1])
	}
}

func TestHandlePollTasks_ColdFullRollbackStaysUnfiltered(t *testing.T) {
	t.Setenv(envMyTasksCold, "full")
	f := &feedAPI{}
	f.add("done", 3)
	callPollTasks(t, feedHarness(t, f), map[string]any{"timeout": 1})
	if strings.Contains(f.queries[0], "status_category") {
		t.Fatalf("COLD=full rollback must keep the unfiltered poll, query=%q", f.queries[0])
	}
}

// More open cards than the page holds: the caller must see that.
func TestHandlePollTasks_HasMoreReachesCaller(t *testing.T) {
	f := &feedAPI{}
	f.add("todo", 4)
	out := callPollTasks(t, feedHarness(t, f), map[string]any{"timeout": 1, "limit": 3})
	if len(out["tasks"].([]any)) != 3 || out["has_more"] != true || out["total_count"] != float64(4) {
		t.Fatalf("limit must cap the page and has_more/total_count reach the caller: n=%d has_more=%v total_count=%v",
			len(out["tasks"].([]any)), out["has_more"], out["total_count"])
	}
}
