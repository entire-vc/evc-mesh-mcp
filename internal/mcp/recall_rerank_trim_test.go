package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// --- keyMatchBoost / rerankRecallItems ---------------------------------------

func TestKeyMatchBoost(t *testing.T) {
	cases := []struct {
		name, key, query string
		want             int
	}{
		{"exact match", "spark-outcome-ranking-design", "spark-outcome-ranking-design", 3},
		{"query contains key", "spark-outcome-ranking-design", "tell me about spark-outcome-ranking-design please", 2},
		{"key contains query", "recall-graph-kpi-2026-06-13", "recall-graph-kpi", 2},
		{"shared token", "recall-graph-kpi-baseline", "what was the graph kpi baseline", 1},
		{"short token ignored", "p2a-bm25-rrf", "p2a and a fix", 0},
		{"no overlap", "unrelated-key-name", "totally different topic", 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := keyMatchBoost(tc.key, tc.query); got != tc.want {
				t.Errorf("keyMatchBoost(%q, %q) = %d, want %d", tc.key, tc.query, got, tc.want)
			}
		})
	}
}

// TestRerankRecallItems_KeyMatchFloatsUp is the core acceptance case: a
// lower-scored item whose key exactly matches the query must outrank items the
// server scored higher on text/vector similarity alone.
func TestRerankRecallItems_KeyMatchFloatsUp(t *testing.T) {
	items := []any{
		map[string]any{"key": "unrelated-high-score", "score": 0.02},
		map[string]any{"key": "also-unrelated", "score": 0.018},
		map[string]any{"key": "recall-threshold-design", "score": 0.005},
	}
	out := rerankRecallItems(items, "recall-threshold-design", "", "")
	if got := out[0].(map[string]any)["key"]; got != "recall-threshold-design" {
		t.Errorf("exact key match must rank first, got %v", got)
	}
}

// TestRerankRecallItems_ProjectAndScopeBoost checks the other two boost
// sources named in the acceptance criteria.
func TestRerankRecallItems_ProjectAndScopeBoost(t *testing.T) {
	items := []any{
		map[string]any{"key": "a", "score": 0.02, "project_id": "other-project", "scope": "workspace"},
		map[string]any{"key": "b", "score": 0.019, "project_id": "my-project", "scope": "project"},
	}
	out := rerankRecallItems(items, "something else entirely", "my-project", "project")
	if got := out[0].(map[string]any)["key"]; got != "b" {
		t.Errorf("project+scope match must outrank a higher base score, got %v first", got)
	}
}

// TestRerankRecallItems_StableWithinBoostTier: items with equal boost (the
// common case — boost 0 for almost everything) must keep the server's
// original relative order, since that order already encodes the real
// relevance ranking the boost has no opinion on.
func TestRerankRecallItems_StableWithinBoostTier(t *testing.T) {
	items := []any{
		map[string]any{"key": "first", "score": 0.02},
		map[string]any{"key": "second", "score": 0.015},
		map[string]any{"key": "third", "score": 0.01},
	}
	out := rerankRecallItems(items, "no overlap with any key here", "", "")
	for i, want := range []string{"first", "second", "third"} {
		if got := out[i].(map[string]any)["key"]; got != want {
			t.Errorf("slot %d = %v, want %v (order should be untouched when nothing boosts)", i, got, want)
		}
	}
}

func TestRerankRecallItems_EmptyInput(t *testing.T) {
	if out := rerankRecallItems(nil, "q", "", ""); len(out) != 0 {
		t.Errorf("rerankRecallItems(nil) = %v, want empty", out)
	}
}

// --- recallTopScore -----------------------------------------------------------

func TestRecallTopScore(t *testing.T) {
	items := []any{
		map[string]any{"key": "a", "score": 0.01},
		map[string]any{"key": "b", "score": 0.017},
		map[string]any{"key": "c", "score": 0.009},
	}
	best, ok := recallTopScore(items)
	if !ok || best != 0.017 {
		t.Errorf("recallTopScore = (%v, %v), want (0.017, true)", best, ok)
	}
}

