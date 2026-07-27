package mcp

import (
	"fmt"
	"testing"
)

// mergeGraphResults' third argument is the caller's `limit`, and the merged page
// must never exceed it.
//
// It used to be a `maxBoost` cap applied on TOP of an already-full base page, so
// the ceiling was 2×limit — recall(limit=10) returned 20 rows while the response
// still echoed "limit": 10. TestMergeGraphResults_CapEnforced below asserted
// exactly that (3 base + 3 boost = 6 items), which is why the overflow survived a
// green suite: the test encoded the defect as the contract. It now asserts the
// bound instead. See task #4c65d3e2.

func makeItems(ids []string, hop float64) []any {
	items := make([]any, len(ids))
	for i, id := range ids {
		items[i] = map[string]any{"id": id, "hop_distance": hop}
	}
	return items
}

func makeIDs(prefix string, n int) []string {
	ids := make([]string, n)
	for i := range ids {
		ids[i] = fmt.Sprintf("%s%03d", prefix, i)
	}
	return ids
}

// TestMergeGraphResults_NeverExceedsLimit is the regression test. Against the
// pre-fix code every case here returns up to 2×limit.
func TestMergeGraphResults_NeverExceedsLimit(t *testing.T) {
	for _, limit := range []int{1, 2, 6, 10, 40} {
		t.Run(fmt.Sprintf("limit=%d", limit), func(t *testing.T) {
			// A full base page plus far more neighbours than could ever fit.
			base := map[string]any{
				"items": makeItems(makeIDs("base", limit), 0),
				"total": limit,
			}
			graph := map[string]any{
				"items": makeItems(makeIDs("nbr", 100), 1),
			}

			out := mergeGraphResults(base, graph, limit)

			items, _ := out["items"].([]any)
			if len(items) > limit {
				t.Errorf("merged page has %d items for limit=%d — graph boost escaped the bound", len(items), limit)
			}
			if total, ok := out["total"].(int); ok && total != len(items) {
				t.Errorf("total=%d disagrees with %d items", total, len(items))
			}
		})
	}
}

// TestMergeGraphResults_BoostDisplacesTailNotAppends pins down which rows survive.
// Neighbours take the tail of the page; the strongest base hits are kept.
func TestMergeGraphResults_BoostDisplacesTailNotAppends(t *testing.T) {
	const limit = 8
	baseIDs := makeIDs("base", limit)
	base := map[string]any{"items": makeItems(baseIDs, 0), "total": limit}
	graph := map[string]any{"items": makeItems(makeIDs("nbr", 50), 1)}

	out := mergeGraphResults(base, graph, limit)
	items, _ := out["items"].([]any)

	if len(items) != limit {
		t.Fatalf("expected a full page of %d, got %d", limit, len(items))
	}

	reserve := graphBoostReserve(limit)
	boost, _ := out["graph_boost_count"].(int)
	if boost != reserve {
		t.Errorf("graph_boost_count=%d, want the reserve %d", boost, reserve)
	}

	// The top base hits must still be there, in order, ahead of the neighbours.
	for i := 0; i < limit-reserve; i++ {
		m := items[i].(map[string]any)
		if m["id"] != baseIDs[i] {
			t.Errorf("slot %d holds %v, want the base hit %s", i, m["id"], baseIDs[i])
		}
		if _, boosted := m["graph_boost"]; boosted {
			t.Errorf("slot %d is a neighbour but should be a base hit", i)
		}
	}
	// The tail must be neighbours, marked as such.
	for i := limit - reserve; i < limit; i++ {
		m := items[i].(map[string]any)
		if boosted, _ := m["graph_boost"].(bool); !boosted {
			t.Errorf("slot %d should hold a graph-boosted neighbour, got %v", i, m["id"])
		}
	}
}

// TestMergeGraphResults_ShortBaseKeepsAllHits checks the reserve is a ceiling and
// not a quota: when the base page is not full there is nothing to displace, so no
// base hit may be dropped to make room.
func TestMergeGraphResults_ShortBaseKeepsAllHits(t *testing.T) {
	const limit = 10
	baseIDs := makeIDs("base", 3) // short page — 7 slots free
	base := map[string]any{"items": makeItems(baseIDs, 0), "total": 3}
	graph := map[string]any{"items": makeItems(makeIDs("nbr", 50), 1)}

	out := mergeGraphResults(base, graph, limit)
	items, _ := out["items"].([]any)

	if len(items) > limit {
		t.Fatalf("merged page has %d items for limit=%d", len(items), limit)
	}
	got := make(map[string]bool, len(items))
	for _, it := range items {
		got[it.(map[string]any)["id"].(string)] = true
	}
	for _, id := range baseIDs {
		if !got[id] {
			t.Errorf("base hit %s was dropped even though the page had free slots", id)
		}
	}
}

// TestMergeGraphResults_NoNeighbours_LeavesBaseUntouched guards the common path:
// with graph expansion returning nothing usable, the caller must get exactly the
// base page — the reserve must not silently shorten it.
func TestMergeGraphResults_NoNeighbours_LeavesBaseUntouched(t *testing.T) {
	const limit = 10
	base := map[string]any{"items": makeItems(makeIDs("base", limit), 0), "total": limit}

	for name, graph := range map[string]map[string]any{
		"empty":     {"items": []any{}},
		"only hop0": {"items": makeItems(makeIDs("h0", 20), 0)},
	} {
		t.Run(name, func(t *testing.T) {
			out := mergeGraphResults(base, graph, limit)
			items, _ := out["items"].([]any)
			if len(items) != limit {
				t.Errorf("base page shortened to %d with no neighbours to add", len(items))
			}
			if _, hasBoost := out["graph_boost_count"]; hasBoost {
				t.Errorf("graph_boost_count reported with no neighbours added")
			}
		})
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
	seen := make(map[string]int, len(items))
	for _, it := range items {
		seen[it.(map[string]any)["id"].(string)]++
	}
	for id, n := range seen {
		if n > 1 {
			t.Errorf("id %s appears %d times in the merged page", id, n)
		}
	}
	if seen["a"] == 0 || seen["b"] == 0 {
		t.Errorf("a duplicate neighbour displaced its own base row")
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

// TestGraphBoostReserve documents the split and pins the edges. A limit of 1 must
// never be spent on a neighbour — the caller asked for the single best hit.
func TestGraphBoostReserve(t *testing.T) {
	cases := map[int]int{
		0:  0,
		1:  0,
		2:  1,
		6:  1,
		8:  2,
		10: 2,
		40: 10,
	}
	for limit, want := range cases {
		if got := graphBoostReserve(limit); got != want {
			t.Errorf("graphBoostReserve(%d) = %d, want %d", limit, got, want)
		}
	}
}

// TestMergeGraphResults_ZeroLimit is the degenerate case: no page, no rows.
func TestMergeGraphResults_ZeroLimit(t *testing.T) {
	base := map[string]any{
		"items": makeItems([]string{"a"}, 0),
		"total": 1,
	}
	graph := map[string]any{
		"items": makeItems([]string{"b", "c"}, 1),
	}

	out := mergeGraphResults(base, graph, 0)

	if _, hasBoost := out["graph_boost_count"]; hasBoost {
		t.Errorf("expected no graph_boost_count for limit=0")
	}
	if len(out["items"].([]any)) != 1 {
		t.Errorf("expected the base page untouched for limit=0")
	}
}
