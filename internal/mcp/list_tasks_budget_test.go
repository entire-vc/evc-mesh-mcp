package mcp

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
)

// twentyRowFixture is a full page of live-shaped cards (long titles, labels,
// parent ids), the shape behind the 5079-char average of limit=20 pages.
func twentyRowFixture() map[string]any {
	fx := listTasksEnvelopeFixture()
	base := fx["items"].([]any)[0].(map[string]any)
	items := make([]any, 0, 20)
	for i := 0; i < 20; i++ {
		it := map[string]any{}
		for k, v := range base {
			it[k] = v
		}
		it["id"] = uuid.New().String()
		it["title"] = fmt.Sprintf("[Runtime R%d] Mesh: ready/running/waiting, triage human gate и ёмкость исполнения", i)
		it["labels"] = []any{"fleet-eng", "runtime-routing", "pin-assignee"}
		it["parent_task_id"] = uuid.New().String()
		items = append(items, it)
	}
	fx["items"] = items
	fx["total_count"] = 40
	fx["total_pages"] = 2
	return fx
}

func TestListTasks_CompactPageStaysUnderBudget(t *testing.T) {
	projectID := uuid.New().String()
	server := listTasksHarness(t, "/api/v1/projects/"+projectID+"/tasks", twentyRowFixture())
	out := callListTasks(t, server, map[string]any{"project_id": projectID, "limit": 20})
	b, _ := json.Marshal(out)
	if len(b) > 3000 {
		t.Fatalf("20-row compact page is %d chars, want <= %d", len(b), 3000)
	}
	rows := out["items"].([]any)
	if int(out["truncated_to"].(float64)) != len(rows) || len(rows) < 5 {
		t.Fatalf("truncated_to=%v rows=%d", out["truncated_to"], len(rows))
	}
	next := out["next"].(map[string]any)
	if next["limit"] != float64(len(rows)) || next["page"] != float64(2) {
		t.Fatalf("page 1 must continue at limit=%d page=2, got %v", len(rows), next)
	}
}

func TestListTasks_FullTrueIsNeverCapped(t *testing.T) {
	projectID := uuid.New().String()
	server := listTasksHarness(t, "/api/v1/projects/"+projectID+"/tasks", twentyRowFixture())
	out := callListTasks(t, server, map[string]any{"project_id": projectID, "limit": 20, "full": true})
	if len(out["items"].([]any)) != 20 || out["truncated_to"] != nil {
		t.Fatalf("full=true must return all 20 rows untouched: %v", out["truncated_to"])
	}
}

func TestCapListTasksPage_ContinuationIsExact(t *testing.T) {
	for _, tc := range []struct{ limit, page int }{{20, 1}, {20, 2}, {20, 3}, {50, 2}, {25, 3}} {
		items := make([]any, tc.limit)
		for i := range items {
			items[i] = map[string]any{"id": uuid.New().String(), "title": "a fairly long title for a card in the list, long enough that a page of them cannot fit one budget"}
		}
		page := map[string]any{"items": items}
		capListTasksPage(page, tc.limit, tc.page)
		k, ok := page["truncated_to"].(int)
		if !ok {
			t.Fatalf("limit=%d page=%d: page was not capped", tc.limit, tc.page)
		}
		next := page["next"].(map[string]any)
		start := (tc.page - 1) * tc.limit
		if next["limit"].(int) != k || (next["page"].(int)-1)*k != start+k {
			t.Errorf("limit=%d page=%d: next=%v does not begin at row %d", tc.limit, tc.page, next, start+k)
		}
		if k < 1 || k >= tc.limit {
			t.Errorf("limit=%d page=%d: kept %d", tc.limit, tc.page, k)
		}
	}
}

func TestCapListTasksPage_ZeroLimitNeverGuessesOffset(t *testing.T) {
	mk := func() map[string]any {
		items := make([]any, 25)
		for i := range items {
			items[i] = map[string]any{"id": uuid.New().String(), "title": "a fairly long title for a card in the list, long enough that a page of them cannot fit one budget"}
		}
		return map[string]any{"items": items}
	}
	later := mk()
	capListTasksPage(later, 0, 3)
	if later["truncated_to"] != nil || len(later["items"].([]any)) != 25 {
		t.Fatalf("limit=0 page>1 must be returned whole, got truncated_to=%v", later["truncated_to"])
	}
	first := mk()
	capListTasksPage(first, 0, 1)
	k, ok := first["truncated_to"].(int)
	if !ok || first["next"].(map[string]any)["page"] != 2 || first["next"].(map[string]any)["limit"] != k {
		t.Fatalf("limit=0 page 1 must continue at (limit=k, page=2): %v", first["next"])
	}
}
