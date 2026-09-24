package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// ── containsSecret ──────────────────────────────────────────────────────────

func TestContainsSecret(t *testing.T) {
	cases := []struct {
		text string
		want bool
	}{
		{"We should use Redis for caching all session state", false},
		{"Deploy to prod on Friday after 17:00 MSK", false},
		{"Disable legacy OAuth2 flow for all new workspaces", false},
		{"api_key=sk-abc123def456...", true},
		{"set api_key to sk-abc123def456", true},
		{"Authorization: Bearer eyJhbGciOiJIUzI1NiJ9", true},
		{"password=hunter2 please rotate this", true},
		{"private_key: -----BEGIN RSA PRIVATE KEY-----", true},
		// 40+ char continuous hex → treated as secret (API key, SHA-1 hash, etc.)
		{"token: a3f8c2d1e4b5a6c7d8e9f0a1b2c3d4e5f6a7b8c9", true},
		// UUIDs (dashed) should NOT trigger
		{"task id: 550e8400-e29b-41d4-a716-446655440000", false},
		// Agent key prefix
		{"agk_ws-006901c7_3f0e34ad7c40e98a6243dd9b4aa670004a55bc8e4257a9ad", true},
	}
	for _, tc := range cases {
		got := containsSecret(tc.text)
		if got != tc.want {
			t.Errorf("containsSecret(%q) = %v, want %v", tc.text, got, tc.want)
		}
	}
}

// ── slugify ─────────────────────────────────────────────────────────────────

func TestSlugify(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"Use Redis for caching", "use-redis-for-caching"},
		{"Disable OLD OAuth2  flow", "disable-old-oauth2-flow"},
		{"  leading and trailing  ", "leading-and-trailing"},
		// Max 50 chars
		{strings.Repeat("a", 60), strings.Repeat("a", 50)},
		{"Mixed CASE & special $chars!", "mixed-case-special-chars"},
	}
	for _, tc := range cases {
		got := slugify(tc.in)
		if got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// ── handleRecordOwnerDecision ─────────────────────────────────────────────────────

func buildOwnerDecisionRequest(text, summary string, propagateTo []string, scope, privacy string) mcpsdk.CallToolRequest {
	args := map[string]any{
		"text":    text,
		"summary": summary,
	}
	if len(propagateTo) > 0 {
		any := make([]any, len(propagateTo))
		for i, s := range propagateTo {
			any[i] = s
		}
		args["propagate_to"] = any
	}
	if scope != "" {
		args["scope"] = scope
	}
	if privacy != "" {
		args["privacy"] = privacy
	}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

func TestHandleRecordOwnerDecision_PublicDecision(t *testing.T) {
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/memories" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		memID := uuid.New().String()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"memory": map[string]any{
				"id":         memID,
				"created_at": time.Now().UTC().Format(time.RFC3339),
			},
			"outcome": "created",
		})
	}))
	defer srv.Close()

	rc := NewRESTClient(srv.URL, "test-key")
	wsID := uuid.New()
	server := &Server{
		restClient: rc,
		tracker:    NewSessionTracker(),
	}
	ctx := withTestSession(context.Background(), server, wsID)

	req := buildOwnerDecisionRequest(
		"We will use Redis for all session state going forward.",
		"Use Redis for sessions",
		[]string{"linus", "bill"},
		"",
		"public",
	)
	result, err := server.handleRecordOwnerDecision(ctx, req)
	if err != nil {
		t.Fatalf("handleRecordOwnerDecision returned error: %v", err)
	}

	out := decodeToolResultJSON(t, result)

	if out["privacy"] != "public" {
		t.Errorf("expected privacy=public, got %v", out["privacy"])
	}
	if out["id"] == "" || out["id"] == nil {
		t.Error("expected non-empty id in response")
	}

	// Verify the key format: canonical-decision-{date}-{slug}
	key, _ := out["key"].(string)
	day := time.Now().UTC().Format("2006-01-02")
	expectedKey := "canonical-decision-" + day + "-use-redis-for-sessions"
	if key != expectedKey {
		t.Errorf("key = %q, want %q", key, expectedKey)
	}

	// Verify tags sent to API include required canonical tags.
	if capturedBody == nil {
		t.Fatal("REST /memories endpoint was not called")
	}
	tags, _ := capturedBody["tags"].([]any)
	tagSet := make(map[string]bool, len(tags))
	for _, tg := range tags {
		if s, ok := tg.(string); ok {
			tagSet[s] = true
		}
	}
	for _, required := range []string{"kind:canonical-decision", "source:owner-decision", "privacy:public", "propagate_to:linus", "propagate_to:bill"} {
		if !tagSet[required] {
			t.Errorf("missing required tag %q in %v", required, tags)
		}
	}
}

