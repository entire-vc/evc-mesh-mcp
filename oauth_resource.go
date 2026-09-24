package main

import (
	"encoding/json"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
)

// OAuth protected-resource support (RFC 9728).
//
// The Mesh API is the authorization server; this binary is only the resource
// server. What it owes an OAuth client is (1) a 401 whose WWW-Authenticate
// header says where the resource's metadata lives, and (2) that metadata
// document, which in turn names the authorization server. A client that gets
// a 200 with the header, or a 401 without it, has nothing to start from.

// oauthScope is the single scope the Mesh authorization server issues.
const oauthScope = "mesh"

// protectedResourceWellKnown is the RFC 9728 well-known prefix. The metadata
// for a resource lives at this prefix plus the resource URL's own path, so
// each profile has a distinct document.
const protectedResourceWellKnown = "/.well-known/oauth-protected-resource"

// mcpProfile identifies which of the two Streamable HTTP endpoints a request
// is for; each is a separate OAuth resource with its own metadata.
type mcpProfile int

const (
	profileFull mcpProfile = iota
	profileCore
)

// oauthResources derives resource URLs, the metadata URLs that point at them,
// and the authorization-server issuer, from configuration.
type oauthResources struct {
	// publicURL is MESH_MCP_PUBLIC_URL: the URL the MCP root is reachable at
	// from outside (e.g. https://mesh.example.com/mcp). Empty when the server
	// is reached directly, in which case the URLs are derived per request.
	publicURL string
	// issuer overrides the authorization-server issuer
	// (MESH_MCP_OAUTH_ISSUER). Empty: the origin of the resource URL, which is
	// right whenever the MCP server is served under the Mesh instance's own
	// origin, as it is on every stock deployment.
	issuer string
}

func newOAuthResources(publicURL, issuer string) *oauthResources {
	return &oauthResources{
		publicURL: strings.TrimRight(strings.TrimSpace(publicURL), "/"),
		issuer:    strings.TrimRight(strings.TrimSpace(issuer), "/"),
	}
}

// resourceURL is the URL of the profile's endpoint exactly as a user enters
// it into a client. With a public URL configured: the URL itself for the full
// profile, and <URL>/core for core — the same shape the proxy routes. Without
// one, the paths this binary really serves, on the origin the request used.
func (o *oauthResources) resourceURL(r *http.Request, p mcpProfile) *url.URL {
	var u *url.URL
	if o.publicURL != "" {
		if parsed, err := url.Parse(o.publicURL); err == nil && parsed.Host != "" {
			u = parsed
			if p == profileCore {
				u.Path = strings.TrimRight(u.Path, "/") + coreBasePath
			}
			u.RawQuery, u.Fragment = "", ""
			return u
		}
	}
	path := streamablePath
	if p == profileCore {
		path = coreBasePath
	}
	return &url.URL{Scheme: requestScheme(r), Host: requestHost(r), Path: path}
}

// metadataURL is where the profile's protected-resource metadata is served.
func (o *oauthResources) metadataURL(r *http.Request, p mcpProfile) string {
	res := o.resourceURL(r, p)
	m := url.URL{Scheme: res.Scheme, Host: res.Host, Path: o.metadataPath(r, p)}
	return m.String()
}

// metadataPath is the path part of metadataURL: the RFC 9728 prefix followed
// by the resource URL's own path.
func (o *oauthResources) metadataPath(r *http.Request, p mcpProfile) string {
	return protectedResourceWellKnown + strings.TrimRight(o.resourceURL(r, p).Path, "/")
}

// issuerURL is the authorization server named in the metadata.
func (o *oauthResources) issuerURL(r *http.Request) string {
	if o.issuer != "" {
		return o.issuer
	}
	res := o.resourceURL(r, profileFull)
	return (&url.URL{Scheme: res.Scheme, Host: res.Host}).String()
}

