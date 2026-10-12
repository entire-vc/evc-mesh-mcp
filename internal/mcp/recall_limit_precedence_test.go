package mcp

import (
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// The third way recall over-served: the multi-session profile
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
