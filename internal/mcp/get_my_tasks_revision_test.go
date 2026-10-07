package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

func revisionFixture() map[string]any {
	return map[string]any{"tasks": []any{
		map[string]any{"id": "a", "version": 1, "description": strings.Repeat("Ю🙂", 300) + "\ncomplete tail", "has_description": true, "title": "A", "status_id": "todo", "assignee_id": "owner", "labels": []any{"tag"}, "comments": []any{map[string]any{"body": "First line\nordinary comment tail"}}},
		map[string]any{"id": "b", "version": 1, "description": "B"},
	}, "count": 2, "total_count": 2, "has_more": false, "extra_envelope": map[string]any{"cursor": "z"}}
}

func snapshotJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func withoutRevision(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	var copy map[string]any
	if err := json.Unmarshal(snapshotJSON(t, value), &copy); err != nil {
		t.Fatal(err)
	}
	delete(copy, "revision")
	return copy
}
func assertCold(t *testing.T, got map[string]any) {
	t.Helper()
	if _, ok := got["tasks"].([]any); !ok || got["unchanged"] == true || got["delta"] == true {
		t.Fatalf("expected complete cold: %v", got)
	}
}
func revisionOf(t *testing.T, got map[string]any) string {
	t.Helper()
	revision, ok := got["revision"].(string)
	if !ok || len(revision) != 64 {
		t.Fatal("complete versioned cold must provide opaque digest")
	}
	return revision
}

func TestMyTasksRevisionRepeat(t *testing.T) {
	fixture := revisionFixture()
	s := myTasksHarness(t, fixture)
	cold := callGetMyTasks(t, s, map[string]any{})
	revision := revisionOf(t, cold)
	item := cold["tasks"].([]any)[0].(map[string]any)
	if item["description"] != fixture["tasks"].([]any)[0].(map[string]any)["description"] {
		t.Fatal("cold cut full Unicode text")
	}
	if !bytes.Equal(snapshotJSON(t, item["comments"]), snapshotJSON(t, fixture["tasks"].([]any)[0].(map[string]any)["comments"])) {
		t.Fatal("ordinary comments cut")
	}
	hit := callGetMyTasks(t, s, map[string]any{"known_revision": revision})
	if hit["unchanged"] != true || len(hit) != 2 || hit["revision"] != revision {
		t.Fatalf("expected exact unchanged: %v", hit)
	}
	// Consumer retains the complete snapshot; an unchanged result replaces no text.
	if !bytes.Equal(snapshotJSON(t, withoutRevision(t, cold)), snapshotJSON(t, withoutRevision(t, callGetMyTasks(t, s, map[string]any{})))) {
		t.Fatal("unchanged did not preserve canonical snapshot")
	}
}

func TestMyTasksRevisionEveryMutation(t *testing.T) {
	changes := map[string]any{"title": "new", "status_id": "done", "priority": "urgent", "assignee_id": "other", "assignee_name": "other", "human_gate": true, "labels": []any{"new"}, "description": "new\n完整🙂", "checked_out_by": "other", "checkout_token": "nonsecret-test-carrier", "checkout_generation": 2, "checkout_expires": "2099-01-01T00:00:00Z", "comments": []any{map[string]any{"body": "edited\nordinary body"}}, "artifact_count": 1, "vcs_link_count": 1, "subtask_count": 1}
	for field, value := range changes {
		t.Run(field, func(t *testing.T) {
			fixture := revisionFixture()
			s := myTasksHarness(t, fixture)
			cold := callGetMyTasks(t, s, map[string]any{})
			revision := revisionOf(t, cold)
			item := fixture["tasks"].([]any)[0].(map[string]any)
			item[field] = value
			item["version"] = 2 // Authoritative trigger also covers non-rendered fields/child activity.
			got := callGetMyTasks(t, s, map[string]any{"known_revision": revision})
			assertCold(t, got)
			if revisionOf(t, got) == revision {
				t.Fatalf("%s did not invalidate", field)
			}
		})
	}
	// Ordinary comment changes can leave updated_at and every visible field
	// unchanged. Their authoritative child version must still invalidate.
	fixture := revisionFixture()
	s := myTasksHarness(t, fixture)
	revision := revisionOf(t, callGetMyTasks(t, s, map[string]any{}))
	fixture["tasks"].([]any)[0].(map[string]any)["version"] = 2
	assertCold(t, callGetMyTasks(t, s, map[string]any{"known_revision": revision}))
}

