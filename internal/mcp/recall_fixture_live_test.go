package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// recallTriple is one real recall call: the query, every other argument the
// caller sent, and the memory keys the session went on to use (the ground
// truth). The data file is deliberately not in this repository — it holds real
// queries and memory keys of a private workspace — and is loaded from
// MESH_LIVE_FIXTURE_FILE.
type recallTriple struct {
	Query string         `json:"query"`
	Args  map[string]any `json:"args"`
	Used  []string       `json:"used"`
	Split string         `json:"split"` // "tune" or "holdout", fixed by hash of the call
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

// recallCapture sits between the handler and the live server and keeps the
// last recall response body untouched, so "before" is the server's own order
// for exactly the request handleRecall built (profile, limit, filters included)
// instead of a re-implementation of that parameter mapping.
type recallCapture struct {
	mu   sync.Mutex
	last []byte
}

func (c *recallCapture) take() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	b := c.last
	c.last = nil
	return b
}

// newReplayHandler serves the recorded raw /memories/search responses back in
// call order (one per fixture case) and keeps each one in capture, exactly like
// the live proxy does. Any other path is 404; running past the recording is 500,
// so a snapshot that does not match the fixture fails loudly instead of cycling.
func newReplayHandler(raws [][]byte, capture *recallCapture) http.Handler {
	var mu sync.Mutex
	next := 0
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/memories/search" {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if next >= len(raws) {
			http.Error(w, "replay exhausted", http.StatusInternalServerError)
			return
		}
		b := raws[next]
		next++
		capture.mu.Lock()
		capture.last = b
		capture.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
	})
}

type splitStats struct {
	calls, empties, used, measurable, hitBefore, hitAfter, sumBefore, sumAfter, gateLosses int
}

