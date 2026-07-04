package mcp

import (
	"testing"
)

func TestClassifyQuery_Temporal(t *testing.T) {
	cases := []string{
		"what happened yesterday",
		"show me entries from last week",
		"what did we do recently",
		"events before the migration",
		"tasks completed after the deploy",
		"what was the state ago",
		"when was the PR merged",
		"incident on 2026-07-01",
	}
	for _, q := range cases {
		if got := ClassifyQuery(q); got != ProfileTemporal {
			t.Errorf("ClassifyQuery(%q) = %q, want %q", q, got, ProfileTemporal)
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
	// temporal beats multi-session: "recently" is temporal, "pattern" is multi-session
	q := "pattern noticed recently in prod"
	if got := ClassifyQuery(q); got != ProfileTemporal {
		t.Errorf("ClassifyQuery(%q) = %q, want temporal (higher priority)", q, got)
	}

	// multi-session beats factual: "trend" matches multi-session, query is short
	q = "trend UPPER_CASE"
	if got := ClassifyQuery(q); got != ProfileMultiSession {
		t.Errorf("ClassifyQuery(%q) = %q, want multi-session (higher priority)", q, got)
	}
}

func TestGetProfileParams_Temporal(t *testing.T) {
	pp := GetProfileParams(ProfileTemporal)
	if !pp.ApplyDecay {
		t.Error("temporal profile: ApplyDecay should be true")
	}
	if pp.HalfLifeDays != 7 {
		t.Errorf("temporal profile: HalfLifeDays = %d, want 7", pp.HalfLifeDays)
	}
	if pp.OrderBy != "decayed_relevance:desc" {
		t.Errorf("temporal profile: OrderBy = %q, want decayed_relevance:desc", pp.OrderBy)
	}
	if pp.MinImportance != 0 {
		t.Errorf("temporal profile: MinImportance should be 0 (no override), got %g", pp.MinImportance)
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

func TestClassifyQuery_YearPattern(t *testing.T) {
	// "2026-07" should classify as temporal
	q := "events in 2026-07 around deploy"
	if got := ClassifyQuery(q); got != ProfileTemporal {
		t.Errorf("ClassifyQuery(%q) = %q, want temporal", q, got)
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
