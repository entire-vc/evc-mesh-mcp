package mcp

import "strings"

// RecallProfile identifies a named query-type-aware recall preset.
type RecallProfile string

const (
	ProfileTemporal     RecallProfile = "temporal"
	ProfileMultiSession RecallProfile = "multi-session"
	ProfileFactual      RecallProfile = "factual"
	ProfileDefault      RecallProfile = "default"
)

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

var temporalKeywords = []string{
	"when", "ago", "yesterday", "last week", "recently", "before", "after",
}

var multiSessionKeywords = []string{
	"pattern", "trend", "history", "always", "recurring", "baseline",
}

// ClassifyQuery returns the RecallProfile best matching the query.
// Priority: temporal > multi-session > factual > default.
func ClassifyQuery(query string) RecallProfile {
	lower := strings.ToLower(query)

	for _, kw := range temporalKeywords {
		if strings.Contains(lower, kw) {
			return ProfileTemporal
		}
	}
	// Modern date patterns like "2026-07" or "2025-01" — require word boundary before "20".
	for i := 0; i <= len(lower)-5; i++ {
		if lower[i] == '2' && lower[i+1] == '0' &&
			lower[i+2] >= '0' && lower[i+2] <= '9' &&
			lower[i+3] >= '0' && lower[i+3] <= '9' &&
			lower[i+4] == '-' {
			if i == 0 || !isAlphaNumByte(lower[i-1]) {
				return ProfileTemporal
			}
		}
	}

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

func isAlphaNumByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
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

// GetProfileParams returns the ProfileParams for the given profile.
func GetProfileParams(profile RecallProfile) ProfileParams {
	switch profile {
	case ProfileTemporal:
		return ProfileParams{
			ApplyDecay:   true,
			HalfLifeDays: 7,
			OrderBy:      "decayed_relevance:desc",
		}
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
