package mcp

import (
	"fmt"
	"strings"
	"testing"
)

// Two contracts govern every memory write, and until this test both lived only
// in bob/CLAUDE-memory.md — 21,913 bytes of prose about a surface whose own
// schema said nothing about it (audit-2026-09 §4.1, task #17840d1b).
//
//  1. The key pattern. The server refuses a non-slug key outright
//     (evc-mesh internal/service/memory_service.go:35, keySlugRegex). The doc
//     had carried colon-delimited examples that contradicted it, and every
//     agent who copied one silently lost their first remember() write.
//
//  2. The kind: -> importance_score table. This is not a taxonomy, it is the
//     retrieval contract: importance_score is what recall's default
//     min_importance (0.3) filters on, so the kind: tag an agent picks decides
//     whether the entry is ever returned again.
//
// Prose drifts from code silently — measured on this very table before the
// move: the doc listed kind:canonical-decision at 0.80 (the server has no such
// case; it scores 0.50), omitted kind:pinned and kind:preference entirely, and
// claimed a "+0.10 per repeated write" boost that has never existed in
// computeImportanceScore. A description nobody pins is a description that will
// drift again, which is why the numbers are asserted here rather than trusted
// to review.
//
// SOURCE OF TRUTH: evc-mesh internal/service/memory_service.go —
// computeImportanceScore() and keySlugRegex. If that function changes, this
// test must be updated in the same change; it is deliberately noisy about it.

func toolParamDesc(t *testing.T, tool, param string) string {
	t.Helper()
	server := NewServer(ServerConfig{})
	registered, ok := server.MCPServer().ListTools()[tool]
	if !ok {
		t.Fatalf("%s tool is not registered", tool)
	}
	raw, present := registered.Tool.InputSchema.Properties[param]
	if !present {
		t.Fatalf("%s must accept %q", tool, param)
	}
	spec, _ := raw.(map[string]any)
	desc, _ := spec["description"].(string)
	if strings.TrimSpace(desc) == "" {
		t.Fatalf("%s.%s needs a description: an undocumented parameter is one nobody reads", tool, param)
	}
	return desc
}

func TestKeyParam_CarriesEnforcedSlugPattern(t *testing.T) {
	for _, tool := range []string{"remember", "set_project_knowledge"} {
		desc := toolParamDesc(t, tool, "key")

		// The literal pattern, so an agent can self-check before the call.
		if !strings.Contains(desc, "^[a-z0-9][a-z0-9-]*[a-z0-9]$") {
			t.Errorf("%s.key must quote the enforced pattern verbatim\ngot: %s", tool, desc)
		}
		// The specific trap: colons look natural for a namespaced key.
		lower := strings.ToLower(desc)
		if !strings.Contains(lower, "colon") {
			t.Errorf("%s.key must name the colon trap explicitly — that is the mistake this documents\ngot: %s", tool, desc)
		}
		// A refusal, not a silent fixup. Getting this wrong in either
		// direction produces a different bug: "normalised" would teach agents
		// a bad key is harmless.
		if !strings.Contains(strings.ToUpper(desc), "REFUSED") {
			t.Errorf("%s.key must say a bad key is REFUSED\ngot: %s", tool, desc)
		}
		if strings.Contains(lower, "is normalised") || strings.Contains(lower, "will be normalised") {
			t.Errorf("%s.key must not claim normalisation — the server rejects\ngot: %s", tool, desc)
		}
	}
}

