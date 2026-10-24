package mcp

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// coldPage is the page the last coldActiveHarnessN serves; tests mutate it in place.
var coldPage map[string]any

func mutateColdPage(t *testing.T, f func([]any)) {
	t.Helper()
	f(coldPage["tasks"].([]any))
}

var coldCategories = []string{"backlog", "todo", "in_progress", "review", "done"}

// coldActiveHarness serves a 50-card page spread over every category (todo
// cards cycle through priorities) and the project's status list.
func coldActiveHarness(t *testing.T) *Server { return coldActiveHarnessN(t, 50, false) }

func coldActiveHarnessN(t *testing.T, n int, statusesFail bool) *Server {
	t.Helper()
	cats := coldCategories
	if n == 3 {
		cats = []string{"in_progress", "review", "todo"}
	}
	prios := []string{"low", "medium", "high", "urgent"}
	tasks := make([]any, 0, n)
	for i := 0; i < n; i++ {
		tasks = append(tasks, map[string]any{
			"id": fmt.Sprintf("00000000-0000-0000-0000-%012d", i), "title": fmt.Sprintf("card %d", i),
			"status_id": "st-" + cats[i%len(cats)], "project_id": "p-1",
			"priority": prios[i%len(prios)], "version": float64(1), "description": "d", "has_description": true,
		})
	}
	page := map[string]any{"count": n, "total_count": n, "has_more": false, "tasks": tasks}
	coldPage = page
	var statuses []any
	for _, c := range coldCategories {
		statuses = append(statuses, map[string]any{"id": "st-" + c, "slug": c, "category": c})
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/statuses") {
			if statusesFail {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
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

func coldCount(t *testing.T, out map[string]any) map[string]int {
	t.Helper()
	got := map[string]int{}
	for _, v := range out["tasks"].([]any) {
		sid := v.(map[string]any)["status_id"].(string)
		got[strings.TrimPrefix(sid, "st-")]++
	}
	return got
}

func TestHandleGetMyTasks_Cold_OnlyActivePlusFiveTodo(t *testing.T) {
	out := callGetMyTasks(t, coldActiveHarness(t), map[string]any{})
	got := coldCount(t, out)
	if got["in_progress"] != 10 || got["review"] != 10 || got["todo"] != 5 || got["backlog"] != 0 || got["done"] != 0 {
		t.Fatalf("cold must hold 10 in_progress + 10 review + 5 todo only, got %v", got)
	}
	// 10 todo cards: 3 urgent... the five kept must be the highest priority.
	for _, v := range out["tasks"].([]any) {
		m := v.(map[string]any)
		if m["status_id"] == "st-todo" && m["priority"] == "low" {
			t.Fatalf("a low todo outranked a higher one: %v", m)
		}
	}
	om, _ := out["omitted"].(map[string]any)
	if om["todo"] != float64(5) || om["backlog"] != float64(10) || om["done"] != float64(10) {
		t.Fatalf("omitted counts wrong: %v", out["omitted"])
	}
	if note, _ := out["omitted_note"].(string); !strings.Contains(note, "full=true") || !strings.Contains(note, "status_category") {
		t.Fatalf("omitted_note must say how to get the rest: %v", out["omitted_note"])
	}
	if out["total_count"] != float64(50) {
		t.Fatalf("total_count must stay complete: %v", out["total_count"])
	}
}

func TestHandleGetMyTasks_Cold_FullTrueAndExplicitCategoryUnchanged(t *testing.T) {
	srv := coldActiveHarness(t)
	if n := len(callGetMyTasks(t, srv, map[string]any{"full": true})["tasks"].([]any)); n != 50 {
		t.Fatalf("full=true must return all 50, got %d", n)
	}
	out := callGetMyTasks(t, srv, map[string]any{"status_category": "backlog"})
	if _, has := out["omitted"]; has {
		t.Fatalf("an explicit status_category is not trimmed further: %v", out["omitted"])
	}
}

func TestHandleGetMyTasks_Cold_ColdFullRollbackKeepsWholeList(t *testing.T) {
	t.Setenv(envMyTasksCold, "full")
	out := callGetMyTasks(t, coldActiveHarness(t), map[string]any{})
	if n := len(out["tasks"].([]any)); n != 50 {
		t.Fatalf("COLD=full must return the whole list, got %d", n)
	}
	if _, has := out["omitted"]; has {
		t.Fatalf("COLD=full must not carry omitted: %v", out)
	}
}

func TestHandleGetMyTasks_Cold_HitStaysUnchangedAfterActiveCold(t *testing.T) {
	srv := coldActiveHarness(t)
	args := map[string]any{"snapshot_session": "s1"}
	rev, _ := callGetMyTasks(t, srv, args)["revision"].(string)
	if rev == "" {
		t.Fatal("cold must carry a revision")
	}
	args["known_revision"] = rev
	if out := callGetMyTasks(t, srv, args); out["unchanged"] != true {
		t.Fatalf("known_revision must still hit: %v", out)
	}
}

func TestHandleGetMyTasks_Cold_UnresolvedCategoryIsNeverSilent(t *testing.T) {
	out := callGetMyTasks(t, coldActiveHarnessN(t, 50, true), map[string]any{})
	if out["omitted_unavailable"] == nil {
		t.Fatalf("a failed category lookup must be reported in the envelope: %v", out)
	}
	if _, has := out["omitted"]; has {
		t.Fatalf("an unfiltered page must not claim an omitted view: %v", out["omitted"])
	}
	if n := len(out["tasks"].([]any)); n != 50 {
		t.Fatalf("the unfiltered page is returned whole, got %d", n)
	}
}

func TestHandleGetMyTasks_Cold_NothingOmittedStillCarriesEnvelope(t *testing.T) {
	// 3 cards: backlog, todo, in_progress would omit one; use 3 active-only ones.
	out := callGetMyTasks(t, coldActiveHarnessN(t, 3, false), map[string]any{})
	if n := len(out["tasks"].([]any)); n != 3 {
		t.Fatalf("expected 3 tasks, got %d", n)
	}
	if om, ok := out["omitted"].(map[string]any); !ok || len(om) != 0 {
		t.Fatalf("omitted must be present and empty: %v", out["omitted"])
	}
	if out["omitted_note"] == nil {
		t.Fatalf("omitted_note must be present: %v", out)
	}
}

// The consumer holds the active-only cold: a delta must name only cards of
// that view, never the omitted ones.
func TestHandleGetMyTasks_Cold_DeltaStaysInsideTheColdView(t *testing.T) {
	srv := coldActiveHarnessN(t, 50, false)
	args := map[string]any{"snapshot_session": "d1"}
	cold := callGetMyTasks(t, srv, args)
	shown := map[string]bool{}
	for _, v := range cold["tasks"].([]any) {
		shown[v.(map[string]any)["id"].(string)] = true
	}
	// Change one omitted (backlog) card and one shown card upstream.
	mutateColdPage(t, func(tasks []any) {
		tasks[0].(map[string]any)["title"] = "backlog changed"
		tasks[2].(map[string]any)["title"] = "in_progress changed"
	})
	delta := callGetMyTasks(t, srv, map[string]any{"snapshot_session": "d1", "known_revision": cold["revision"], "accept_delta": true})
	if delta["delta"] != true {
		t.Fatalf("expected a delta: %v", delta)
	}
	for _, id := range delta["order"].([]any) {
		if !shown[id.(string)] {
			t.Fatalf("delta order names a card outside the cold view: %v", id)
		}
	}
	for _, v := range delta["changed"].([]any) {
		if !shown[v.(map[string]any)["id"].(string)] {
			t.Fatalf("delta carries a card outside the cold view: %v", v)
		}
	}
	if len(delta["changed"].([]any)) != 1 {
		t.Fatalf("only the shown card changed: %v", delta["changed"])
	}
}

// A status lookup that fails on the second call must not make the delta
// envelope contradict the delta membership: one lookup outcome per request.
func TestHandleGetMyTasks_Cold_DeltaAndEnvelopeShareOneLookup(t *testing.T) {
	srv := coldActiveHarness(t)
	args := map[string]any{"snapshot_session": "d2"}
	cold := callGetMyTasks(t, srv, args)
	mutateColdPage(t, func(tasks []any) { tasks[2].(map[string]any)["title"] = "changed" })
	delta := callGetMyTasks(t, srv, map[string]any{"snapshot_session": "d2", "known_revision": cold["revision"], "accept_delta": true})
	env, _ := delta["envelope"].(map[string]any)
	if env["omitted"] == nil || env["omitted_unavailable"] != nil {
		t.Fatalf("a filtered delta must carry the omitted envelope: %v", env)
	}
}