// TestRecallFixtureLive replays real logged recall calls against a live Mesh
// server through the real handler and reports per-item top-3 hit rate before
// (server order) and after (rerank + trim) and the average response size. It
// is skipped unless MESH_LIVE_FIXTURE_URL, _WORKSPACE, _FILE and MESH_AGENT_KEY
// are all set, so it never runs in CI.
//
// A used key that the live server no longer returns at all (corpus drift since
// the call was logged) cannot say anything about ranking; it is counted and
// left out of the rates rather than counted as a miss.
func TestRecallFixtureLive(t *testing.T) {
	baseURL := os.Getenv("MESH_LIVE_FIXTURE_URL")
	key := os.Getenv("MESH_AGENT_KEY")
	wsID := os.Getenv("MESH_LIVE_FIXTURE_WORKSPACE")
	file := os.Getenv("MESH_LIVE_FIXTURE_FILE")
	replayFile := os.Getenv("MESH_LIVE_FIXTURE_REPLAY")
	if replayFile != "" {
		// Frozen-snapshot mode: no server, no key. The raw responses of an earlier
		// live run (MESH_LIVE_FIXTURE_DUMP) are served back in call order, so
		// different ranking logic can be compared on exactly the same corpus.
		if file == "" {
			file = replayFile
		}
		if wsID == "" {
			wsID = uuid.NewString()
		}
		baseURL, key = "http://replay.invalid", "replay"
	}
	if baseURL == "" || key == "" || wsID == "" || file == "" {
		t.Skip("live fixture needs MESH_LIVE_FIXTURE_URL, MESH_LIVE_FIXTURE_WORKSPACE, MESH_LIVE_FIXTURE_FILE and MESH_AGENT_KEY (or MESH_LIVE_FIXTURE_REPLAY)")
	}
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var triples []recallTriple
	if err := json.Unmarshal(data, &triples); err != nil {
		t.Fatalf("parse fixture: %v", err)
	}
	var replayRaw [][]byte
	if replayFile != "" {
		rd, err := os.ReadFile(replayFile)
		if err != nil {
			t.Fatalf("read replay snapshot: %v", err)
		}
		var rows []struct {
			Raw json.RawMessage `json:"raw"`
		}
		if err := json.Unmarshal(rd, &rows); err != nil {
			t.Fatalf("parse replay snapshot: %v", err)
		}
		if len(rows) != len(triples) {
			t.Fatalf("snapshot has %d rows, fixture has %d cases", len(rows), len(triples))
		}
		for _, r := range rows {
			replayRaw = append(replayRaw, r.Raw)
		}
	}
	if len(triples) == 0 {
		t.Fatal("fixture is empty")
	}

	target, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		t.Fatalf("bad MESH_LIVE_FIXTURE_URL: %v", err)
	}
	capture := &recallCapture{}
	proxy := &httputil.ReverseProxy{
		Director: func(r *http.Request) {
			r.URL.Scheme, r.URL.Host, r.Host = target.Scheme, target.Host, target.Host
			r.Header.Del("Accept-Encoding")
		},
		ModifyResponse: func(resp *http.Response) error {
			if strings.HasSuffix(resp.Request.URL.Path, "/memories/search") {
				b, err := io.ReadAll(resp.Body)
				if err != nil {
					return err
				}
				_ = resp.Body.Close()
				resp.Body = io.NopCloser(bytes.NewReader(b))
				capture.mu.Lock()
				capture.last = b
				capture.mu.Unlock()
			}
			return nil
		},
	}
	var handler http.Handler = proxy
	if replayFile != "" {
		handler = newReplayHandler(replayRaw, capture)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	ws := uuid.MustParse(wsID)
	s := &Server{restClient: NewRESTClient(srv.URL, key), tracker: NewSessionTracker(), session: &AgentSession{AgentID: uuid.New(), WorkspaceID: ws}}
	ctx := context.Background()

	stats := map[string]*splitStats{"tune": {}, "holdout": {}}
	// MESH_LIVE_FIXTURE_DUMP=<path> also writes every call's raw server response
	// there, so ranking changes can be tried offline without hitting the server.
	type dumpRow struct {
		Query string          `json:"query"`
		Args  map[string]any  `json:"args"`
		Used  []string        `json:"used"`
		Split string          `json:"split"`
		Raw   json.RawMessage `json:"raw"`
	}
	var dump []dumpRow
	for _, tr := range triples {
		st := stats["tune"]
		if tr.Split == "holdout" {
			st = stats["holdout"]
		}
		req := mcpsdk.CallToolRequest{}
		args := map[string]any{"query": tr.Query}
		for k, v := range tr.Args {
			args[k] = v
		}
		req.Params.Arguments = args
		res, err := s.handleRecall(ctx, req)
		if err != nil {
			t.Fatalf("handleRecall(%q): %v", tr.Query, err)
		}
		txt := res.Content[0].(mcpsdk.TextContent).Text
		var after map[string]any
		if err := json.Unmarshal([]byte(txt), &after); err != nil {
			t.Fatalf("handleRecall(%q): not JSON: %v (%.200s)", tr.Query, err, txt)
		}
		rawBody := capture.take()
		dump = append(dump, dumpRow{tr.Query, tr.Args, tr.Used, tr.Split, rawBody})
		var raw map[string]any
		if err := json.Unmarshal(rawBody, &raw); err != nil {
			t.Fatalf("captured raw response for %q: %v", tr.Query, err)
		}
		beforeKeys := keysOf(asItems(raw["items"]))
		afterItems := asItems(after["items"])
		afterKeys := keysOf(afterItems)

		st.calls++
		st.sumBefore += utf8.RuneCountInString(string(rawBody))
		st.sumAfter += utf8.RuneCountInString(txt)
		if len(afterItems) == 0 {
			st.empties++
			// A gate loss is an empty answer to a call whose raw server response
			// DID carry at least one key the session went on to use: ranking is
			// not the suspect — before-rank is known — so the gate (threshold
			// floor or noise ceiling) dropped it (2026-10-03 fixture acceptance).
			for _, want := range tr.Used {
				if rb := rankOf(beforeKeys, want); rb > 0 {
					st.gateLosses++
					t.Logf("GATE-LOSS q=%q want=%s before=%d", tr.Query, want, rb)
					break
				}
			}
		}
		for _, want := range tr.Used {
			st.used++
			rb := rankOf(beforeKeys, want)
			if rb == 0 {
				continue // drifted out of the live corpus/page
			}
			st.measurable++
			ra := rankOf(afterKeys, want)
			if rb <= 3 {
				st.hitBefore++
			}
			if ra >= 1 && ra <= 3 {
				st.hitAfter++
			}
			if ra == 0 || ra > 3 {
				t.Logf("MISS q=%q want=%s before=%d after=%d", tr.Query, want, rb, ra)
			}
		}
	}

	if p := os.Getenv("MESH_LIVE_FIXTURE_DUMP"); p != "" {
		b, _ := json.Marshal(dump)
		if err := os.WriteFile(p, b, 0o600); err != nil {
			t.Fatalf("write dump: %v", err)
		}
	}

	all := splitStats{}
	for name, st := range stats {
		t.Logf("%-7s calls=%d used=%d measurable=%d top3 before=%d after=%d | avg chars(runes) raw=%d default=%d | empty=%d gate-loss=%d",
			name, st.calls, st.used, st.measurable, st.hitBefore, st.hitAfter,
			div(st.sumBefore, st.calls), div(st.sumAfter, st.calls), st.empties, st.gateLosses)
		all.calls += st.calls
		all.used += st.used
		all.measurable += st.measurable
		all.hitBefore += st.hitBefore
		all.hitAfter += st.hitAfter
		all.sumBefore += st.sumBefore
		all.sumAfter += st.sumAfter
		all.empties += st.empties
		all.gateLosses += st.gateLosses
	}
	t.Logf("ALL     calls=%d used=%d measurable=%d top3 before=%d/%d (%d%%) after=%d/%d (%d%%) | avg chars(runes) raw=%d default=%d | empty=%d gate-loss=%d",
		all.calls, all.used, all.measurable, all.hitBefore, all.measurable, pct(all.hitBefore, all.measurable),
		all.hitAfter, all.measurable, pct(all.hitAfter, all.measurable),
		div(all.sumBefore, all.calls), div(all.sumAfter, all.calls), all.empties, all.gateLosses)

	// MESH_LIVE_FIXTURE_ENFORCE=1 turns the acceptance thresholds into
	// assertions: per-item top-3 >= 65% (on all calls and >= 62% on the hold-out
	// half alone) and average default response <= 9000 chars. Raised from 60/60
	// on 2026-10-03 when the importance tie-bands rerank moved the fixture to
	// 65%+ overall; the holdout bar keeps the un-tuned half honest.
	if os.Getenv("MESH_LIVE_FIXTURE_ENFORCE") == "1" {
		if all.measurable == 0 {
			t.Fatal("no measurable used keys: fixture does not match this corpus")
		}
		if pct(all.hitAfter, all.measurable) < 65 {
			t.Errorf("top-3 %d/%d below 65%%", all.hitAfter, all.measurable)
		}
		if h := stats["holdout"]; h.measurable > 0 && pct(h.hitAfter, h.measurable) < 62 {
			t.Errorf("hold-out top-3 %d/%d below 62%%", h.hitAfter, h.measurable)
		}
		if avg := div(all.sumAfter, all.calls); avg > 9000 {
			t.Errorf("average default response %d chars exceeds 9000", avg)
		}
		// Fixture acceptance (2026-10-03): an empty answer to a call whose raw server
		// response carried a key the session then used is a gate loss — the
		// gate dropped a real hit, whatever the average top-3 says.
		if all.gateLosses != 0 {
			t.Errorf("gate emptied %d call(s) whose used key was in the raw response", all.gateLosses)
		}
	}
}

func asItems(v any) []any {
	items, _ := v.([]any)
	return items
}

func div(a, b int) int {
	if b == 0 {
		return 0
	}
	return a / b
}

func pct(a, b int) int {
	if b == 0 {
		return 0
	}
	return a * 100 / b
}
