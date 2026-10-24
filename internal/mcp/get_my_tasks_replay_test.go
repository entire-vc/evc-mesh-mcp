package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// Reconstructed replay (NOT a per-call archive replay) of the 2026-10-05T16:05:43Z..2026-10-06T16:05:43Z get_my_tasks window
// (Mesh Doc 007d82fd, section "Воспроизведение и покрытие") through the code in
// this tree.
//
// The raw transcripts of that window are no longer on disk (retention): a
// rerun of toolbloat.py over it finds no get_my_tasks call. What survives is the
// aggregate packet: per status_category call counts and mean sizes, the 182
// same-session/same-args exact repeats of 243 calls, and per-field character
// totals over 591 cards. The schedule below is rebuilt from exactly those
// numbers; cards are generated with the archived per-field sizes and each
// category page is calibrated so that its OLD-shape size equals the archived
// mean (TestMyTasksReplayCalibration, red on a wrong size). Call counts, repeat
// counts and sizes are the archive's; which call of a category is the cold one
// is not recorded and is spread proportionally (bounds are printed).

type replayCategory struct {
	name     string
	filter   string // status_category argument, "" = unfiltered
	calls    int
	cards    int
	oldMean  float64 // archived mean chars (old payload, archive measure)
	cold     int     // calls that cannot be a repeat
	archived string
}

// 243 calls: 182 exact repeats, 61 first-of-signature. The 4 unfiltered 50-card
// pages and the single 8-card backlog page are unique signatures by their size;
// the other 56 colds are apportioned over in_progress/review/todo by call count.
var replayCategories = []replayCategory{
	{name: "in_progress", filter: "in_progress", calls: 89, cards: 1, oldMean: 1191.90, cold: 21},
	{name: "review", filter: "review", calls: 74, cards: 1, oldMean: 741.81, cold: 17},
	{name: "todo", filter: "todo", calls: 75, cards: 2, oldMean: 2163.31, cold: 18},
	{name: "backlog", filter: "backlog", calls: 1, cards: 8, oldMean: 6501, cold: 1},
	{name: "unfiltered", filter: "", calls: 4, cards: 50, oldMean: 27248, cold: 4},
}

// archiveChars is the archive measure: len of the tool_result content as
// json.dumps(list-of-blocks, ensure_ascii=False) (a block container, 243/243).
func archiveChars(text string) int {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(text)
	quoted := strings.TrimSuffix(buf.String(), "\n")
	return len(`[{"type": "text", "text": `) + len(quoted) + len(`}]`)
}

func replayCard(cat string, idx, titleLen int) map[string]any {
	pad := func(n int, c string) string { return strings.Repeat(c, n) }
	item := map[string]any{
		"id":              fmt.Sprintf("%08x-0000-4000-8000-%012d", idx+1, idx),
		"title":           pad(titleLen, "t"),
		"description":     pad(123, "d") + "\nsecond line of the full upstream description that the old payload cut",
		"has_description": true,
		"project_id":      "11111111-1111-4111-8111-111111111111",
		"status_id":       "st-" + cat,
		"priority":        []string{"urgent", "high", "medium"}[idx%3],
		"labels":          []any{"mesh", "mcp", "performance"},
		"assignee_name":   "Alice",
		"updated_at":      "2026-10-05T17:42:11.123456Z",
		"version":         float64(3),
		"url":             fmt.Sprintf("https://mesh.example.invalid/t/%08x-0000-4000-8000-%012d", idx+1, idx),
		"artifact_count":  float64(1),
		"subtask_count":   float64(2),
	}
	switch cat {
	case "todo", "backlog":
		item["start_after"] = "2026-10-06T00:00:00.000000Z"
		if idx%50 == 1 { // a few gated cards, as in the archive (74 of 591)
			item["checked_out_by"] = "7a307a5c-ee16-4d66-9686-9f47b436096f"
			item["checkout_token"] = "nonsecret-replay-carrier-token-00000000000000000000000000"
			item["checkout_acquired_at"] = "2026-10-05T17:00:00.000000Z"
			item["checkout_expires"] = "2026-10-05T18:00:00.000000Z"
			item["checkout_generation"] = float64(4)
			item["human_gate"] = true
			item["gate_reason"] = pad(140, "g")
			item["gate_author"] = "garfield"
			item["gate_author_type"] = "agent"
			item["gate_deadline"] = "2026-10-06T12:00:00.000000Z"
			item["human_gate_armed_at"] = "2026-10-05T17:30:00.000000Z"
			item["recommended_default"] = pad(60, "r")
			item["recurring_schedule_id"] = "22222222-2222-4222-8222-222222222222"
			item["recurring_instance_number"] = float64(12)
		}
	}
	return item
}

