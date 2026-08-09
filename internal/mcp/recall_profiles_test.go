package mcp

import (
	"testing"
)

// A query being ABOUT time must not route into a preset of its own. These are
// the exact strings the removed `temporal` profile claimed, including the
// ISO-date pattern; each must now fall through to the default.
func TestClassifyQuery_TimeFlavouredQueriesAreDefault(t *testing.T) {
	cases := []string{
		"what happened yesterday",
		"show me entries from last week",
		"what did we do recently",
		"when was the PR merged",
		"how many days ago did I attend the baking class",
		"in what order did the three trips happen",
		"incident on 2026-07-01",
	}
	for _, q := range cases {
		if got := ClassifyQuery(q); got != ProfileDefault {
			t.Errorf("ClassifyQuery(%q) = %q, want %q", q, got, ProfileDefault)
		}
	}
}

func TestClassifyQuery_MultiSession(t *testing.T) {
	cases := []string{
		"recurring pattern in memory writes",
		"show trend over multiple sessions",
		"history of recall failures",
		"always fails on Tuesday",
		"baseline from last month",
	}
	for _, q := range cases {
		if got := ClassifyQuery(q); got != ProfileMultiSession {
			t.Errorf("ClassifyQuery(%q) = %q, want %q", q, got, ProfileMultiSession)
		}
	}
}

func TestClassifyQuery_Factual(t *testing.T) {
	cases := []string{
		"RECALL_GRAPH_ENABLED setting",
		"MESH_API_KEY value",
		"4e2c1857-a501-48b7-90a2-5766345d99d3",
		"/api/v1/memories/search",
		"~/bin/mesh-mcp path",
	}
	for _, q := range cases {
		if got := ClassifyQuery(q); got != ProfileFactual {
			t.Errorf("ClassifyQuery(%q) = %q, want %q", q, got, ProfileFactual)
		}
	}
}

func TestClassifyQuery_Default(t *testing.T) {
	cases := []string{
		"what is the current authentication approach for agents",
		"how do we handle errors in the mesh dispatcher pipeline",
		"show me all memories related to the sprint retrospective",
	}
	for _, q := range cases {
		if got := ClassifyQuery(q); got != ProfileDefault {
			t.Errorf("ClassifyQuery(%q) = %q, want %q", q, got, ProfileDefault)
		}
	}
}

func TestClassifyQuery_Priority(t *testing.T) {
	// multi-session is now the top priority: a time word no longer outranks it.
	q := "pattern noticed recently in prod"
	if got := ClassifyQuery(q); got != ProfileMultiSession {
		t.Errorf("ClassifyQuery(%q) = %q, want multi-session (highest priority)", q, got)
	}

	// multi-session beats factual: "trend" matches multi-session, query is short
	q = "trend UPPER_CASE"
	if got := ClassifyQuery(q); got != ProfileMultiSession {
		t.Errorf("ClassifyQuery(%q) = %q, want multi-session (higher priority)", q, got)
	}
}

// The property, not the absence of one identifier: NO profile may arm recency
// decay. `handleRecall` reads `pp.ApplyDecay` one way — it can force decay ON
// over the caller's explicit false, and can never force it OFF — so a preset
// here is not a default, it is an override the caller cannot refuse.
//
// Since evc-mesh#540 an OrderBy of "decayed_relevance:desc" arms decay on the
// server BY ITSELF, so that string is checked too: re-adding it would restore
// decay silently, at the server's 30-day half-life rather than the 7 this
// change measured as harmful.
//
// Mutation check (re-run after editing this file): put
// `ApplyDecay: true, HalfLifeDays: 7, OrderBy: "decayed_relevance:desc"` back
// on any case in GetProfileParams — this test must go red, and it is the only
// thing standing between an intuition about time and a fleet-wide reranking.
func TestNoProfileArmsRecencyDecay(t *testing.T) {
	for _, p := range []RecallProfile{
		ProfileMultiSession, ProfileFactual, ProfileDefault,
		RecallProfile("temporal"), // the removed profile, and any unknown string
	} {
		pp := GetProfileParams(p)
		if pp.ApplyDecay {
			t.Errorf("profile %q: ApplyDecay must be false — a profile can force decay ON but never OFF", p)
		}
		if pp.HalfLifeDays != 0 {
			t.Errorf("profile %q: HalfLifeDays = %d, want 0", p, pp.HalfLifeDays)
		}
		if pp.OrderBy == "decayed_relevance:desc" {
			t.Errorf("profile %q: OrderBy %q arms decay server-side by itself (evc-mesh#540)", p, pp.OrderBy)
		}
	}
}

