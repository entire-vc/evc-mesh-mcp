package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// shortIDFixture is the single source of truth for the short-ID harness: the
// short prefix, the full UUID it resolves to, and the child-resource payloads
// every include_* read must surface identically whether get_task was called
// with the short form or the full one.
type shortIDFixture struct {
	shortID string
	fullID  string
	// scheduleID is optional: when set, the /context task carries recurring
	// fields and the harness serves the schedule's history — that is what
	// get_task_context's recurring enrichment reads. Empty (the default
	// newShortIDFixture) means no recurring wiring at all.
	scheduleID string
}

func newShortIDFixture() shortIDFixture {
	full := uuid.New()
	// A 6-hex prefix of the full UUID — the shortest form the get_task
	// contract promises ("full UUID or 6–12 char hex short-ID prefix").
	return shortIDFixture{shortID: strings.ReplaceAll(full.String(), "-", "")[:6], fullID: full.String()}
}

// getTaskShortIDHarness stands up a fake Mesh REST API that is faithful to
// the live backend's mixed short-ID tolerance:
//
//   - GET /tasks/by-short-id/:prefix resolves the short form to the full task
//     (this is how RESTClient.GetTask maps a prefix onto a task);
//   - GET /tasks/:id/comments, /artifacts, /vcs-links accept both the short
//     prefix and the full UUID (measured live 02.10: include_comments with a
//     short ID works);
//   - GET /tasks/:id/dependencies parses the path id as a STRICT UUID and
//     answers 400 "invalid task_id" for anything else (measured live 02.10:
//     the red this file exists for).
//
// Every requested path is recorded so tests can assert on the wire — which
// form of the id each child read actually used — not just on the decoded
// response.
func getTaskShortIDHarness(t *testing.T, fx shortIDFixture, resolve func(short string) (map[string]any, int)) (*Server, func() []string) {
	t.Helper()
	var mu sync.Mutex
	var paths []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		mu.Lock()
		paths = append(paths, r.Method+" "+r.URL.Path)
		mu.Unlock()

		taskObj := map[string]any{
			"id":         fx.fullID,
			"title":      "short id child reads",
			"updated_at": "2026-10-02T12:00:00Z",
		}
		comments := map[string]any{
			// Wire order is newest-first (GetTaskComments requests sort_dir=desc
			// and reverses client-side) — mirror that so the reversal round-trips.
			"items": []map[string]any{
				{"id": "c2", "body": "second", "created_at": "2026-10-02T11:00:00Z"},
				{"id": "c1", "body": "first", "created_at": "2026-10-02T10:00:00Z"},
			},
			"total_count": 2,
			"has_more":    false,
		}
		artifacts := map[string]any{
			"items": []map[string]any{
				{"id": "a1", "name": "proof.txt", "artifact_type": "log"},
			},
			"total_count": 1,
			"has_more":    false,
		}
		deps := map[string]any{
			"outgoing": []map[string]any{
				{"task_id": fx.fullID, "depends_on_task_id": "89d36289-0000-0000-0000-000000000000", "dependency_type": "blocks"},
			},
			"incoming": []map[string]any{},
		}
		// The /context payload mirrors the task object (plus recurring fields
		// when the fixture carries a schedule) and bundles the child
		// collections the get_task_context contract promises in one call.
		contextTask := map[string]any{
			"id":         fx.fullID,
			"title":      "short id child reads",
			"updated_at": "2026-10-02T12:00:00Z",
		}
		if fx.scheduleID != "" {
			contextTask["recurring_schedule_id"] = fx.scheduleID
			contextTask["recurring_instance_number"] = float64(2)
		}
		contextPayload := map[string]any{
			"task":         contextTask,
			"comments":     comments["items"],
			"artifacts":    artifacts["items"],
			"dependencies": deps["outgoing"],
			"activity":     []map[string]any{{"id": "ev1", "event_type": "status_change"}},
		}
		vcsLinks := map[string]any{
			"vcs_links": []map[string]any{
				{"id": "v1", "task_id": fx.fullID, "provider": "gitlab", "link_type": "pr", "external_id": "14", "status": "merged"},
			},
			"count": 1,
		}

		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/tasks/by-short-id/"):
			// Key every resolution decision on the prefix the CALLER asked
			// for, not on the fixture's own short ID: the resolve callback
			// models the backend's decision for arbitrary prefixes (404
			// unknown vs 400 ambiguous). With resolve unset, only the
			// fixture's prefix resolves — anything else is a 404, like a
			// backend that knows exactly one task.
			short := strings.TrimPrefix(r.URL.Path, "/api/v1/tasks/by-short-id/")
			if resolve != nil {
				if body, code := resolve(short); code != 200 {
					w.WriteHeader(code)
					_ = json.NewEncoder(w).Encode(body)
					return
				}
			} else if short != fx.shortID {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": 404, "message": "task not found"})
				return
			}
			_ = json.NewEncoder(w).Encode(taskObj)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+fx.fullID:
			_ = json.NewEncoder(w).Encode(taskObj)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+fx.fullID+"/comments",
			r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+fx.shortID+"/comments":
			_ = json.NewEncoder(w).Encode(comments)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+fx.fullID+"/artifacts",
			r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+fx.shortID+"/artifacts":
			_ = json.NewEncoder(w).Encode(artifacts)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+fx.fullID+"/vcs-links",
			r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+fx.shortID+"/vcs-links":
			_ = json.NewEncoder(w).Encode(vcsLinks)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+fx.fullID+"/dependencies":
			_ = json.NewEncoder(w).Encode(deps)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+fx.shortID+"/dependencies":
			// Faithful to the live backend: the dependencies handler parses the
			// path id as a strict UUID; a 6-hex prefix is a 400, not a lookup.
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 400, "message": "invalid task_id"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+fx.fullID+"/context":
			_ = json.NewEncoder(w).Encode(contextPayload)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+fx.shortID+"/context":
			// Faithful to the live backend: the context route parses the path
			// id as a strict UUID too (live red 02.10 on #e143d391:
			// get_task_context('06d0c6d8') → "Bad Request: invalid task_id").
			w.WriteHeader(http.StatusBadRequest)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 400, "message": "invalid task_id"})
		case fx.scheduleID != "" && r.Method == http.MethodGet && r.URL.Path == "/api/v1/recurring/"+fx.scheduleID+"/history":
			_ = json.NewEncoder(w).Encode(map[string]any{
				// Newest-first: instance 2 is the current one the fixture task
				// points at, instance 1 is the previous get_task_context must
				// surface as previous_instance.
				"items": []map[string]any{
					{"instance_number": float64(2), "summary": "current instance"},
					{"instance_number": float64(1), "summary": "previous instance"},
				},
			})
		default:
			// Unknown path (including an unresolvable prefix hitting /tasks/<short>
			// directly): never fall through to serving the fixture task.
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 404, "message": "task not found"})
		}
	}))
	t.Cleanup(srv.Close)

	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
	return server, func() []string {
		mu.Lock()
		defer mu.Unlock()
		out := make([]string, len(paths))
		copy(out, paths)
		return out
	}
}

