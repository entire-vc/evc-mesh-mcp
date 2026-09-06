package mcp

import "strings"

// RecallProfile identifies a named query-type-aware recall preset.
type RecallProfile string

const (
	ProfileMultiSession RecallProfile = "multi-session"
	ProfileFactual      RecallProfile = "factual"
	ProfileDefault      RecallProfile = "default"
)

// There is deliberately no `temporal` profile. It existed until 2026-08-09 and
// forced `apply_recency_decay=true` with a 7-day half-life on any query whose
// text mentioned time — over the caller's explicit `false`, because
// `handleRecall` lets a profile turn decay ON but never OFF.
//
// A query being ABOUT time does not make its answer RECENT: "how many days ago
// did I attend the baking class" is answered by the oldest matching session,
// and recency decay is precisely the ranking that buries it. Measured on
// LongMemEval-S once evc-mesh#535 gave the bench realistically-aged fixtures
// (same server, same corpus, only this file's binary differing across arms):
// the one question this profile was operative on went from gold rank 32/33 —
// far outside top-10, a miss in both runs — to rank 1 with the profile gone.
// Softening the half-life to 30 days recovered it only to rank 9, one place
// from falling out of the window again, while demoting a question that had
// been rank 1 in every run without decay.
//
// Restoring a decay preset here therefore needs a MEASUREMENT, not an
// intuition — and note that since evc-mesh#540 an `OrderBy` of
// "decayed_relevance:desc" arms decay on the server BY ITSELF, so putting that
// string back into any profile silently re-enables it at the server's default
// 30-day half-life.

// ProfileParams holds the parameter overrides applied by a recall profile.
// Zero/nil values mean "no override — use the caller's value or server default".
type ProfileParams struct {
	ApplyDecay        bool
	HalfLifeDays      int
	MinImportance     float64
	OrderBy           string
	Limit             int
	IncludeSuperseded bool // when true → sends exclude_superseded=false to include superseded entries
}

var multiSessionKeywords = []string{
	"pattern", "trend", "history", "always", "recurring", "baseline",
}

// ClassifyQuery returns the RecallProfile best matching the query.
// Priority: multi-session > factual > default.
//
// Time-flavoured queries are NOT special-cased — see the note on the profile
// constants above for the measurement that removed that branch.
func ClassifyQuery(query string) RecallProfile {
	lower := strings.ToLower(query)

	for _, kw := range multiSessionKeywords {
		if strings.Contains(lower, kw) {
			return ProfileMultiSession
		}
	}

	// factual: short query (<7 words) with UUID, file path, or env-var.
	if len(strings.Fields(query)) < 7 {
		if queryContainsUUID(lower) || queryContainsPath(lower) || queryContainsEnvVar(query) {
			return ProfileFactual
		}
	}

	return ProfileDefault
}

// queryContainsUUID reports whether s contains an 8-hex-char segment followed by a dash,
// which is characteristic of UUID strings like "4e2c1857-a501-...".
func queryContainsUUID(s string) bool {
	n := len(s)
	for i := 0; i <= n-9; i++ {
		if s[i+8] == '-' && isHexRun(s, i, 8) {
			return true
		}
	}
	return false
}

func isHexRun(s string, start, length int) bool {
	for j := start; j < start+length; j++ {
		c := s[j]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// queryContainsPath reports whether s looks like a file system path.
func queryContainsPath(s string) bool {
	return strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~/") || strings.Contains(s, "//")
}

// queryContainsEnvVar reports whether query contains an UPPER_CASE_WORD with at least one underscore.
func queryContainsEnvVar(query string) bool {
	for _, word := range strings.Fields(query) {
		if len(word) < 4 || !strings.Contains(word, "_") {
			continue
		}
		upper := true
		for _, c := range word {
			if !((c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '_') {
				upper = false
				break
			}
		}
		if upper {
			return true
		}
	}
	return false
}

// resolveProfileOrderBy applies a profile's `order_by` preset the way every
// preset must be applied: it fills in what the caller left unsaid, and never
// overrules what the caller said.
//
// It exists as a function rather than as two lines inside `handleRecall` so the
// test can exercise the real resolution instead of a copy of it. The sibling
// rule for `limit` is still mirrored in its test, and a mirror is only ever as
// true as the day it was written.
func resolveProfileOrderBy(profileOrderBy, callerOrderBy string) string {
	if callerOrderBy != "" {
		return callerOrderBy
	}
	return profileOrderBy
}

// GetProfileParams returns the ProfileParams for the given profile.
func GetProfileParams(profile RecallProfile) ProfileParams {
	switch profile {
	case ProfileMultiSession:
		return ProfileParams{
			MinImportance:     0.2,
			Limit:             20,
			IncludeSuperseded: true,
		}
	case ProfileFactual:
		return ProfileParams{
			MinImportance: 0.5,
			OrderBy:       "relevance:desc",
		}
	default:
		return ProfileParams{}
	}
}

// recallDefaultMinImportance is this client's fallback when the caller passes no
// min_importance. It must never exceed the server's own default — see
// recall_min_importance_default_test.go for why a stricter value here silently
// replaces the server's instead of deferring to it (#a9752575).
const recallDefaultMinImportance = 0.3