// Independent reference consumer: reconstruct from the public wire format,
// compare canonical JSON with a newly fetched complete cold response.
func applyMyTasksWire(t *testing.T, base, delta map[string]any) map[string]any {
	t.Helper()
	if delta["base_revision"] != base["revision"] || delta["delta"] != true {
		t.Fatal("base/schema mismatch")
	}
	items := map[string]any{}
	for _, item := range base["tasks"].([]any) {
		items[item.(map[string]any)["id"].(string)] = item
	}
	for _, id := range delta["removed_ids"].([]any) {
		delete(items, id.(string))
	}
	for _, item := range delta["changed"].([]any) {
		items[item.(map[string]any)["id"].(string)] = item
	}
	tasks := []any{}
	for _, id := range delta["order"].([]any) {
		item, ok := items[id.(string)]
		if !ok {
			t.Fatal("missing ordered item")
		}
		tasks = append(tasks, item)
	}
	rebuilt := delta["envelope"].(map[string]any)
	rebuilt["tasks"] = tasks
	rebuilt["revision"] = delta["revision"]
	return rebuilt
}

func TestMyTasksDeltaExactRoundtrip(t *testing.T) {
	fixture := revisionFixture()
	s := myTasksHarness(t, fixture)
	base := callGetMyTasks(t, s, map[string]any{})
	a := fixture["tasks"].([]any)[0].(map[string]any)
	a["description"] = "Changed\n" + strings.Repeat("界🙂", 250)
	a["version"] = 2
	fixture["tasks"] = []any{map[string]any{"id": "c", "version": 1, "description": "New\nfull body"}, a}
	fixture["extra_envelope"] = map[string]any{"cursor": "next", "complete": true}
	delta := callGetMyTasks(t, s, map[string]any{"known_revision": revisionOf(t, base), "accept_delta": true})
	if len(delta["changed"].([]any)) != 2 || len(delta["removed_ids"].([]any)) != 1 {
		t.Fatal("new/changed/deleted wire incomplete")
	}
	want := callGetMyTasks(t, s, map[string]any{})
	rebuilt := applyMyTasksWire(t, base, delta)
	if !bytes.Equal(snapshotJSON(t, rebuilt), snapshotJSON(t, want)) {
		t.Fatal("delta did not reconstruct exact canonical cold")
	}
	// Reorder without replacing unchanged items; empty changed/removed arrays,
	// complete envelope and order still reconstruct exact bytes.
	fixture["tasks"] = []any{a, fixture["tasks"].([]any)[0]}
	next := callGetMyTasks(t, s, map[string]any{"known_revision": revisionOf(t, want), "accept_delta": true})
	if len(next["changed"].([]any)) != 0 {
		t.Fatal("reorder needlessly retransmitted text")
	}
	if !bytes.Equal(snapshotJSON(t, applyMyTasksWire(t, want, next)), snapshotJSON(t, callGetMyTasks(t, s, map[string]any{}))) {
		t.Fatal("order roundtrip failed")
	}
}

type snapshotTransport struct {
	mcpserver.ClientSession
	id string
}

func (s snapshotTransport) SessionID() string { return s.id }