// TestHandleGetTask_ShortIDMatchesUUID_AllIncludes is the core acceptance:
// with every include_* flag set, a short-ID get_task and a full-UUID get_task
// must return identical data. Red on the old code: the short-ID call dies at
// include_dependencies with the live error ("failed to list dependencies:
// Bad Request: invalid task_id") while the UUID call succeeds.
func TestHandleGetTask_ShortIDMatchesUUID_AllIncludes(t *testing.T) {
	fx := newShortIDFixture()
	server, _ := getTaskShortIDHarness(t, fx, nil)

	allIncludes := map[string]any{"include_comments": true, "include_artifacts": true, "include_dependencies": true, "include_vcs_links": true}
	uuidArgs := map[string]any{"task_id": fx.fullID}
	shortArgs := map[string]any{"task_id": fx.shortID}
	for k, v := range allIncludes {
		uuidArgs[k] = v
		shortArgs[k] = v
	}
	_, byUUID := callGetTask(t, server, uuidArgs)
	_, byShort := callGetTask(t, server, shortArgs)

	if !reflect.DeepEqual(byUUID, byShort) {
		t.Fatalf("short-ID response differs from UUID response:\nUUID:  %#v\nshort: %#v", byUUID, byShort)
	}

	// The comparison above only means something if the response actually
	// carries each include_* payload — assert presence, not just equality of
	// two equally-empty maps (§ no-green-without-a-positive-control).
	for _, key := range []string{"task", "comments", "artifacts", "dependencies", "dependencies_incoming", "vcs_links"} {
		if _, present := byShort[key]; !present {
			t.Errorf("response is missing %q: %#v", key, byShort)
		}
	}
	if deps, ok := byShort["dependencies"].([]any); !ok || len(deps) != 1 {
		t.Errorf("dependencies = %#v, want the 1 outgoing edge from the fixture", byShort["dependencies"])
	}
	if c, ok := byShort["comments"].([]any); !ok || len(c) != 2 {
		t.Errorf("comments = %#v, want the 2 fixture comments", byShort["comments"])
	}
}