// oldShape renders a card the way the pre-trim code did: first line of the
// description cut at 200 runes plus description_truncated on every item.
func oldShapeSize(page []any) int {
	items := make([]any, 0, len(page))
	for _, v := range page {
		src := v.(map[string]any)
		cp := map[string]any{}
		for k, x := range src {
			cp[k] = x
		}
		d, _ := cp["description"].(string)
		first, _, truncated := strings.Cut(d, "\n")
		cp["description"] = first
		cp["description_truncated"] = truncated
		for _, k := range []string{"version", "url", "checkout_generation", "assignee_id"} {
			delete(cp, k)
		}
		items = append(items, cp)
	}
	b, _ := json.Marshal(map[string]any{"count": len(items), "total_count": len(items), "has_more": false, "tasks": items})
	return archiveChars(string(b))
}

// replayPage returns the upstream page for a category with its title filler
// solved so the old-shape size equals the archived mean.
func replayPage(c replayCategory) []any {
	build := func(titleLen int) []any {
		page := make([]any, 0, c.cards)
		cats := []string{c.filter}
		if c.filter == "" { // live unfiltered 50-card page spread over every category
			cats = []string{"in_progress", "review", "todo", "todo", "backlog", "backlog", "backlog", "done", "done", "done"}
		}
		for i := 0; i < c.cards; i++ {
			page = append(page, replayCard(cats[i%len(cats)], i, titleLen))
		}
		return page
	}
	lo, hi := 1, 600
	for lo < hi {
		mid := (lo + hi) / 2
		if float64(oldShapeSize(build(mid))) < c.oldMean {
			lo = mid + 1
		} else {
			hi = mid
		}
	}
	return build(lo)
}

func replayServer(t *testing.T, page map[string]any) *Server {
	t.Helper()
	var statuses []any
	for _, c := range coldCategories {
		statuses = append(statuses, map[string]any{"id": "st-" + c, "slug": c, "category": c})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/statuses") {
			_ = json.NewEncoder(w).Encode(statuses)
			return
		}
		_ = json.NewEncoder(w).Encode(page)
	}))
	t.Cleanup(srv.Close)
	client := NewRESTClient(srv.URL, "test-key")
	client.SetForwardedOrigin("https://mesh.example.invalid")
	return &Server{restClient: client, tracker: NewSessionTracker(), session: &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()}}
}

func rawGetMyTasks(t *testing.T, s *Server, args map[string]any) (string, map[string]any) {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleGetMyTasks(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("get_my_tasks failed: %v %v", err, res)
	}
	text := res.Content[0].(mcpsdk.TextContent).Text
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	return text, out
}

type replayStat struct{ sizes []int }

func (s *replayStat) add(n int) { s.sizes = append(s.sizes, n) }
func (s replayStat) row(name string) string {
	if len(s.sizes) == 0 {
		return fmt.Sprintf("%-8s count=%-4d mean=%-9s p95=%s", name, 0, "-", "-")
	}
	sorted := append([]int(nil), s.sizes...)
	sort.Ints(sorted)
	total := 0
	for _, n := range sorted {
		total += n
	}
	// p95 uses the archive analyzer's index rule (int(n*.95)) so it compares with the archived 2949.
	return fmt.Sprintf("%-8s count=%-4d mean=%-9.2f p95=%d", name, len(sorted), float64(total)/float64(len(sorted)), sorted[int(float64(len(sorted))*.95)])
}
func (s replayStat) mean() float64 {
	if len(s.sizes) == 0 {
		return 0
	}
	t := 0
	for _, n := range s.sizes {
		t += n
	}
	return float64(t) / float64(len(s.sizes))
}

type replayResult struct {
	cold, hit, full, delta replayStat
	all                    replayStat
}