func TestHandleRecordOwnerDecision_SecretBackstop(t *testing.T) {
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&capturedBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"memory":  map[string]any{"id": uuid.New().String(), "created_at": time.Now().UTC().Format(time.RFC3339)},
			"outcome": "created",
		})
	}))
	defer srv.Close()

	rc := NewRESTClient(srv.URL, "test-key")
	wsID := uuid.New()
	server := &Server{
		restClient: rc,
		tracker:    NewSessionTracker(),
	}
	ctx := withTestSession(context.Background(), server, wsID)

	// Text contains a secret — should auto-elevate to private.
	req := buildOwnerDecisionRequest(
		"New deploy token: a3f8c2d1e4b5a6c7d8e9f0a1b2c3d4e5f6a7b8c9d0",
		"Deploy token rotation",
		[]string{"all"},
		"",
		"public", // caller says public but text has a secret
	)
	result, err := server.handleRecordOwnerDecision(ctx, req)
	if err != nil {
		t.Fatalf("handleRecordOwnerDecision returned error: %v", err)
	}

	out := decodeToolResultJSON(t, result)

	// Privacy must be auto-elevated to private regardless of caller request.
	if out["privacy"] != "private" {
		t.Errorf("expected privacy=private (backstop triggered), got %v", out["privacy"])
	}

	// Verify privacy:private tag was set in the API call.
	tags, _ := capturedBody["tags"].([]any)
	found := false
	for _, tg := range tags {
		if tg == "privacy:private" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected privacy:private tag in request, got tags: %v", tags)
	}
}

func TestHandleRecordOwnerDecision_DeduplicatesViaSameKey(t *testing.T) {
	calls := 0
	var lastKey string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		lastKey, _ = body["key"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"memory":  map[string]any{"id": uuid.New().String(), "created_at": time.Now().UTC().Format(time.RFC3339)},
			"outcome": "updated", // second call returns "updated" (server-side upsert)
		})
	}))
	defer srv.Close()

	rc := NewRESTClient(srv.URL, "test-key")
	wsID := uuid.New()
	server := &Server{
		restClient: rc,
		tracker:    NewSessionTracker(),
	}
	ctx := withTestSession(context.Background(), server, wsID)

	req := buildOwnerDecisionRequest("Same decision text.", "Same summary", []string{"all"}, "", "public")

	// Call twice — both produce same key because summary+day are identical.
	for i := 0; i < 2; i++ {
		_, err := server.handleRecordOwnerDecision(ctx, req)
		if err != nil {
			t.Fatalf("call %d: handleRecordOwnerDecision error: %v", i+1, err)
		}
	}

	if calls != 2 {
		t.Errorf("expected 2 REST calls, got %d", calls)
	}

	// Both calls must produce the same key (server-side upsert handles dedup).
	day := time.Now().UTC().Format("2006-01-02")
	want := fmt.Sprintf("canonical-decision-%s-same-summary", day)
	if lastKey != want {
		t.Errorf("key = %q, want %q", lastKey, want)
	}
}

// ── handleRecordOwnerDecision + task_id (human_gate linkage) ─────────────────────

// buildOwnerDecisionRequestWithTask is buildOwnerDecisionRequest plus an
// optional task_id argument.
func buildOwnerDecisionRequestWithTask(text, summary string, propagateTo []string, scope, privacy, taskID string) mcpsdk.CallToolRequest {
	req := buildOwnerDecisionRequest(text, summary, propagateTo, scope, privacy)
	if taskID != "" {
		req.Params.Arguments.(map[string]any)["task_id"] = taskID
	}
	return req
}

// ownerTeamDirectoryPayload is a minimal team directory response carrying one
// human with role=="owner", matching what resolveDeciderUserID falls back to.
func ownerTeamDirectoryPayload(ownerID string) map[string]any {
	return map[string]any{
		"humans": []any{
			map[string]any{"id": ownerID, "username": "alice", "role": "owner"},
		},
		"agents": []any{},
	}
}

