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

// Independent verification of #5d3dc714 found this gap by mutation: forwarding nil
// instead of args.Predicate from handleSetHumanGate left the ENTIRE suite green,
// because nothing called handleSetHumanGate at all — every existing test exercises
// parseSetHumanGateArgs in isolation.
//
// That regression would not be subtle in production: the server requires a predicate on
// API-sourced arms, so every single set_human_gate call would 422, and no test would
// have caught it first. The seam between "parsed correctly" and "sent correctly" is
// exactly where this card's own defect class lives.
//
// So this test drives the real handler against a real HTTP server and asserts on the
// BODY that goes out on the wire.
func TestHandleSetHumanGate_SendsPredicateOnTheWire(t *testing.T) {
	var gotBody map[string]any
	var gotPath string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"t","human_gate":true}`))
	}))
	defer srv.Close()

	s := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
	}

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = predicateArgs(map[string]any{
		"task_id":             "11111111-1111-1111-1111-111111111111",
		"reason":              "мёржим сейчас или ждём?",
		"recommended_default": "жду ответа до дедлайна",
	})

	res, err := s.handleSetHumanGate(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if text := resultText(t, res); strings.Contains(text, "failed") {
		t.Fatalf("handler reported failure: %s", text)
	}

	if !strings.Contains(gotPath, "/human-gate") {
		t.Errorf("wrong endpoint: %s", gotPath)
	}

	// The whole point: the predicate must actually be IN the request body.
	p, ok := gotBody["predicate"].(map[string]any)
	if !ok {
		t.Fatalf("predicate missing from the request body — the server would 422 every "+
			"set_human_gate call; body was: %+v", gotBody)
	}
	for _, k := range []string{
		"credential_exists", "credential_reason",
		"reversible", "reversible_reason",
		"blocked_by_other_task", "blocked_reason",
		"customer_visible_now", "customer_reason",
	} {
		if _, present := p[k]; !present {
			t.Errorf("predicate.%s missing on the wire: %+v", k, p)
		}
	}
	// And the values must survive, not just the keys.
	if p["customer_reason"] != "gateway inactive, nobody can be charged" {
		t.Errorf("customer_reason mangled in transit: %v", p["customer_reason"])
	}
}