// replayWindow runs the 243-call schedule. adoption is the share of repeat
// calls whose consumer sends known_revision; the rest are cold again.
// coldShift moves cold calls between categories for the bounds run.
func replayWindow(t *testing.T, categories []replayCategory, adoption float64) replayResult {
	t.Helper()
	var res replayResult
	for _, c := range categories {
		page := map[string]any{"count": c.cards, "total_count": c.cards, "has_more": false, "tasks": replayPage(c)}
		repeats := c.calls - c.cold
		for g := 0; g < c.cold; g++ {
			// One signature = one consumer session: cold first, then its share of repeats.
			s := replayServer(t, page)
			args := map[string]any{"snapshot_session": fmt.Sprintf("%s-%d", c.name, g)}
			if c.filter != "" {
				args["status_category"] = c.filter
			}
			text, out := rawGetMyTasks(t, s, args)
			res.cold.add(archiveChars(text))
			res.all.add(archiveChars(text))
			rev, _ := out["revision"].(string)
			if rev == "" {
				t.Fatalf("%s: cold carries no revision", c.name)
			}
			n := repeats / c.cold
			if g < repeats%c.cold {
				n++
			}
			for k := 0; k < n; k++ {
				call := map[string]any{}
				for key, v := range args {
					call[key] = v
				}
				adopted := float64(k+1) <= adoption*float64(n)
				if adopted {
					call["known_revision"] = rev
				}
				text, out := rawGetMyTasks(t, s, call)
				switch {
				case adopted && out["unchanged"] == true:
					res.hit.add(archiveChars(text))
				case adopted:
					t.Fatalf("%s: repeat on the same revision was not unchanged: %.200s", c.name, text)
				default:
					res.cold.add(archiveChars(text))
				}
				res.all.add(archiveChars(text))
			}
		}
		// The archive holds no full and no delta call (full=0, every repeat exact),
		// so these are probes on a private copy of the page, reported in their own
		// rows and kept out of ALL: one full=true bypass and one mutation answered
		// as delta per category.
		var copyPage map[string]any
		b, _ := json.Marshal(page)
		_ = json.Unmarshal(b, &copyPage)
		s := replayServer(t, copyPage)
		args := map[string]any{"snapshot_session": c.name + "-probe"}
		if c.filter != "" {
			args["status_category"] = c.filter
		}
		_, out := rawGetMyTasks(t, s, args)
		rev, _ := out["revision"].(string)
		fullArgs := map[string]any{"full": true, "known_revision": rev}
		for k, v := range args {
			fullArgs[k] = v
		}
		text, got := rawGetMyTasks(t, s, fullArgs)
		if got["unchanged"] == true || got["revision"] != nil {
			t.Fatalf("%s: full=true must bypass revision, got %.120s", c.name, text)
		}
		res.full.add(archiveChars(text))
		copyPage["tasks"].([]any)[0].(map[string]any)["version"] = float64(4)
		copyPage["tasks"].([]any)[0].(map[string]any)["title"] = "mutated"
		deltaArgs := map[string]any{"known_revision": rev, "accept_delta": true}
		for k, v := range args {
			deltaArgs[k] = v
		}
		text, got = rawGetMyTasks(t, s, deltaArgs)
		if got["delta"] != true {
			t.Fatalf("%s: mutation with accept_delta must answer delta, got %.120s", c.name, text)
		}
		res.delta.add(archiveChars(text))
	}
	return res
}

func TestMyTasksReplayCalibration(t *testing.T) {
	for _, c := range replayCategories {
		got := float64(oldShapeSize(replayPage(c)))
		if d := (got - c.oldMean) / c.oldMean; d > 0.03 || d < -0.03 {
			t.Errorf("%s: calibrated old-shape size %.0f is %.1f%% off archived mean %.2f", c.name, got, d*100, c.oldMean)
		}
	}
	// Red control: a page of the wrong size must be rejected by the same bound.
	wrong := replayCategories[0]
	wrong.oldMean = 5000
	if d := (float64(oldShapeSize(replayPage(replayCategories[0]))) - wrong.oldMean) / wrong.oldMean; d < 0.03 && d > -0.03 {
		t.Fatal("calibration check accepts a wrong size")
	}
}

func TestMyTasksReconstructedWindowReplay(t *testing.T) {
	total := 0
	for _, c := range replayCategories {
		total += c.calls
	}
	if total != 243 {
		t.Fatalf("schedule has %d calls, archive has 243", total)
	}
	repeats := 0
	for _, c := range replayCategories {
		repeats += c.calls - c.cold
	}
	if repeats != 182 {
		t.Fatalf("schedule has %d exact repeats, archive has 182", repeats)
	}
	// Archived baseline over the same schedule, old compact payloads.
	var old replayStat
	for _, c := range replayCategories {
		for i := 0; i < c.calls; i++ {
			old.add(int(c.oldMean + 0.5))
		}
	}
	t.Logf("baseline (archive, old compact): count=%d mean=%.2f p95=2949 (archived p95)", len(old.sizes), old.mean())
	for _, adoption := range []float64{1, 0.75, 0.5, 0} {
		r := replayWindow(t, replayCategories, adoption)
		t.Logf("adoption=%.0f%%", adoption*100)
		t.Logf("  %s", r.full.row("full"))
		t.Logf("  %s", r.cold.row("cold"))
		t.Logf("  %s", r.hit.row("hit"))
		t.Logf("  %s", r.delta.row("delta"))
		t.Logf("  %s   target<=1280: cold part %.2f, hit part %.2f",
			r.all.row("ALL"), r.cold.mean()*float64(len(r.cold.sizes))/float64(len(r.all.sizes)), r.hit.mean()*float64(len(r.hit.sizes))/float64(len(r.all.sizes)))
		if adoption == 1 && r.all.mean() > 1280 {
			t.Errorf("mean %.2f above the 1280 target at full adoption", r.all.mean())
		}
	}
	// Bounds for the unrecorded split of the 56 non-pinned colds: all in the
	// largest category (todo) and all in the smallest (review).
	for _, name := range []string{"todo", "review"} {
		cats := append([]replayCategory(nil), replayCategories...)
		for i := range cats {
			if cats[i].name == "in_progress" || cats[i].name == "review" || cats[i].name == "todo" {
				cats[i].cold = 1
			}
		}
		spare := 56 - 3
		for i := range cats {
			if cats[i].name == name {
				cats[i].cold += spare
			}
		}
		r := replayWindow(t, cats, 1)
		t.Logf("bound, all spare colds in %s: %s", name, r.all.row("ALL"))
	}
}