func TestHandleRecordOwnerDecision_TaskID_LinksAndReleasesGate(t *testing.T) {
	ownerID := uuid.New().String()
	taskID := uuid.New().String()
	var capturedDecisionBody map[string]any
	var capturedDecisionPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/memories":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"memory":  map[string]any{"id": uuid.New().String(), "created_at": time.Now().UTC().Format(time.RFC3339)},
				"outcome": "created",
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/team"):
			_ = json.NewEncoder(w).Encode(ownerTeamDirectoryPayload(ownerID))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/"+taskID+"/human-gate-decisions":
			capturedDecisionPath = r.URL.Path
			_ = json.NewDecoder(r.Body).Decode(&capturedDecisionBody)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":            uuid.New().String(),
				"task_id":       taskID,
				"canonical_key": capturedDecisionBody["canonical_key"],
				"decided_by":    capturedDecisionBody["decided_by"],
				"provenance":    capturedDecisionBody["provenance"],
				"channel":       capturedDecisionBody["channel"],
				"quote":         capturedDecisionBody["quote"],
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	rc := NewRESTClient(srv.URL, "test-key")
	wsID := uuid.New()
	server := &Server{restClient: rc, tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, wsID)

	req := buildOwnerDecisionRequestWithTask(
		"Free tier stays unlimited on the owner's own account.",
		"Free tier owner account unlimited",
		nil, "", "public", taskID,
	)
	result, err := server.handleRecordOwnerDecision(ctx, req)
	if err != nil {
		t.Fatalf("handleRecordOwnerDecision returned error: %v", err)
	}
	out := decodeToolResultJSON(t, result)

	if capturedDecisionPath == "" {
		t.Fatal("POST .../human-gate-decisions was never called")
	}
	if errMsg, ok := out["human_gate_decision_error"]; ok {
		t.Fatalf("unexpected human_gate_decision_error: %v", errMsg)
	}
	decision, ok := out["human_gate_decision"].(map[string]any)
	if !ok {
		t.Fatalf("expected human_gate_decision object in response, got %v", out["human_gate_decision"])
	}

	// AC1: canonical_key on the decision is non-empty and equal to the
	// canonical record's own key — the backlink resolves both ways.
	wantKey, _ := out["key"].(string)
	if wantKey == "" {
		t.Fatal("canonical key missing from response")
	}
	if decision["canonical_key"] != wantKey {
		t.Errorf("decision canonical_key = %v, want %v", decision["canonical_key"], wantKey)
	}

	if capturedDecisionBody["decided_by"] != ownerID {
		t.Errorf("decided_by = %v, want %v", capturedDecisionBody["decided_by"], ownerID)
	}
	if capturedDecisionBody["provenance"] != "attested" {
		t.Errorf("provenance = %v, want attested", capturedDecisionBody["provenance"])
	}
	if capturedDecisionBody["channel"] != "telegram" {
		t.Errorf("channel = %v, want telegram", capturedDecisionBody["channel"])
	}
	if capturedDecisionBody["quote"] != "Free tier stays unlimited on the owner's own account." {
		t.Errorf("quote = %v, want verbatim text", capturedDecisionBody["quote"])
	}
}

// TestHandleRecordOwnerDecision_NoTaskID_Unchanged is the regression control for
// AC2: omitting task_id must not call the human-gate-decisions or team
// endpoints at all, and the response must carry no human_gate_decision* keys.
func TestHandleRecordOwnerDecision_NoTaskID_Unchanged(t *testing.T) {
	var otherCalls []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost && r.URL.Path == "/api/v1/memories" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"memory":  map[string]any{"id": uuid.New().String(), "created_at": time.Now().UTC().Format(time.RFC3339)},
				"outcome": "created",
			})
			return
		}
		otherCalls = append(otherCalls, r.Method+" "+r.URL.Path)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	rc := NewRESTClient(srv.URL, "test-key")
	wsID := uuid.New()
	server := &Server{restClient: rc, tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, wsID)

	req := buildOwnerDecisionRequest("Plain decision, no task.", "Plain decision", nil, "", "public")
	result, err := server.handleRecordOwnerDecision(ctx, req)
	if err != nil {
		t.Fatalf("handleRecordOwnerDecision returned error: %v", err)
	}
	out := decodeToolResultJSON(t, result)

	if len(otherCalls) != 0 {
		t.Errorf("expected no calls beyond /api/v1/memories, got: %v", otherCalls)
	}
	if _, ok := out["human_gate_decision"]; ok {
		t.Errorf("unexpected human_gate_decision in response: %v", out["human_gate_decision"])
	}
	if _, ok := out["human_gate_decision_error"]; ok {
		t.Errorf("unexpected human_gate_decision_error in response: %v", out["human_gate_decision_error"])
	}
}