func TestRecallTopScore_NoScoredItems(t *testing.T) {
	items := []any{map[string]any{"key": "a"}}
	if _, ok := recallTopScore(items); ok {
		t.Error("recallTopScore should report not-found when no item carries a numeric score")
	}
	if _, ok := recallTopScore(nil); ok {
		t.Error("recallTopScore(nil) should report not-found")
	}
}

// --- trimRecallItems / firstLineTruncated --------------------------------------

func TestTrimRecallItems_KeepsTopFullRestTrimmed(t *testing.T) {
	items := make([]any, 5)
	for i := range items {
		items[i] = map[string]any{
			"key":     "item",
			"content": "full content that should only survive for the top items\nsecond line",
			"score":   0.01,
			"extra":   "field that must not leak into a trimmed item",
		}
	}
	out := trimRecallItems(items)

	for i := 0; i < recallFullCount; i++ {
		m := out[i].(map[string]any)
		if _, has := m["content"]; !has {
			t.Errorf("item %d should keep full content, lost it", i)
		}
		if _, has := m["extra"]; !has {
			t.Errorf("item %d should be untouched (all fields), lost %q", i, "extra")
		}
	}
	for i := recallFullCount; i < len(out); i++ {
		m := out[i].(map[string]any)
		if _, has := m["content"]; has {
			t.Errorf("item %d should not carry full content", i)
		}
		if _, has := m["extra"]; has {
			t.Errorf("item %d should not carry unrelated fields", i)
		}
		if m["key"] != "item" {
			t.Errorf("item %d must keep its key, got %v", i, m["key"])
		}
		if m["score"] != 0.01 {
			t.Errorf("item %d must keep its score, got %v", i, m["score"])
		}
		snippet, _ := m["snippet"].(string)
		if snippet == "" {
			t.Errorf("item %d must carry a snippet", i)
		}
		if snippet != "full content that should only survive for the top items" {
			t.Errorf("snippet should be the first line only, got %q", snippet)
		}
	}
}

