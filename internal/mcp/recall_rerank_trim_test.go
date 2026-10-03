package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// --- keyMatchBoost / rerankRecallItems ---------------------------------------

func TestKeyMatchBoost(t *testing.T) {
	cases := []struct {
		name, key, query string
		want             int
	}{
		{"exact match", "alpha-outcome-ranking-design", "alpha-outcome-ranking-design", 3},
		{"query contains key", "alpha-outcome-ranking-design", "tell me about alpha-outcome-ranking-design please", 2},
		{"key contains query", "beta-graph-kpi-2026-01-01", "beta-graph-kpi", 2},
		{"three shared tokens", "beta-graph-threshold-baseline", "what was the beta graph threshold baseline", 1},
		{"two shared tokens are not a pointer", "beta-graph-kpi-baseline", "what was the graph baseline", 0},
		{"one shared token is not a pointer", "alpha-seo-drift-rules", "alpha anything else", 0},
		{"repeated token counts once", "mesh-mesh-mesh-mesh", "mesh topic", 0},
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

func TestCompactTopItem_DropsServiceFieldsKeepsRest(t *testing.T) {
	in := map[string]any{
		"id": "i", "key": "k", "content": "body", "score": 0.01, "tags": []any{"t"}, "scope": "workspace",
		"updated_at": "2026-09-30", "project_id": "p", "hop": 1, "graph_boost": true,
		"agent_id": "a", "workspace_id": "w", "content_simhash": 7, "freshness_score": 1.0,
		"recency_score": 1.0, "relevance": 1.0, "created_at": "c", "expires_at": "e",
		"last_accessed_at": "l", "source_type": "agent", "version": 3,
		"archived": false, "status": "active",
	}
	out := compactTopItem(in)
	for _, f := range []string{"agent_id", "workspace_id", "content_simhash", "freshness_score", "recency_score",
		"relevance", "created_at", "expires_at", "last_accessed_at", "source_type", "archived", "status"} {
		if _, has := out[f]; has {
			t.Errorf("service field %q must be dropped", f)
		}
	}
	for _, f := range []string{"id", "key", "content", "score", "tags", "scope", "updated_at", "project_id", "hop", "graph_boost", "version"} {
		if _, has := out[f]; !has {
			t.Errorf("field %q must be kept", f)
		}
	}
	if _, has := in["agent_id"]; !has {
		t.Error("input map must not be mutated")
	}
}

func TestCompactTopItem_KeepsNonDefaultArchivedAndStatus(t *testing.T) {
	out := compactTopItem(map[string]any{"key": "k", "archived": true, "status": "review_needed"})
	if out["archived"] != true || out["status"] != "review_needed" {
		t.Errorf("non-default archived/status must survive, got %v", out)
	}
}

func TestCompactTopItem_CapsContentByRunes(t *testing.T) {
	long := strings.Repeat("ж", recallTopContentChars+500)
	out := compactTopItem(map[string]any{"key": "k", "content": long})
	got, _ := out["content"].(string)
	if n := len([]rune(got)); n != recallTopContentChars {
		t.Errorf("content cut to %d runes, want %d", n, recallTopContentChars)
	}
	if !utf8.ValidString(got) {
		t.Error("cut content must stay valid UTF-8")
	}
	if out["content_truncated"] != true || out["content_chars"] != recallTopContentChars+500 {
		t.Errorf("truncation must be announced with the full length, got %v / %v", out["content_truncated"], out["content_chars"])
	}
	short := compactTopItem(map[string]any{"key": "k", "content": "short"})
	if _, has := short["content_truncated"]; has {
		t.Error("content under the cap must not be flagged")
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
// for the false-negative raised in review of the relevance threshold and that the 2026-09-30
// live fixture run confirmed: a query whose only real match happens to sit
// low on the RRF scale (observed as low as 0.01148 against real fleet data)
// must still come back, not get zeroed by too-aggressive a floor. If this
// starts failing, the threshold was raised back into genuine-hit territory.
func TestHandleRecall_GenuineLowScoreHit_SurvivesThreshold(t *testing.T) {
	items := []any{
		map[string]any{"key": "episode-agent-b-provider-topup", "content": "openrouter topup not needed", "score": 0.01148},
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

// #858f3a13: nonsense query -> dense arm returns neighbours at ~0.0115 (above
// the absolute floor) but the sparse arm matched nothing. Must come back empty.
func TestHandleRecall_NoLexicalMatchLowScore_ReturnsEmpty(t *testing.T) {
	items := []any{
		map[string]any{"key": "noise-a", "content": "x", "score": 0.01148},
		map[string]any{"key": "noise-b", "content": "y", "score": 0.0111},
	}
	server, closeFn := newRecallTestServer(t, items, map[string]any{"dense_rows": 30, "sparse_rows": 0})
	defer closeFn()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "qqqqzzzzxxxx"}
	result, err := server.handleRecall(context.Background(), req)
	if err != nil {
		t.Fatalf("handleRecall: %v", err)
	}
	out := decodeRecallResult(t, result)
	if got, _ := out["items"].([]any); len(got) != 0 {
		t.Errorf("expected empty items, got %d", len(got))
	}
	if e, _ := out["explanation"].(string); e == "" {
		t.Error("expected explanation")
	}
}

// A high-ranking dense-only hit (paraphrase, no keyword overlap) must survive.
func TestHandleRecall_NoLexicalMatchHighScore_Survives(t *testing.T) {
	items := []any{map[string]any{"key": "para", "content": "x", "score": 0.0164}}
	server, closeFn := newRecallTestServer(t, items, map[string]any{"dense_rows": 30, "sparse_rows": 0})
	defer closeFn()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "paraphrase"}
	result, _ := server.handleRecall(context.Background(), req)
	out := decodeRecallResult(t, result)
	if got, _ := out["items"].([]any); len(got) != 1 {
		t.Errorf("expected 1 item, got %d", len(got))
	}
}

// #858f3a13 live acceptance: the sparse_rows count drifts on the same garbage
// string (0, 1, 3), so the gate must hold for any count when the score is low.
func TestHandleRecall_LowScoreAnySparseCount_ReturnsEmpty(t *testing.T) {
	for _, sparse := range []int{0, 1, 3, 15} {
		t.Run(fmt.Sprintf("sparse-%d", sparse), func(t *testing.T) {
			items := []any{map[string]any{"key": "noise", "content": "x", "score": 0.011475}}
			server, closeFn := newRecallTestServer(t, items, map[string]any{"dense_rows": 30, "sparse_rows": sparse})
			defer closeFn()
			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = map[string]any{"query": "qqqqzzzzxxxx"}
			result, err := server.handleRecall(context.Background(), req)
			if err != nil {
				t.Fatalf("handleRecall: %v", err)
			}
			out := decodeRecallResult(t, result)
			if got, _ := out["items"].([]any); len(got) != 0 {
				t.Errorf("expected empty items, got %d", len(got))
			}
			if e, _ := out["explanation"].(string); e == "" {
				t.Error("expected explanation")
			}
		})
	}
}

// Positive controls: a score at/above the ceiling survives at any sparse count,
// and a server that does not report sparse_rows keeps the old behaviour.
func TestHandleRecall_HighScoreOrNoArmCounts_Survive(t *testing.T) {
	for name, tc := range map[string]struct {
		score float64
		extra map[string]any
	}{
		"high-score-sparse-0":  {0.0164, map[string]any{"dense_rows": 30, "sparse_rows": 0}},
		"high-score-sparse-3":  {0.0164, map[string]any{"dense_rows": 30, "sparse_rows": 3}},
		"at-ceiling":           {0.0120, map[string]any{"dense_rows": 30, "sparse_rows": 1}},
		"old-server-no-counts": {0.01148, map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			items := []any{map[string]any{"key": "real", "content": "x", "score": tc.score}}
			server, closeFn := newRecallTestServer(t, items, tc.extra)
			defer closeFn()
			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = map[string]any{"query": "openrouter topup"}
			result, _ := server.handleRecall(context.Background(), req)
			out := decodeRecallResult(t, result)
			if got, _ := out["items"].([]any); len(got) != 1 {
				t.Errorf("expected 1 item, got %d", len(got))
			}
		})
	}
}

// #858f3a13 acceptance fixture 9: a real identifier-only query lands at the
// same 0.01148 / sparse_rows=0 as garbage. A shared query word in an entry
// (here "verified" via the key/content) must keep it; garbage must not.
func TestHandleRecall_LowScoreWithQueryWordInEntry_Survives(t *testing.T) {
	items := []any{
		map[string]any{"key": "other", "content": "unrelated text", "score": 0.01148},
		map[string]any{"key": "decision-quinn", "content": "the verified-by label was forged", "score": 0.0112,
			"tags": []any{"kind:decision"}},
	}
	server, closeFn := newRecallTestServer(t, items, map[string]any{"dense_rows": 30, "sparse_rows": 0})
	defer closeFn()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "gate_service_token internal.py периметр verified-by"}
	result, _ := server.handleRecall(context.Background(), req)
	out := decodeRecallResult(t, result)
	if got, _ := out["items"].([]any); len(got) != 2 {
		t.Errorf("expected 2 items, got %d", len(got))
	}
}

// Same shape but the entries share no word with the query: still noise. Also a
// query of only short words cannot be judged lexically and must not be gated.
func TestHandleRecall_LowScoreNoQueryWordInEntry(t *testing.T) {
	items := []any{map[string]any{"key": "noise", "content": "unrelated text", "score": 0.01148}}
	for query, wantItems := range map[string]int{
		"zxqvbn wlkjhg qpzmxn 9f8e7d6c5b4a": 0,
		"mcp rls":                           1,
	} {
		t.Run(query, func(t *testing.T) {
			server, closeFn := newRecallTestServer(t, items, map[string]any{"dense_rows": 30, "sparse_rows": 0})
			defer closeFn()
			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = map[string]any{"query": query}
			result, _ := server.handleRecall(context.Background(), req)
			out := decodeRecallResult(t, result)
			if got, _ := out["items"].([]any); len(got) != wantItems {
				t.Errorf("expected %d items, got %d", wantItems, len(got))
			}
		})
	}
}

// Measured on the re-recorded 2026-10-03 snapshot + live garbage probes:
// the lost real queries sit at fused 0.0078–0.0098 — BELOW garbage's 0.011475
// (dense-arm rank 1 alone) — and their dense cosines (tops ~0.86) overlap the
// garbage band (0.839–0.864), so neither the noise ceiling nor any dense floor
// can separate the classes. What separates them: a query token occurs in a
// returned entry ("gotcha-verify-driver-…", "fiddlersessiondead-…",
// "solution-checkout-…"). The unconditional relevance threshold killed exactly
// those before the overlap check could run; it must respect the overlap below
// the ceiling, and still gate servers that report no arm counts.
func TestHandleRecall_ThresholdRespectsOverlapBelowCeiling(t *testing.T) {
	for name, tc := range map[string]struct {
		query     string
		items     []any
		meta      map[string]any
		wantItems int
	}{
		// The three lost fixture cases, distilled: real query, very low fused,
		// query token inside a compound key, arm counts present -> rescued.
		"rare-token-query-survives": {
			"verify driver status",
			[]any{map[string]any{"key": "gotcha-verify-driver-no-record-means-rights-gap", "content": "harness entry point", "score": 0.00782}},
			map[string]any{"dense_rows": 30, "sparse_rows": 30}, 1,
		},
		"identifier-query-survives": {
			"FiddlerSessionDead occurrence root cause",
			[]any{map[string]any{"key": "fiddlersessiondead-fleetwide-mesh-poll-gap-22sep", "content": "fleetwide poll gap", "score": 0.00978}},
			map[string]any{"dense_rows": 15, "sparse_rows": 3}, 1,
		},
		// Garbage: below the ceiling, no lexical overlap -> still empty.
		"garbage-stays-empty": {
			"qqqqzzzzxxxx",
			[]any{map[string]any{"key": "noise-a", "content": "x", "score": 0.011475}},
			map[string]any{"dense_rows": 30, "sparse_rows": 4}, 0,
		},
		"garbage-multiword-stays-empty": {
			"blorptangle fnurgle wibbleswoosh 7731",
			[]any{map[string]any{"key": "noise-b", "content": "y", "score": 0.011475}},
			map[string]any{"dense_rows": 30, "sparse_rows": 0}, 0,
		},
		// No arm counts -> the legacy threshold gates alone, as before.
		"no-arm-counts-below-threshold-empty": {
			"checkout metadata front security",
			[]any{map[string]any{"key": "solution-checkout-refresh-does-not-detect", "content": "checkout refresh", "score": 0.001}},
			nil, 0,
		},
		"no-arm-counts-above-threshold-survives": {
			"openrouter topup limit",
			[]any{map[string]any{"key": "episode-agent-b-provider-topup", "content": "openrouter topup not needed", "score": 0.01148}},
			nil, 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			server, closeFn := newRecallTestServer(t, tc.items, tc.meta)
			defer closeFn()
			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = map[string]any{"query": tc.query}
			result, err := server.handleRecall(context.Background(), req)
			if err != nil {
				t.Fatalf("handleRecall: %v", err)
			}
			out := decodeRecallResult(t, result)
			if got, _ := out["items"].([]any); len(got) != tc.wantItems {
				t.Errorf("expected %d items, got %d (explanation: %v)", tc.wantItems, len(got), out["explanation"])
			}
			if tc.wantItems == 0 {
				if e, _ := out["explanation"].(string); e == "" {
					t.Error("expected explanation on a gated-empty result")
				}
			}
		})
	}
}