// TestHandleRecordOwnerDecision_TaskID_UngatedTask_NoErrorSurfaced is AC3: a task_id
// pointing at a task with no live gate still records the decision server-side
// (the server no-ops the release, per contract) and the tool call surfaces no
// error — canon was written either way.
func TestHandleRecordOwnerDecision_TaskID_UngatedTask_NoErrorSurfaced(t *testing.T) {
	ownerID := uuid.New().String()
	taskID := uuid.New().String()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/memories":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"memory":  map[string]any{"id": uuid.New().String(), "created_at": time.Now().UTC().Format(time.RFC3339)},
				"outcome": "created",
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/team"):
			_ = json.NewEncoder(w).Encode(ownerTeamDirectoryPayload(ownerID))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/"+taskID+"/human-gate-decisions":
			// Server records the row and, because the task has no live gate,
			// returns 201 without touching human_gate — no distinct signal in
			// the response body, matching RecordHumanGateDecision's contract.
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":            uuid.New().String(),
				"task_id":       taskID,
				"canonical_key": body["canonical_key"],
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	rc := NewRESTClient(srv.URL, "test-key")
	wsID := uuid.New()
	server := &Server{restClient: rc, tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, wsID)

	req := buildOwnerDecisionRequestWithTask("Ungated task decision.", "Ungated task decision", nil, "", "public", taskID)
	result, err := server.handleRecordOwnerDecision(ctx, req)
	if err != nil {
		t.Fatalf("handleRecordOwnerDecision returned error: %v", err)
	}
	out := decodeToolResultJSON(t, result)

	if errMsg, ok := out["human_gate_decision_error"]; ok {
		t.Fatalf("unexpected human_gate_decision_error on an ungated task: %v", errMsg)
	}
	if _, ok := out["human_gate_decision"].(map[string]any); !ok {
		t.Fatalf("expected human_gate_decision object even on an ungated task, got %v", out["human_gate_decision"])
	}
}

// TestHandleRecordOwnerDecision_TaskID_DeciderUnresolvable_SurfacesError covers the
// failure path: the canonical write still succeeds (its own assertions are
// covered by the other tests), and a team-directory lookup failure is
// reported via human_gate_decision_error rather than failing the whole call.
func TestHandleRecordOwnerDecision_TaskID_DeciderUnresolvable_SurfacesError(t *testing.T) {
	taskID := uuid.New().String()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/memories":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"memory":  map[string]any{"id": uuid.New().String(), "created_at": time.Now().UTC().Format(time.RFC3339)},
				"outcome": "created",
			})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/team"):
			// No preferred username and no "owner" role — unresolvable.
			_ = json.NewEncoder(w).Encode(map[string]any{"humans": []any{}, "agents": []any{}})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	rc := NewRESTClient(srv.URL, "test-key")
	wsID := uuid.New()
	server := &Server{restClient: rc, tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, wsID)

	req := buildOwnerDecisionRequestWithTask("Some decision.", "Some decision", nil, "", "public", taskID)
	result, err := server.handleRecordOwnerDecision(ctx, req)
	if err != nil {
		t.Fatalf("handleRecordOwnerDecision returned error: %v", err)
	}
	out := decodeToolResultJSON(t, result)

	if out["key"] == "" || out["key"] == nil {
		t.Error("canonical write must still have succeeded (key present) despite the linkage failure")
	}
	if _, ok := out["human_gate_decision_error"]; !ok {
		t.Error("expected human_gate_decision_error to be surfaced")
	}
	if _, ok := out["human_gate_decision"]; ok {
		t.Error("human_gate_decision must be absent when linkage failed")
	}
}