// challenge is the WWW-Authenticate value sent with a 401 for the profile.
func (o *oauthResources) challenge(r *http.Request, p mcpProfile) string {
	return `Bearer resource_metadata="` + o.metadataURL(r, p) + `", scope="` + oauthScope + `"`
}

// handler serves the protected-resource metadata documents. Each profile's
// document sits at the path RFC 9728 derives from its resource URL; the bare
// prefix answers with the full profile's document as a courtesy to clients
// that probe it first. Anything else under the prefix is a 404 — a JSON one,
// so a client never mistakes an error page for metadata.
func (o *oauthResources) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD, OPTIONS")
			writeJSONError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}

		path := strings.TrimRight(r.URL.Path, "/")
		var profile mcpProfile
		switch {
		case path == protectedResourceWellKnown, path == o.metadataPath(r, profileFull):
			profile = profileFull
		case path == o.metadataPath(r, profileCore):
			profile = profileCore
		default:
			writeJSONError(w, http.StatusNotFound, "not_found")
			return
		}

		doc := map[string]any{
			"resource":                 o.resourceURL(r, profile).String(),
			"authorization_servers":    []string{o.issuerURL(r)},
			"scopes_supported":         []string{oauthScope},
			"bearer_methods_supported": []string{"header"},
		}
		w.Header().Set("Content-Type", "application/json")
		if o.publicURL != "" {
			w.Header().Set("Cache-Control", "public, max-age=300")
		} else {
			// Derived from the request's own origin: a shared cache must not
			// hand one caller's origin to another.
			w.Header().Set("Cache-Control", "no-store")
		}
		w.WriteHeader(http.StatusOK)
		if r.Method == http.MethodHead {
			return
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
}

func writeJSONError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

// requestScheme and requestHost describe the origin a client used to reach
// this server, honouring a fronting proxy's forwarding headers. They only
// matter when MESH_MCP_PUBLIC_URL is unset, i.e. no proxy is configured to
// speak for the server.
func requestScheme(r *http.Request) string {
	if v := firstHeaderValue(r, "X-Forwarded-Proto"); v == "http" || v == "https" {
		return v
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// hostPattern is a DNS name, IPv4 address or bracketed IPv6 address with an
// optional port. Anything else in a Host or X-Forwarded-Host header is not
// echoed into a URL: it lands inside a quoted WWW-Authenticate parameter and a
// JSON document, and a client-supplied '"' would end the quote early.
var hostPattern = regexp.MustCompile(`^(?:[A-Za-z0-9](?:[A-Za-z0-9.-]*[A-Za-z0-9])?|\[[0-9A-Fa-f:.]+\])(?::[0-9]{1,5})?$`)

func requestHost(r *http.Request) string {
	if v := firstHeaderValue(r, "X-Forwarded-Host"); hostPattern.MatchString(v) {
		return v
	}
	if hostPattern.MatchString(r.Host) {
		return r.Host
	}
	return "localhost"
}

// warnIfMisconfigured logs, once at startup, the two configurations that make
// the advertised OAuth URLs wrong without failing anything else.
func (o *oauthResources) warnIfMisconfigured() {
	if o.publicURL == "" {
		log.Printf("OAuth: MESH_MCP_PUBLIC_URL is unset; resource URLs are derived from each request's own host, which is only right when clients reach this server directly. Set it to the URL users enter into their client (behind a proxy the paths differ, e.g. /mcp/core, not /core).")
		return
	}
	if u, err := url.Parse(o.publicURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		log.Printf("OAuth: MESH_MCP_PUBLIC_URL=%q is not an absolute http(s) URL; OAuth metadata falls back to per-request URLs", o.publicURL)
	}
}

func firstHeaderValue(r *http.Request, name string) string {
	v := r.Header.Get(name)
	if i := strings.Index(v, ","); i >= 0 {
		v = v[:i]
	}
	return strings.TrimSpace(v)
}