// TestHandleGetTask_ShortIDChildReadsUseResolvedUUID ratchets the fix on the
// wire: after GetTask resolves the prefix, EVERY child read (comments,
// artifacts, dependencies, vcs-links) must hit the resolved full-UUID path,
// never the raw prefix. Old code passes comments/artifacts/vcs-links to the
// backend under the short form and only works where the backend happens to
// tolerate it.
func TestHandleGetTask_ShortIDChildReadsUseResolvedUUID(t *testing.T) {
	fx := newShortIDFixture()
	server, recordedPaths := getTaskShortIDHarness(t, fx, nil)

	_, decoded := callGetTask(t, server, map[string]any{
		"task_id":              fx.shortID,
		"include_comments":     true,
		"include_artifacts":    true,
		"include_dependencies": true,
		"include_vcs_links":    true,
	})
	if task, ok := decoded["task"].(map[string]any); !ok || task["id"] != fx.fullID {
		t.Fatalf("task.id = %#v, want the resolved full UUID %s", decoded["task"], fx.fullID)
	}

	for _, p := range recordedPaths() {
		if strings.Contains(p, "/"+fx.shortID+"/") {
			t.Errorf("child read used the raw short id instead of the resolved UUID: %s", p)
		}
		if strings.Contains(p, fx.fullID+"/") || strings.HasSuffix(p, fx.fullID) || strings.Contains(p, "by-short-id") {
			continue
		}
		t.Errorf("unexpected request path: %s", p)
	}
}

// TestHandleGetTask_ShortIDUnknownPrefixIsToolError controls the failure
// paths of resolution itself: a valid hex prefix that matches nothing (404)
// and one that matches several tasks (ambiguous, 400) must each come back as
// a normal tool error — and no child read may fire, no task object leak.
// Both controls are VALID 6-hex prefixes per the tool contract, so they
// exercise the resolver's decision, not input validation rejecting a
// malformed id before resolution ever runs.
func TestHandleGetTask_ShortIDUnknownPrefixIsToolError(t *testing.T) {
	fx := newShortIDFixture()
	server, recordedPaths := getTaskShortIDHarness(t, fx, func(short string) (map[string]any, int) {
		if short == "abcdef" {
			return map[string]any{"code": 400, "message": "ambiguous task id: 2 matches"}, http.StatusBadRequest
		}
		return map[string]any{"code": 404, "message": "task not found"}, http.StatusNotFound
	})

	// Each prefix must surface its OWN resolver verdict, not one generic
	// error: deadbe resolves nothing (404 not found), abcdef matches two
	// tasks (400 ambiguous). Asserting the distinction is the control that
	// the two cases really exercise two different resolver decisions —
	// with both landing in the same 404 the "ambiguous" control would prove
	// nothing (codex-review run 3, P2).
	wantSubstring := map[string]string{
		"deadbe": "not found",
		"abcdef": "ambiguous",
	}
	for _, short := range []string{"deadbe", "abcdef"} {
		req := mcpsdk.CallToolRequest{}
		req.Params.Arguments = map[string]any{
			"task_id":              short,
			"include_dependencies": true,
		}
		result, err := server.handleGetTask(context.Background(), req)
		if err != nil {
			t.Fatalf("handleGetTask(%q) returned a Go error instead of a tool error: %v", short, err)
		}
		if !result.IsError {
			t.Fatalf("get_task(%q) succeeded (%#v), want a tool error", short, result.Content)
		}
		text := resultText(t, result)
		if want := wantSubstring[short]; !strings.Contains(text, want) {
			t.Errorf("get_task(%q) error = %q, want the resolver's own verdict containing %q", short, text, want)
		}
	}

	for _, p := range recordedPaths() {
		if strings.Contains(p, "/dependencies") || strings.Contains(p, "/comments") {
			t.Errorf("child read fired for an unresolvable prefix: %s", p)
		}
	}
}

// TestHandleGetTask_ShortIDSinceDeltaStubCarriesResolvedID pins the since
// delta against the short form: the trimmed stub must identify the task by
// the resolved full UUID (so a later get_task(since=..., include_*) call and
// the stub agree on id), not echo back the caller's prefix.
func TestHandleGetTask_ShortIDSinceDeltaStubCarriesResolvedID(t *testing.T) {
	fx := newShortIDFixture()
	server, _ := getTaskShortIDHarness(t, fx, nil)

	_, decoded := callGetTask(t, server, map[string]any{
		"task_id": fx.shortID,
		"since":   "2026-10-02T12:30:00Z", // after the fixture's updated_at → unchanged
	})

	if changed, _ := decoded["task_changed"].(bool); changed {
		t.Fatalf("task_changed = %v, want false (since is after updated_at)", decoded["task_changed"])
	}
	stub, ok := decoded["task"].(map[string]any)
	if !ok {
		t.Fatalf("task = %#v, want the delta stub object", decoded["task"])
	}
	if stub["id"] != fx.fullID {
		t.Errorf("delta stub id = %v, want the resolved full UUID %s", stub["id"], fx.fullID)
	}
}