// ── handleGetCanonicalUpdates ────────────────────────────────────────────────

func TestHandleGetCanonicalUpdates_ForwardsParams(t *testing.T) {
	var capturedURL string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capturedURL = r.URL.String()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"since":   "2026-06-01T10:00:00Z",
			"now":     "2026-06-01T17:00:00Z",
			"count":   1,
			"updates": []any{},
		})
	}))
	defer srv.Close()

	rc := NewRESTClient(srv.URL, "test-key")
	server := &Server{
		restClient: rc,
		tracker:    NewSessionTracker(),
	}

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"since": "2026-06-01T10:00:00Z",
		"agent": "linus",
		"scope": "",
	}

	result, err := server.handleGetCanonicalUpdates(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetCanonicalUpdates error: %v", err)
	}

	out := decodeToolResultJSON(t, result)
	if out["count"] == nil {
		t.Error("expected count in response")
	}

	// Verify query params were forwarded correctly.
	if !strings.Contains(capturedURL, "since=2026-06-01") {
		t.Errorf("expected since param in URL, got %q", capturedURL)
	}
	if !strings.Contains(capturedURL, "agent=linus") {
		t.Errorf("expected agent=linus in URL, got %q", capturedURL)
	}
}

// withTestSession returns a context carrying a fake AgentSession so getSession(ctx) returns it.
func withTestSession(ctx context.Context, _ *Server, wsID uuid.UUID) context.Context {
	return ContextWithSession(ctx, &AgentSession{WorkspaceID: wsID})
}

// ── decider resolution & published tool names ───────────────────────────────

// TestResolveDeciderUserID_PreferredUsernameWins pins the operator override:
// with MESH_MCP_DECIDER_USERNAME set, that human is decided_by even when a
// different human holds role=owner.
func TestResolveDeciderUserID_PreferredUsernameWins(t *testing.T) {
	preferredID, otherOwnerID := uuid.New().String(), uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"humans": []any{
				map[string]any{"id": otherOwnerID, "username": "carol", "role": "owner"},
				map[string]any{"id": preferredID, "username": "dave", "role": "member"},
			},
			"agents": []any{},
		})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())

	t.Setenv(envDeciderUsername, "dave")
	if got, err := server.resolveDeciderUserID(ctx); err != nil || got != preferredID {
		t.Errorf("with %s=dave: got (%q, %v), want %q", envDeciderUsername, got, err, preferredID)
	}
	// A preferred username that matches nobody falls back to the owner (and logs).
	t.Setenv(envDeciderUsername, "nobody-here")
	if got, err := server.resolveDeciderUserID(ctx); err != nil || got != otherOwnerID {
		t.Errorf("with unmatched %s: got (%q, %v), want owner %q", envDeciderUsername, got, err, otherOwnerID)
	}
	t.Setenv(envDeciderUsername, "")
	if got, err := server.resolveDeciderUserID(ctx); err != nil || got != otherOwnerID {
		t.Errorf("with %s unset: got (%q, %v), want owner %q", envDeciderUsername, got, err, otherOwnerID)
	}
}

// TestPublishedToolNames_LegacyDecisionAliasIsOptIn: catalogs read tools/list
// of the published binary, so the legacy name must be absent by default and
// present only when a deployment opts in.
func TestPublishedToolNames_LegacyDecisionAliasIsOptIn(t *testing.T) {
	for _, profile := range []string{ProfileCore, ProfileFull} {
		t.Setenv(envLegacyToolAliases, "")
		tools := NewServer(ServerConfig{Profile: profile}).MCPServer().ListTools()
		if _, ok := tools[toolRecordOwnerDecision]; !ok {
			t.Errorf("profile %s: %s not registered", profile, toolRecordOwnerDecision)
		}
		if _, ok := tools[toolLegacyDecisionAlias]; ok {
			t.Errorf("profile %s: legacy alias %s registered without opt-in", profile, toolLegacyDecisionAlias)
		}

		t.Setenv(envLegacyToolAliases, "1")
		tools = NewServer(ServerConfig{Profile: profile}).MCPServer().ListTools()
		if _, ok := tools[toolLegacyDecisionAlias]; !ok {
			t.Errorf("profile %s: legacy alias %s missing with %s=1", profile, toolLegacyDecisionAlias, envLegacyToolAliases)
		}
	}
}