func TestFirstLineTruncated(t *testing.T) {
	cases := []struct{ in, want string }{
		{"one line only", "one line only"},
		{"first\nsecond\nthird", "first"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := firstLineTruncated(tc.in, 120); got != tc.want {
			t.Errorf("firstLineTruncated(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}

	long := ""
	for i := 0; i < 200; i++ {
		long += "x"
	}
	if got := firstLineTruncated(long, 120); len(got) != 120 {
		t.Errorf("firstLineTruncated did not cap at 120 chars, got len %d", len(got))
	}
}

// --- handleRecall end-to-end: threshold gate, trim, full=true ------------------

func newRecallTestServer(t *testing.T, items []any, extra map[string]any) (*Server, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{"items": items, "total": len(items)}
		for k, v := range extra {
			body[k] = v
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
	return server, srv.Close
}

func decodeRecallResult(t *testing.T, result *mcpsdk.CallToolResult) map[string]any {
	t.Helper()
	text, ok := result.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("content[0] is not TextContent, got %T", result.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
		t.Fatalf("result is not valid JSON: %v\n%s", err, text.Text)
	}
	return out
}

// TestHandleRecall_BelowThreshold_ReturnsEmptyWithExplanation is the acceptance
// case for requirement 4: no candidate clears the bar -> empty list, not the
// weakest matches, and the caller is told why.
func TestHandleRecall_BelowThreshold_ReturnsEmptyWithExplanation(t *testing.T) {
	items := []any{
		map[string]any{"key": "weak-a", "content": "barely related", "score": 0.001},
		map[string]any{"key": "weak-b", "content": "also barely related", "score": 0.0005},
	}
	server, closeFn := newRecallTestServer(t, items, map[string]any{"search_mode": "hybrid"})
	defer closeFn()

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "something unrelated"}
	result, err := server.handleRecall(context.Background(), req)
	if err != nil {
		t.Fatalf("handleRecall: %v", err)
	}
	out := decodeRecallResult(t, result)

	gotItems, _ := out["items"].([]any)
	if len(gotItems) != 0 {
		t.Errorf("expected empty items below threshold, got %d", len(gotItems))
	}
	if total, _ := out["total"].(float64); total != 0 {
		t.Errorf("expected total=0, got %v", out["total"])
	}
	explanation, _ := out["explanation"].(string)
	if explanation == "" {
		t.Error("expected a non-empty explanation when the gate empties the result")
	}
	// Diagnostic envelope fields from the server must survive the gate.
	if out["search_mode"] != "hybrid" {
		t.Errorf("search_mode should still be forwarded, got %v", out["search_mode"])
	}
}

// TestHandleRecall_GenuineLowScoreHit_SurvivesThreshold is a regression guard
// for the false-negative Garfield flagged on #7771a196 and that the 2026-09-30
// live fixture run confirmed: a query whose only real match happens to sit
// low on the RRF scale (observed as low as 0.01148 against real fleet data)
// must still come back, not get zeroed by too-aggressive a floor. If this
// starts failing, the threshold was raised back into genuine-hit territory.
func TestHandleRecall_GenuineLowScoreHit_SurvivesThreshold(t *testing.T) {
	items := []any{
		map[string]any{"key": "episode-bill-openrouter-topup", "content": "openrouter topup not needed", "score": 0.01148},
	}
	server, closeFn := newRecallTestServer(t, items, nil)
	defer closeFn()

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "openrouter topup limit"}
	result, err := server.handleRecall(context.Background(), req)
	if err != nil {
		t.Fatalf("handleRecall: %v", err)
	}
	out := decodeRecallResult(t, result)
	gotItems, _ := out["items"].([]any)
	if len(gotItems) != 1 {
		t.Fatalf("expected the genuine low-score hit to survive, got %d items (explanation: %v)", len(gotItems), out["explanation"])
	}
}

// TestHandleRecall_AboveThreshold_TrimsToTopThree covers requirements 1 and 5:
// a call with no new params gets full content for the top 3 and key+snippet+
// score for the rest — this is the default (backward-compatible) behavior.
func TestHandleRecall_AboveThreshold_TrimsToTopThree(t *testing.T) {
	items := make([]any, 6)
	for i := range items {
		items[i] = map[string]any{
			"key":     "strong-match",
			"content": "this is the full content of a relevant memory",
			"score":   0.02, // well above recallRelevanceThreshold
		}
	}
	server, closeFn := newRecallTestServer(t, items, nil)
	defer closeFn()

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "strong match"}
	result, err := server.handleRecall(context.Background(), req)
	if err != nil {
		t.Fatalf("handleRecall: %v", err)
	}
	out := decodeRecallResult(t, result)
	gotItems, _ := out["items"].([]any)
	if len(gotItems) != 6 {
		t.Fatalf("expected all 6 items to survive (trimmed, not dropped), got %d", len(gotItems))
	}
	for i, it := range gotItems {
		m := it.(map[string]any)
		_, hasContent := m["content"]
		if i < recallFullCount && !hasContent {
			t.Errorf("item %d should keep full content", i)
		}
		if i >= recallFullCount && hasContent {
			t.Errorf("item %d should be trimmed (no content field)", i)
		}
	}
}

// TestHandleRecall_Full_ReturnsEverythingFull covers requirement 2: full=true
// gets the complete text of every item, the pre-change behavior.
func TestHandleRecall_Full_ReturnsEverythingFull(t *testing.T) {
	items := make([]any, 6)
	for i := range items {
		items[i] = map[string]any{"key": "k", "content": "full text", "score": 0.02}
	}
	server, closeFn := newRecallTestServer(t, items, nil)
	defer closeFn()

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "q", "full": true}
	result, err := server.handleRecall(context.Background(), req)
	if err != nil {
		t.Fatalf("handleRecall: %v", err)
	}
	out := decodeRecallResult(t, result)
	gotItems, _ := out["items"].([]any)
	if len(gotItems) != 6 {
		t.Fatalf("expected 6 items, got %d", len(gotItems))
	}
	for i, it := range gotItems {
		if _, has := it.(map[string]any)["content"]; !has {
			t.Errorf("item %d should keep full content under full=true", i)
		}
	}
}
