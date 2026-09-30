package mcp

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// recallFixtureCase is one saved recall query plus the memory keys the session
// actually used afterwards (ground truth), from the 23-30.09 audit of agent
// session logs (#7771a196).
type recallFixtureCase struct {
	query string
	used  []string
}

var recallFixture = []recallFixtureCase{
	{"test account service token", []string{"canon-no-owner-account-in-agent-tests", "pavel-decision-merge-approval-scope-billing"}},
	{"HarnessFreshnessStall fleet-tests harness prod journal isolation", []string{"sol-fleet-tests-move-status-split-and-noise-gate-01a8f26b"}},
	{"grey zone review record_auto_publish auto-publish queue Spark", []string{"spark-second-autopublish-path-bypasses-grey-zone"}},
	{"openrouter topup пополнение лимит $100", []string{"canon-leads-maintainer-openrouter-topup-limit"}},
	{"job token allowlist evc-brandkit project 9", []string{"gitlab-ci-job-token-allowlist-is-per-consumer-project"}},
	{"spark seo drift baseline", []string{"spark-drift-cwv-rules-base-rate", "learning-seo-drift-cwv-percent-threshold-no-absolute-floor"}},
	{"undershoot fix batch regeneration", []string{"episode-spark-gen-2026-09-22-undershoot-3cbcb039", "episode-spark-gen-2026-09-23-undershoot-a58fc43e"}},
	{"MeshMcpAuthFailure riker", []string{"solution-mesh-mcp-startup-auth-no-retry-silent-blackout"}},
	{"gate_service_token gate_cabinet_token internal.py периметр verified-by", []string{"decision-quinn-gitlab-account-not-workaround-47bcba25"}},
	{"spark-gen agent BROKEN stale spawn", []string{"solution-agent-broken-wave-grouping-ed6603fb"}},
}

// recallReplayCase is a REAL logged recall call (query + the parameters the
// agent actually sent) that returned one of the fixture's used keys, found by
// scanning ~/.claude/projects session logs 23-30.09 for result bodies that
// contain the key. The fixture's own query strings are paraphrases and carry no
// tags/scope/limit, which is why they cannot reproduce the logged ranks.
type recallReplayCase struct {
	query string
	args  map[string]any
	want  string
	logRk int // 0-based rank the key had in the logged response
}

var recallReplay = []recallReplayCase{
	{"undershoot fix batch regeneration", map[string]any{}, "episode-spark-gen-2026-09-22-undershoot-3cbcb039", 3},
	{"undershoot fix batch regeneration", map[string]any{}, "episode-spark-gen-2026-09-23-undershoot-a58fc43e", 4},
	{"undershoot regeneration batch", map[string]any{}, "episode-spark-gen-2026-09-23-undershoot-a58fc43e", 3},
	{"pavel decision billing receipt email", map[string]any{"tags_any": []any{"pavel-decision"}, "scope": "workspace"}, "pavel-decision-merge-approval-scope-billing", 1},
	{"pavel decision billing CI allow_failure", map[string]any{"tags_any": []any{"pavel-decision"}, "scope": "workspace", "limit": 10}, "pavel-decision-merge-approval-scope-billing", 3},
	{"pavel decision grey zone", map[string]any{"tags_any": []any{"pavel-decision"}, "scope": "workspace"}, "pavel-decision-merge-approval-scope-billing", 6},
	{"SEO drift check spark.entire.vc", map[string]any{"tags_any": []any{"pavel-decision", "solution"}}, "learning-seo-drift-cwv-percent-threshold-no-absolute-floor", 4},
	{"authed e2e drag board filters Mesh web", map[string]any{"tags_any": []any{"pavel-decision", "solution"}}, "solution-mesh-mcp-startup-auth-no-retry-silent-blackout", 4},
	{"mesh-mcp limiter X-Forwarded-For nginx trusted proxies", map[string]any{"min_importance": 0.3}, "solution-mesh-mcp-startup-auth-no-retry-silent-blackout", 7},
	{"KidCash receipt_email format", map[string]any{"tags_any": []any{"pavel-decision"}, "scope": "workspace"}, "canon-no-owner-account-in-agent-tests", 5},
	{"gate_service_token gate_cabinet_token internal.py периметр verified-by", map[string]any{"min_importance": 0, "limit": 10}, "decision-quinn-gitlab-account-not-workaround-47bcba25", 8},
}

