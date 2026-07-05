package mcp

import (
	"testing"
)

func makeItems(ids []string, hop float64) []any {
	items := make([]any, len(ids))
	for i, id := range ids {
		items[i] = map[string]any{"id": id, "hop_distance": hop}
	}
	return items
}

func TestMergeGraphResults_CapEnforced(t *testing.T) {
	base := map[string]any{
		"items": makeItems([]string{"a", "b", "c"}, 0),
		"total": 3,
	}
	// Graph returns 100 hop>0 neighbors — should be capped at maxBoost=3.
	graphIDs := make([]string, 100)
	for i := range graphIDs {
		graphIDs[i] = string(rune('d' + i))
	}
	graph := map[string]any{
		"items": makeItems(graphIDs, 1),
	}

	out := mergeGraphResults(base, graph, 3)

	items := out["items"].([]any)
	if len(items) != 6 { // 3 base + 3 capped graph
		t.Errorf("expected 6 items, got %d", len(items))
	}
	if boost, _ := out["graph_boost_count"].(int); boost != 3 {
		t.Errorf("expected graph_boost_count=3, got %v", out["graph_boost_count"])
	}
}

func TestMergeGraphResults_NoDuplicates(t *testing.T) {
	base := map[string]any{
		"items": makeItems([]string{"a", "b"}, 0),
		"total": 2,
	}
	// Graph has hop>0 items, two of which duplicate base IDs.
	graphItems := []any{
		map[string]any{"id": "a", "hop_distance": float64(1)}, // duplicate
		map[string]any{"id": "c", "hop_distance": float64(1)}, // new
		map[string]any{"id": "b", "hop_distance": float64(1)}, // duplicate
		map[string]any{"id": "d", "hop_distance": float64(1)}, // new
	}
	graph := map[string]any{"items": graphItems}

	out := mergeGraphResults(base, graph, 10)

	items := out["items"].([]any)
	if len(items) != 4 { // 2 base + 2 new (c, d)
		t.Errorf("expected 4 items, got %d", len(items))
	}
}

func TestMergeGraphResults_Hop0ItemsSkipped(t *testing.T) {
	base := map[string]any{
		"items": makeItems([]string{"a"}, 0),
		"total": 1,
	}
	// Graph has hop=0 items (already in base by definition) — must be skipped.
	graph := map[string]any{
		"items": makeItems([]string{"b", "c"}, 0),
	}

	out := mergeGraphResults(base, graph, 10)

	// No boost since all graph items have hop=0.
	if _, hasBoost := out["graph_boost_count"]; hasBoost {
		t.Errorf("expected no graph_boost_count when all graph items are hop=0")
	}
	if len(out["items"].([]any)) != 1 {
		t.Errorf("expected 1 item (base only) when all graph items are hop=0")
	}
}

func TestMergeGraphResults_ZeroMaxBoost(t *testing.T) {
	base := map[string]any{
		"items": makeItems([]string{"a"}, 0),
		"total": 1,
	}
	graph := map[string]any{
		"items": makeItems([]string{"b", "c"}, 1),
	}

	out := mergeGraphResults(base, graph, 0)

	// maxBoost=0 → no graph items added.
	if _, hasBoost := out["graph_boost_count"]; hasBoost {
		t.Errorf("expected no graph_boost_count for maxBoost=0")
	}
	if len(out["items"].([]any)) != 1 {
		t.Errorf("expected 1 item (base only) for maxBoost=0")
	}
}