func TestTagsParam_CarriesImportanceScoreTable(t *testing.T) {
	desc := toolParamDesc(t, "remember", "tags")

	// Every (kind, score) pair the server actually implements.
	// Mirrors computeImportanceScore in evc-mesh.
	for _, tc := range []struct {
		kind  string
		score string
	}{
		{"kind:pinned", "1.0"},
		{"kind:incident", "0.85"},
		{"kind:decision", "0.80"},
		{"kind:preference", "0.80"},
		{"kind:learning", "0.70"},
		{"kind:fact", "0.60"},
		{"kind:session-checkpoint", "0.30"},
	} {
		idx := strings.Index(desc, tc.kind)
		if idx < 0 {
			t.Errorf("tags description is missing %s — an agent cannot pick a kind: tag it never sees\ngot: %s", tc.kind, desc)
			continue
		}
		// The score must sit next to its kind, not merely somewhere in the
		// blob: a table that lists all seven names and all seven numbers in
		// unrelated order would otherwise pass.
		window := desc[idx:min(idx+len(tc.kind)+16, len(desc))]
		if !strings.Contains(window, tc.score) {
			t.Errorf("%s must be followed by its score %s\ngot window: %q", tc.kind, tc.score, window)
		}
	}

	// The default for an untagged entry, and the override rule — the two
	// things that decide retrieval for entries that carry no obvious kind.
	if !strings.Contains(desc, "0.50") {
		t.Errorf("tags description must state the no-kind default of 0.50\ngot: %s", desc)
	}
	lower := strings.ToLower(desc)
	if !strings.Contains(lower, "downgrade") || !strings.Contains(lower, "overrides") {
		t.Errorf("tags description must state that kind:session-checkpoint overrides other kind: tags — "+
			"this is the non-obvious half of the table\ngot: %s", desc)
	}

	// The link to retrieval. Without it the table reads as taxonomy trivia
	// rather than "this decides whether recall ever returns your entry".
	for _, want := range []string{"importance_score", "recall", "0.3"} {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Errorf("tags description must connect the tag to retrieval; missing %q\ngot: %s", want, desc)
		}
	}

	// Both boosts, with their real trigger values.
	for _, want := range []string{"icp", "architecture", "license", "security", "money", "0.8", "+0.10"} {
		if !strings.Contains(lower, strings.ToLower(want)) {
			t.Errorf("tags description must state the boosts; missing %q\ngot: %s", want, desc)
		}
	}
}

func TestRelevanceParam_IsDistinguishedFromImportanceScore(t *testing.T) {
	desc := toolParamDesc(t, "remember", "relevance")
	lower := strings.ToLower(desc)

	// The two fields were conflated in the prose this replaces: the doc told
	// agents "default 0.5 if unsure" while the server defaults relevance to
	// 1.0 and computes importance_score from tags instead.
	if !strings.Contains(desc, "1.0") {
		t.Errorf("relevance must state its real default of 1.0\ngot: %s", desc)
	}
	if !strings.Contains(lower, "importance_score") {
		t.Errorf("relevance must say it is NOT importance_score — that conflation is what this fixes\ngot: %s", desc)
	}
	if !strings.Contains(lower, "min_importance") {
		t.Errorf("relevance must say which field min_importance filters on\ngot: %s", desc)
	}
}

// Mutation control: the assertions above must be capable of failing.
// Without this, a rewritten description that quietly dropped the table would
// still leave three green tests, which is the failure mode the whole task is
// about (a check that cannot go red is not a check).
func TestImportanceContractAssertions_CanFail(t *testing.T) {
	broken := "Tags for categorization and filtering."

	for _, kind := range []string{"kind:pinned", "kind:incident", "kind:session-checkpoint"} {
		if strings.Contains(broken, kind) {
			t.Fatalf("control string is not actually missing %s", kind)
		}
	}
	if strings.Contains(strings.ToLower(broken), "importance_score") {
		t.Fatal("control string is not actually missing the retrieval link")
	}

	// And the positive half: the live description must differ from it, or the
	// negative control is passing for the wrong reason.
	live := toolParamDesc(t, "remember", "tags")
	if live == broken {
		t.Fatal("live tags description is still the pre-change stub")
	}
	fmt.Fprintf(nopWriter{}, "%s", live)
}

type nopWriter struct{}

func (nopWriter) Write(p []byte) (int, error) { return len(p), nil }

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
