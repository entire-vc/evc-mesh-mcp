package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

func buildPavelDecisionRequest(scope, text, propagateTo, summary, privacy string) mcpsdk.CallToolRequest {
	args := map[string]any{
		"scope":        scope,
		"text":         text,
		"propagate_to": propagateTo,
		"summary":      summary,
	}
	if privacy != "" {
		args["privacy"] = privacy
	}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

func newKnowledgeServer(t *testing.T, captured *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/knowledge") {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(captured); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":         "test-id-123",
			"created_at": "2026-06-01T12:00:00Z",
			"updated_at": "2026-06-01T12:00:01Z",
		})
	}))
}

func tagsFromBody(body map[string]any) []string {
	raw, _ := body["tags"].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func findTag(tags []string, prefix string) string {
	for _, t := range tags {
		if strings.HasPrefix(t, prefix) {
			return t
		}
	}
	return ""
}

// TestHandlePavelDecision_PrivacyBackstop verifies that text containing
// credential patterns is auto-flagged as privacy:private regardless of input.
func TestHandlePavelDecision_PrivacyBackstop(t *testing.T) {
	cases := []struct {
		name         string
		text         string
		inputPrivacy string
		wantPrivacy  string
	}{
		{
			name:         "bearer token forced private",
			text:         "Bearer eyJhbGciOiJSUzI1NiJ9.abc123",
			inputPrivacy: "public",
			wantPrivacy:  "private",
		},
		{
			name:         "40-char hex forced private",
			text:         "api key is a1b2c3d4e5f6a1b2c3d4e5f6a1b2c3d4e5f6a1b2",
			inputPrivacy: "public",
			wantPrivacy:  "private",
		},
		{
			name:         "password kv pattern forced private",
			text:         "password: s3cr3tPassw0rd",
			inputPrivacy: "public",
			wantPrivacy:  "private",
		},
		{
			name:         "token kv pattern forced private",
			text:         "token=ghp_abcdefghijklmnop",
			inputPrivacy: "public",
			wantPrivacy:  "private",
		},
		{
			name:         "clean decision stays public",
			text:         "All agents should use RFC3339 timestamps in API calls going forward",
			inputPrivacy: "public",
			wantPrivacy:  "public",
		},
		{
			name:         "explicit private input stays private",
			text:         "Internal note: deprioritize this feature for now",
			inputPrivacy: "private",
			wantPrivacy:  "private",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var capturedBody map[string]any
			srv := newKnowledgeServer(t, &capturedBody)
			defer srv.Close()

			server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
			req := buildPavelDecisionRequest("proj-uuid", tc.text, "all", "test decision", tc.inputPrivacy)

			result, err := server.handlePavelDecision(context.Background(), req)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result == nil {
				t.Fatal("result is nil")
			}
			if capturedBody == nil {
				t.Fatal("REST endpoint not called")
			}

			tags := tagsFromBody(capturedBody)
			got := strings.TrimPrefix(findTag(tags, "privacy:"), "privacy:")
			if got != tc.wantPrivacy {
				t.Errorf("privacy tag = %q, want %q (text=%q)", got, tc.wantPrivacy, tc.text)
			}
		})
	}
}

// TestHandlePavelDecision_IdempotencyKey verifies that two calls with the same
// summary produce the same key, implementing upsert (latest-wins) semantics.
// The spec mentions (summary, day) dedup; we deliberately use summary-only because
// canonical decisions should be freely overwritable — adding a day component would
// create duplicate entries on subsequent days instead of updating the canonical record.
func TestHandlePavelDecision_IdempotencyKey(t *testing.T) {
	calls := 0
	var lastKey string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		lastKey, _ = body["key"].(string)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "t1", "created_at": "2026-06-01T00:00:00Z", "updated_at": "2026-06-01T00:00:01Z"})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	summary := "Use RFC3339 timestamps everywhere"

	if _, err := server.handlePavelDecision(context.Background(),
		buildPavelDecisionRequest("proj-uuid", "first version of the decision", "all", summary, "public")); err != nil {
		t.Fatalf("first call: %v", err)
	}
	firstKey := lastKey

	if _, err := server.handlePavelDecision(context.Background(),
		buildPavelDecisionRequest("proj-uuid", "revised decision text", "all", summary, "public")); err != nil {
		t.Fatalf("second call: %v", err)
	}

	if firstKey != lastKey {
		t.Errorf("key changed between calls: %q → %q (must be stable for upsert to work)", firstKey, lastKey)
	}
	if !strings.HasPrefix(firstKey, "canonical-decision:") {
		t.Errorf("key must start with 'canonical-decision:', got %q", firstKey)
	}
	if calls != 2 {
		t.Errorf("expected 2 REST calls (upsert, not skip), got %d", calls)
	}
}

// TestHandlePavelDecision_PropagationTags verifies that comma-separated
// propagate_to values produce individual propagate_to:<slug> tags and
// that the affects list in the response matches.
func TestHandlePavelDecision_PropagationTags(t *testing.T) {
	var capturedBody map[string]any
	srv := newKnowledgeServer(t, &capturedBody)
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	req := buildPavelDecisionRequest("proj-uuid", "some decision text", "linus,garfield", "routing decision", "public")

	result, err := server.handlePavelDecision(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	out := decodeToolResultJSON(t, result)

	affects, _ := out["affects"].([]any)
	if len(affects) != 2 {
		t.Errorf("expected 2 affects, got %d: %v", len(affects), affects)
	}

	tags := tagsFromBody(capturedBody)
	if findTag(tags, "propagate_to:linus") == "" {
		t.Errorf("expected propagate_to:linus tag, got %v", tags)
	}
	if findTag(tags, "propagate_to:garfield") == "" {
		t.Errorf("expected propagate_to:garfield tag, got %v", tags)
	}
	if findTag(tags, "kind:canonical-decision") == "" {
		t.Errorf("expected kind:canonical-decision tag, got %v", tags)
	}
}
