package mcp

import (
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// The third way recall over-served (task #4c65d3e2): the multi-session profile
// widened the page to 20 even when the caller had explicitly asked for fewer.
//
// It compounded with the graph-boost overflow. recall(limit=6) on a query
// containing "pattern"/"trend"/"history" became limit=20 here, and the old merge
// then appended up to 20 neighbours on top — which is how a request for 6 rows
// came back with 40, the worst of the four measurements on this task.
//
// A preset is a default. It fills in what the caller left unsaid; it does not
// overrule what the caller said.

// requestWith builds a CallToolRequest carrying the given arguments.
func requestWith(args map[string]any) mcpsdk.CallToolRequest {
	var r mcpsdk.CallToolRequest
	r.Params.Arguments = args
	return r
}

func TestHasArgument(t *testing.T) {
	req := requestWith(map[string]any{"limit": 6, "query": "x", "nil_value": nil})

	if !hasArgument(req, "limit") {
		t.Error("hasArgument(limit) = false for an explicitly supplied limit")
	}
	if hasArgument(req, "offset") {
		t.Error("hasArgument(offset) = true for an absent argument")
	}
	if hasArgument(req, "nil_value") {
		t.Error("hasArgument = true for an explicit null — nothing was supplied")
	}
	if hasArgument(requestWith(nil), "limit") {
		t.Error("hasArgument = true on a request with no arguments at all")
	}
}

// TestProfileLimit_DoesNotOverrideExplicitLimit encodes the precedence rule at the
// level the handler applies it. Pre-fix, the explicit case resolved to 20.
func TestProfileLimit_DoesNotOverrideExplicitLimit(t *testing.T) {
	// "pattern" is a multi-session keyword, so the classifier sets Limit=20.
	const query = "what pattern do we see in deploys"
	if got := ClassifyQuery(query); got != ProfileMultiSession {
		t.Fatalf("precondition: ClassifyQuery(%q) = %q, want multi-session", query, got)
	}
	pp := GetProfileParams(ProfileMultiSession)
	if pp.Limit != 20 {
		t.Fatalf("precondition: multi-session Limit = %d, want 20", pp.Limit)
	}

	// resolveLimit mirrors the handler: parse, then let the profile fill in only
	// what the caller omitted.
	resolveLimit := func(req mcpsdk.CallToolRequest) int {
		limit := mcpsdk.ParseInt(req, "limit", 10)
		if pp.Limit > 0 && !hasArgument(req, "limit") {
			limit = pp.Limit
		}
		return limit
	}

	t.Run("explicit limit wins over the profile", func(t *testing.T) {
		for _, want := range []int{1, 6, 10, 40} {
			req := requestWith(map[string]any{"query": query, "limit": want})
			if got := resolveLimit(req); got != want {
				t.Errorf("recall(limit=%d) resolved to %d — the profile overrode an explicit limit", want, got)
			}
		}
	})

	t.Run("profile still widens when no limit was given", func(t *testing.T) {
		req := requestWith(map[string]any{"query": query})
		if got := resolveLimit(req); got != pp.Limit {
			t.Errorf("resolved to %d with no explicit limit, want the profile's %d", got, pp.Limit)
		}
	})
}