func callMyTasksContext(t *testing.T, s *Server, ctx context.Context, args map[string]any) map[string]any {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	result, err := s.handleGetMyTasks(ctx, req)
	if err != nil || result.IsError {
		t.Fatalf("call failed: %v %v", result, err)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(result.Content[0].(mcpsdk.TextContent).Text), &got); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestMyTasksRevisionScopeFallback(t *testing.T) {
	fixture := revisionFixture()
	s := myTasksHarness(t, fixture)
	cold := callGetMyTasks(t, s, map[string]any{})
	revision := revisionOf(t, cold)
	for _, args := range []map[string]any{{"project_id": "other"}, {"status_category": "done"}, {"limit": 1}, {"known_revision": "obsolete-schema"}} {
		if args["known_revision"] == nil {
			args["known_revision"] = revision
		}
		args["accept_delta"] = true
		assertCold(t, callGetMyTasks(t, s, args))
	}
	// Explicit and implicit default have identical effective scope.
	hit := callGetMyTasks(t, s, map[string]any{"limit": 50, "known_revision": revision})
	if hit["unchanged"] != true {
		t.Fatal("effective default scope unstable")
	}
	for _, identity := range []*AgentSession{{AgentID: uuid.New(), WorkspaceID: s.session.WorkspaceID}, {AgentID: s.session.AgentID, WorkspaceID: uuid.New()}} {
		assertCold(t, callMyTasksContext(t, s, ContextWithSession(context.Background(), identity), map[string]any{"known_revision": revision, "accept_delta": true}))
	}
	transport := mcpserver.NewMCPServer("test", "1")
	ctx1 := transport.WithContext(context.Background(), snapshotTransport{id: "one"})
	ctx2 := transport.WithContext(context.Background(), snapshotTransport{id: "two"})
	r1 := revisionOf(t, callMyTasksContext(t, s, ctx1, map[string]any{}))
	assertCold(t, callMyTasksContext(t, s, ctx2, map[string]any{"known_revision": r1, "accept_delta": true}))
	if callMyTasksContext(t, s, ctx1, map[string]any{"known_revision": r1})["unchanged"] != true {
		t.Fatal("same transport lost base")
	}
	// Restart or consumer compaction: no revision, or lost server base → cold.
	assertCold(t, callGetMyTasks(t, myTasksHarness(t, fixture), map[string]any{"known_revision": revision, "accept_delta": true}))
	assertCold(t, callGetMyTasks(t, s, map[string]any{}))
	full := callGetMyTasks(t, s, map[string]any{"known_revision": revision, "accept_delta": true, "full": true})
	if !bytes.Equal(snapshotJSON(t, full), snapshotJSON(t, fixture)) {
		t.Fatal("full bypass changed original REST shape")
	}
}

func TestMyTasksCacheLossExpiryCorruption(t *testing.T) {
	for _, condition := range []string{"loss", "expiry", "corruption"} {
		t.Run(condition, func(t *testing.T) {
			s := myTasksHarness(t, revisionFixture())
			revision := revisionOf(t, callGetMyTasks(t, s, map[string]any{}))
			c := &s.myTasksSnapshots
			c.mu.Lock()
			b := c.bases[revision]
			switch condition {
			case "loss":
				delete(c.bases, revision)
				c.bytes = 0
			case "expiry":
				b.expires = time.Now().Add(-time.Second)
				c.bases[revision] = b
			case "corruption":
				c.bytes += 1 - len(b.body)
				b.body = []byte("{")
				c.bases[revision] = b
			}
			c.mu.Unlock()
			assertCold(t, callGetMyTasks(t, s, map[string]any{"known_revision": revision, "accept_delta": true}))
			c.mu.Lock()
			actualBytes := 0
			for _, base := range c.bases {
				actualBytes += len(base.body)
			}
			if actualBytes != c.bytes {
				t.Errorf("cache byte accounting: %d, actual %d", c.bytes, actualBytes)
			}
			c.mu.Unlock()
		})
	}
}

func TestMyTasksCompletenessAndVersionGate(t *testing.T) {
	for _, kind := range []string{"page", "item", "blank", "duplicate"} {
		t.Run(kind, func(t *testing.T) {
			fixture := revisionFixture()
			item := fixture["tasks"].([]any)[0].(map[string]any)
			switch kind {
			case "page":
				fixture["truncated"] = true
			case "item":
				item["description_truncated"] = true
			case "blank":
				item["description"] = ""
			case "duplicate":
				fixture["tasks"].([]any)[1].(map[string]any)["id"] = "a"
			}
			s := myTasksHarness(t, fixture)
			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = map[string]any{}
			got, err := s.handleGetMyTasks(context.Background(), req)
			if err != nil || !got.IsError {
				t.Fatal("incomplete upstream accepted as complete base")
			}
		})
	}
	fixture := revisionFixture()
	delete(fixture["tasks"].([]any)[0].(map[string]any), "version")
	s := myTasksHarness(t, fixture)
	out := callGetMyTasks(t, s, map[string]any{"known_revision": "anything", "accept_delta": true})
	assertCold(t, out)
	if out["revision"] != nil {
		t.Fatal("unversioned child activity advertised as conditionally safe")
	}
}

func TestMyTasksAlwaysReadsAuthenticatedAPI(t *testing.T) {
	reads := 0
	reject := false
	fixture := revisionFixture()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reads++
		if reject {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Query().Get("page_size") != "50" {
			t.Error("effective limit not forwarded")
		}
		_ = json.NewEncoder(w).Encode(fixture)
	}))
	defer upstream.Close()
	s := &Server{restClient: NewRESTClient(upstream.URL, "test-key"), session: &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()}}
	revision := revisionOf(t, callGetMyTasks(t, s, map[string]any{}))
	if callGetMyTasks(t, s, map[string]any{"known_revision": revision})["unchanged"] != true || reads != 2 {
		t.Fatal("repeat did not re-read current authenticated API")
	}
	reject = true
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"known_revision": revision}
	result, err := s.handleGetMyTasks(context.Background(), req)
	if err != nil || !result.IsError || reads != 3 {
		t.Fatal("stale cache bypassed failed authentication")
	}
}