func keysOf(items []any) []string {
	out := make([]string, 0, len(items))
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			k, _ := m["key"].(string)
			out = append(out, k)
		}
	}
	return out
}

func rankOf(keys []string, want string) int {
	for i, k := range keys {
		if k == want {
			return i + 1
		}
	}
	return 0
}

// TestRecallFixtureLive measures the fixture against a live Mesh server. It is
// skipped unless MESH_LIVE_FIXTURE_URL and MESH_AGENT_KEY are both set, so it
// never runs in CI; the output is what the acceptance comment on #67adb65a
// quotes.
func TestRecallFixtureLive(t *testing.T) {
	baseURL := os.Getenv("MESH_LIVE_FIXTURE_URL")
	key := os.Getenv("MESH_AGENT_KEY")
	wsID := os.Getenv("MESH_LIVE_FIXTURE_WORKSPACE")
	if baseURL == "" || key == "" || wsID == "" {
		t.Skip("live fixture needs MESH_LIVE_FIXTURE_URL, MESH_LIVE_FIXTURE_WORKSPACE and MESH_AGENT_KEY")
	}
	ws := uuid.MustParse(wsID)
	rc := NewRESTClient(baseURL, key)
	s := &Server{restClient: rc, tracker: NewSessionTracker(), session: &AgentSession{AgentID: uuid.New(), WorkspaceID: ws}}
	ctx := context.Background()

	call := func(q string, full bool, extra map[string]any) (map[string]any, int) {
		req := mcpsdk.CallToolRequest{}
		args := map[string]any{"query": q, "full": full}
		for k, v := range extra {
			args[k] = v
		}
		req.Params.Arguments = args
		res, err := s.handleRecall(ctx, req)
		if err != nil {
			t.Fatalf("handleRecall(%q): %v", q, err)
		}
		if len(res.Content) == 0 {
			t.Fatalf("handleRecall(%q): empty content", q)
		}
		txt := res.Content[0].(mcpsdk.TextContent).Text
		var m map[string]any
		if err := json.Unmarshal([]byte(txt), &m); err != nil {
			t.Fatalf("handleRecall(%q): not JSON: %v (%.200s)", q, err, txt)
		}
		return m, len(txt)
	}

	var items, hitBefore, hitAfter, sumBefore, sumAfter, sumFull, empties int
	for _, c := range recallFixture {
		// Before: the server's own order, as the old handler returned it.
		profile := ClassifyQuery(c.query)
		pp := GetProfileParams(profile)
		limit := 10
		if pp.Limit > 0 {
			limit = pp.Limit
		}
		raw, err := rc.RecallMemories(ctx, RecallMemoriesParams{
			Query: c.query, WorkspaceID: ws.String(), ImportanceMin: recallDefaultMinImportance,
			ApplyRecencyDecay: pp.ApplyDecay, HalfLifeDays: pp.HalfLifeDays, OrderBy: pp.OrderBy, Limit: limit,
		})
		if err != nil {
			t.Fatalf("raw recall %q: %v", c.query, err)
		}
		rawBytes, _ := json.Marshal(raw)
		rawItems, _ := raw["items"].([]any)
		beforeKeys := keysOf(rawItems)

		after, afterLen := call(c.query, false, nil)
		_, fullLen := call(c.query, true, nil)
		afterItems, _ := after["items"].([]any)
		afterKeys := keysOf(afterItems)
		if len(afterItems) == 0 {
			empties++
		}
		sumBefore += len(rawBytes)
		sumAfter += afterLen
		sumFull += fullLen

		for _, want := range c.used {
			items++
			rb, ra := rankOf(beforeKeys, want), rankOf(afterKeys, want)
			if rb >= 1 && rb <= 3 {
				hitBefore++
			}
			if ra >= 1 && ra <= 3 {
				hitAfter++
			}
			t.Logf("q=%q want=%s rank_before=%d rank_after=%d (raw n=%d after n=%d)", c.query, want, rb, ra, len(beforeKeys), len(afterKeys))
		}
		t.Logf("q=%q chars raw=%d after_default=%d after_full=%d", c.query, len(rawBytes), afterLen, fullLen)
	}
	n := len(recallFixture)
	t.Logf("RESULT per-item top3 before=%d/%d after=%d/%d | avg chars raw=%d default=%d full=%d | empty responses %d/%d",
		hitBefore, items, hitAfter, items, sumBefore/n, sumAfter/n, sumFull/n, empties, n)

	// --- where the characters go (default, non-full responses) ---
	var topChars, tailChars, topContent, nTop, nTail int
	for _, c := range recallFixture {
		after, _ := call(c.query, false, nil)
		its, _ := after["items"].([]any)
		for i, it := range its {
			b, _ := json.Marshal(it)
			if i < recallFullCount {
				topChars += len(b)
				nTop++
				if m, ok := it.(map[string]any); ok {
					cs, _ := m["content"].(string)
					topContent += len(cs)
				}
			} else {
				tailChars += len(b)
				nTail++
			}
		}
	}
	if nTop > 0 && nTail > 0 {
		t.Logf("SIZE top-%d items: avg %d chars/item (of which content %d); trimmed tail: avg %d chars/item",
			recallFullCount, topChars/nTop, topContent/nTop, tailChars/nTail)
	}

	// --- faithful replay: logged calls with their real parameters ---
	var rItems, rBefore, rAfter int
	for _, c := range recallReplay {
		pp := GetProfileParams(ClassifyQuery(c.query))
		rp := RecallMemoriesParams{Query: c.query, WorkspaceID: ws.String(), ImportanceMin: recallDefaultMinImportance,
			ApplyRecencyDecay: pp.ApplyDecay, HalfLifeDays: pp.HalfLifeDays, OrderBy: pp.OrderBy, Limit: 10}
		if v, ok := c.args["limit"].(int); ok {
			rp.Limit = v
		}
		if v, ok := c.args["min_importance"].(float64); ok {
			rp.ImportanceMin = v
		}
		if v, ok := c.args["min_importance"].(int); ok {
			rp.ImportanceMin = float64(v)
		}
		if v, ok := c.args["scope"].(string); ok {
			rp.Scope = v
		}
		if v, ok := c.args["tags_any"].([]any); ok {
			for _, x := range v {
				rp.TagsAny = append(rp.TagsAny, x.(string))
			}
		}
		raw, err := rc.RecallMemories(ctx, rp)
		if err != nil {
			t.Fatalf("replay raw %q: %v", c.query, err)
		}
		rawItems, _ := raw["items"].([]any)
		after, _ := call(c.query, false, c.args)
		afterItems, _ := after["items"].([]any)
		rb, ra := rankOf(keysOf(rawItems), c.want), rankOf(keysOf(afterItems), c.want)
		rItems++
		if rb >= 1 && rb <= 3 {
			rBefore++
		}
		if ra >= 1 && ra <= 3 {
			rAfter++
		}
		t.Logf("REPLAY q=%q want=%s logged_rank=%d live_before=%d live_after=%d", c.query, c.want, c.logRk+1, rb, ra)
	}
	t.Logf("REPLAY RESULT per-item top3 before=%d/%d after=%d/%d (logged calls, real params)", rBefore, rItems, rAfter, rItems)

	// MESH_LIVE_FIXTURE_ENFORCE=1 turns the acceptance thresholds of epic
	// c3a16d5c into assertions: per-item top-3 >= 60% and default-response
	// average <= 9000 chars. Off by default so the harness can be used to measure.
	if os.Getenv("MESH_LIVE_FIXTURE_ENFORCE") == "1" {
		if hitAfter*100 < 60*items {
			t.Errorf("fixture top-3 %d/%d is below 60%%", hitAfter, items)
		}
		if rAfter*100 < 60*rItems {
			t.Errorf("replay top-3 %d/%d is below 60%%", rAfter, rItems)
		}
		if avg := sumAfter / n; avg > 9000 {
			t.Errorf("average default response %d chars exceeds 9000", avg)
		}
	}
}
