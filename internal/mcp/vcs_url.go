package mcp

import (
	"net/url"
	"strings"
)

// Canonical link_type values accepted by the API.
const (
	vcsLinkTypePR     = "pr"
	vcsLinkTypeCommit = "commit"
	vcsLinkTypeBranch = "branch"
)

// vcsLinkTypeAliases maps the spellings a caller might reasonably use onto
// the canonical value. It deliberately mirrors the API's accepted set rather
// than extending it, so this client never accepts something the API would
// reject; normalising here means the tool behaves identically against an API
// that has not yet learned the alias.
var vcsLinkTypeAliases = map[string]string{
	"pr":           vcsLinkTypePR,
	"pull_request": vcsLinkTypePR,
	"commit":       vcsLinkTypeCommit,
	"branch":       vcsLinkTypeBranch,
}

// normalizeVCSLinkType folds case and resolves aliases to the canonical
// link_type. An unrecognised value is returned lower-cased and unchanged so
// the API — not this client — gets to decide whether it is valid.
func normalizeVCSLinkType(raw string) string {
	cleaned := strings.ToLower(strings.TrimSpace(raw))
	if canonical, ok := vcsLinkTypeAliases[cleaned]; ok {
		return canonical
	}
	return cleaned
}

// vcsURLFacts holds what a VCS URL says about itself. Every field is best
// effort: a URL shape we do not recognise yields a zero value, and the caller
// falls back to explicit arguments.
type vcsURLFacts struct {
	Provider   string // "github" | "gitlab"
	LinkType   string // canonical link_type
	ExternalID string // PR number, commit SHA, or branch name
	Repository string // "owner/repo", for the default title
}

// parseVCSURL infers provider, link_type and external_id from a hosted VCS
// URL. It recognises the forms an agent actually pastes:
//
//	https://github.com/owner/repo/pull/123
//	https://github.com/owner/repo/commit/<sha>
//	https://github.com/owner/repo/tree/<branch>
//	https://gitlab.com/group/repo/-/merge_requests/123
//
// This is the whole point of the tool: linking the PR you are already looking
// at should not require restating three fields the URL already contains.
func parseVCSURL(raw string) vcsURLFacts {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return vcsURLFacts{}
	}

	host := strings.ToLower(u.Host)
	if h, _, found := strings.Cut(host, ":"); found {
		host = h
	}
	host = strings.TrimPrefix(host, "www.")

	var facts vcsURLFacts
	switch {
	case host == "github.com" || strings.HasSuffix(host, ".github.com"):
		facts.Provider = "github"
	case host == "gitlab.com" || strings.HasSuffix(host, ".gitlab.com"):
		facts.Provider = "gitlab"
	default:
		// Self-hosted instances are indistinguishable from any other host by
		// URL alone; leave the provider to the caller but still try the path,
		// since both products use the same path grammar.
	}

	segments := splitPath(u.Path)
	if len(segments) < 4 {
		return facts
	}
	facts.Repository = segments[0] + "/" + segments[1]

	// GitLab puts a literal "-" between the project path and the resource.
	rest := segments[2:]
	if rest[0] == "-" {
		rest = rest[1:]
	}
	if len(rest) < 2 {
		return facts
	}

	switch rest[0] {
	case "pull", "pulls", "merge_requests":
		if isAllDigits(rest[1]) {
			facts.LinkType = vcsLinkTypePR
			facts.ExternalID = rest[1]
		}
	case "commit", "commits":
		facts.LinkType = vcsLinkTypeCommit
		facts.ExternalID = rest[1]
	case "tree":
		facts.LinkType = vcsLinkTypeBranch
		// Branch names may contain slashes (feature/foo).
		facts.ExternalID = strings.Join(rest[1:], "/")
	}

	return facts
}

// splitPath returns the non-empty segments of a URL path.
func splitPath(p string) []string {
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