// An unknown or retired profile name must degrade to the default preset rather
// than to a zero value that happens to look like one: `recall_profile=temporal`
// is still a legal argument callers may pass.
func TestGetProfileParams_RetiredTemporalFallsBackToDefault(t *testing.T) {
	if got, want := GetProfileParams(RecallProfile("temporal")), GetProfileParams(ProfileDefault); got != want {
		t.Errorf("GetProfileParams(\"temporal\") = %+v, want the default preset %+v", got, want)
	}
}

func TestGetProfileParams_MultiSession(t *testing.T) {
	pp := GetProfileParams(ProfileMultiSession)
	if pp.ApplyDecay {
		t.Error("multi-session profile: ApplyDecay should be false")
	}
	if pp.MinImportance != 0.2 {
		t.Errorf("multi-session profile: MinImportance = %g, want 0.2", pp.MinImportance)
	}
	if pp.Limit != 20 {
		t.Errorf("multi-session profile: Limit = %d, want 20", pp.Limit)
	}
	if !pp.IncludeSuperseded {
		t.Error("multi-session profile: IncludeSuperseded should be true")
	}
}

func TestGetProfileParams_Factual(t *testing.T) {
	pp := GetProfileParams(ProfileFactual)
	if pp.ApplyDecay {
		t.Error("factual profile: ApplyDecay should be false")
	}
	if pp.MinImportance != 0.5 {
		t.Errorf("factual profile: MinImportance = %g, want 0.5", pp.MinImportance)
	}
	if pp.OrderBy != "relevance:desc" {
		t.Errorf("factual profile: OrderBy = %q, want relevance:desc", pp.OrderBy)
	}
}

func TestGetProfileParams_Default(t *testing.T) {
	pp := GetProfileParams(ProfileDefault)
	if pp.ApplyDecay {
		t.Error("default profile: ApplyDecay should be false")
	}
	if pp.MinImportance != 0 {
		t.Errorf("default profile: MinImportance should be 0, got %g", pp.MinImportance)
	}
	if pp.OrderBy != "" {
		t.Errorf("default profile: OrderBy should be empty, got %q", pp.OrderBy)
	}
	if pp.Limit != 0 {
		t.Errorf("default profile: Limit should be 0, got %d", pp.Limit)
	}
}

func TestClassifyQuery_YearPatternIsNotSpecial(t *testing.T) {
	// An ISO date in the query used to route to the temporal preset. It no
	// longer routes anywhere: the date is a search term like any other.
	q := "events in 2026-07 around deploy"
	if got := ClassifyQuery(q); got != ProfileDefault {
		t.Errorf("ClassifyQuery(%q) = %q, want default", q, got)
	}
}

func TestClassifyQuery_UUIDFactual(t *testing.T) {
	// Long UUID — short query → factual
	q := "4e2c1857-a501"
	if got := ClassifyQuery(q); got != ProfileFactual {
		t.Errorf("ClassifyQuery(%q) = %q, want factual", q, got)
	}
}

func TestClassifyQuery_PathFactual(t *testing.T) {
	q := "/internal/mcp/tools.go"
	if got := ClassifyQuery(q); got != ProfileFactual {
		t.Errorf("ClassifyQuery(%q) = %q, want factual", q, got)
	}
}

func TestQueryContainsEnvVar(t *testing.T) {
	if !queryContainsEnvVar("RECALL_GRAPH_ENABLED") {
		t.Error("RECALL_GRAPH_ENABLED should match env-var pattern")
	}
	if !queryContainsEnvVar("set MESH_API_KEY to value") {
		t.Error("MESH_API_KEY should match env-var pattern")
	}
	if queryContainsEnvVar("lowercase word here") {
		t.Error("lowercase should not match env-var pattern")
	}
}
