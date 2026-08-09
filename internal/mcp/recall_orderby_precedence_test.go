package mcp

import (
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// The same precedence bug as the limit one (`recall_limit_precedence_test.go`,
// task #4c65d3e2), in the parameter next to it: `order_by`.
//
// `ProfileFactual` presets "relevance:desc", and the handler applied it
// unconditionally — so a caller who explicitly asked for
// "decayed_relevance:desc" had it rewritten whenever the query happened to be
// short and to contain a UUID, a path, or an env-var name. That shape is what
// most lookups look like ("~/bin/mesh-mcp path", "MESH_API_KEY value",
// "4e2c1857-a501 status").
//
// It cost nothing until 2026-08-09, because no order_by armed anything on the
// recall path. evc-mesh#540 made "decayed_relevance:desc" arm time decay by
// itself, and from that moment the rewrite silently removes the decay the
// caller requested.
//
// A preset is a default. It fills in what the caller left unsaid; it does not
// overrule what the caller said.
func TestProfileOrderBy_DoesNotOverrideExplicitOrderBy(t *testing.T) {
	// A short query leading with a path → factual, which presets OrderBy.
	// (`queryContainsPath` matches a PREFIX, so the path has to lead.)
	const query = "~/bin/mesh-mcp contents"
	if got := ClassifyQuery(query); got != ProfileFactual {
		t.Fatalf("precondition: ClassifyQuery(%q) = %q, want factual", query, got)
	}
	pp := GetProfileParams(ProfileFactual)
	if pp.OrderBy != "relevance:desc" {
		t.Fatalf("precondition: factual OrderBy = %q, want relevance:desc", pp.OrderBy)
	}

	// Not a mirror of the handler — the same function the handler calls. A
	// re-implementation here would keep passing after someone edited tools.go.
	resolveOrderBy := func(req mcpsdk.CallToolRequest) string {
		return resolveProfileOrderBy(pp.OrderBy, mcpsdk.ParseString(req, "order_by", ""))
	}

	t.Run("explicit order_by wins over the profile", func(t *testing.T) {
		// "decayed_relevance:desc" is the case with teeth — since evc-mesh#540
		// losing it means losing time decay. The others guard the general rule.
		for _, want := range []string{"decayed_relevance:desc", "created_at:desc", "created_at:asc"} {
			req := requestWith(map[string]any{"query": query, "order_by": want})
			if got := resolveOrderBy(req); got != want {
				t.Errorf("recall(order_by=%q) resolved to %q — the profile overrode an explicit order_by", want, got)
			}
		}
	})

	t.Run("profile still fills in when no order_by was given", func(t *testing.T) {
		req := requestWith(map[string]any{"query": query})
		if got := resolveOrderBy(req); got != pp.OrderBy {
			t.Errorf("resolved to %q with no explicit order_by, want the profile's %q", got, pp.OrderBy)
		}
	})
}