func TestMyTasksCacheBounds(t *testing.T) {
	cache := &myTasksSnapshots{}
	now := time.Now()
	for i := 0; i < myTasksCacheEntries+1; i++ {
		cache.exchange("scope", uuid.NewString(), "", []byte("{}"), now.Add(time.Duration(i)*time.Second))
	}
	if len(cache.bases) != myTasksCacheEntries || cache.bytes != myTasksCacheEntries*2 {
		t.Fatal("entry bound failed")
	}
	cache.exchange("scope", "oversize", "", make([]byte, myTasksCacheBytes+1), now)
	if len(cache.bases) != myTasksCacheEntries {
		t.Fatal("oversize base cached")
	}
	cache.exchange("scope", "after-ttl", "", []byte("{}"), now.Add(2*myTasksCacheTTL))
	if len(cache.bases) != 1 || cache.bytes != 2 {
		t.Fatal("TTL bound failed")
	}
}

func TestMyTasksStatelessSessionScope(t *testing.T) {
	s := myTasksHarness(t, revisionFixture())
	transport := mcpserver.NewMCPServer("test", "1")
	ctx := transport.WithContext(context.Background(), snapshotTransport{id: ""})
	cold := callMyTasksContext(t, s, ctx, map[string]any{})
	assertCold(t, cold)
	if cold["revision"] != nil {
		t.Fatal("stateless request without caller session advertised reusable revision")
	}
	first := callMyTasksContext(t, s, ctx, map[string]any{"snapshot_session": "one"})
	rev := revisionOf(t, first)
	if callMyTasksContext(t, s, ctx, map[string]any{"snapshot_session": "one", "known_revision": rev})["unchanged"] != true {
		t.Fatal("same caller session lost base")
	}
	for _, namespace := range []string{"two", ""} {
		assertCold(t, callMyTasksContext(t, s, ctx, map[string]any{"snapshot_session": namespace, "known_revision": rev, "accept_delta": true}))
	}
}

func TestMyTasksScopeMemoryBound(t *testing.T) {
	s := myTasksHarness(t, revisionFixture())
	scope := s.myTasksSnapshots.scope(context.Background(), s.session, s.getRESTClient(context.Background()), map[string]string{"project_id": strings.Repeat("x", 1<<20)}, strings.Repeat("y", 1<<20))
	if len(scope) != 64 {
		t.Fatalf("retained scope has %d bytes; expected fixed digest", len(scope))
	}
}
