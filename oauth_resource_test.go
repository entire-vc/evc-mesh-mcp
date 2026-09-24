package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpserver "github.com/entire-vc/evc-mesh-mcp/internal/mcp"
	sdkserver "github.com/mark3labs/mcp-go/server"
)

const (
	testPublicURL = "https://mesh.example.com/mcp"
	testFullMeta  = "https://mesh.example.com/.well-known/oauth-protected-resource/mcp"
	testCoreMeta  = "https://mesh.example.com/.well-known/oauth-protected-resource/mcp/core"
)

func wantChallenge(metaURL string) string {
	return `Bearer resource_metadata="` + metaURL + `", scope="mesh"`
}

// oauthAwareAPI is a fake Mesh API that recognises one agent key (X-Agent-Key)
// and one OAuth access token (Authorization: Bearer) the way the real API does
// — each credential only in its own header — and can be told to revoke the
// token or to fail with a gateway error.
type oauthAwareAPI struct {
	*httptest.Server
	revoked atomic.Bool
	gateway atomic.Bool
	// failStatus, when non-zero, is answered to every request (429, 500, ...).
	failStatus atomic.Int64
	// acceptPrefix, when set, makes every "Bearer <acceptPrefix>..." token valid,
	// to model many distinct users behind one address.
	acceptPrefix string
	meCalls      int64
	lastAuth     atomic.Value // string: Authorization header of the last request
	lastKey      atomic.Value // string: X-Agent-Key header of the last request
}

func newOAuthAwareAPI(t *testing.T, goodKey, goodToken string) *oauthAwareAPI {
	t.Helper()
	a := &oauthAwareAPI{}
	a.lastAuth.Store("")
	a.lastKey.Store("")
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.lastAuth.Store(r.Header.Get("Authorization"))
		a.lastKey.Store(r.Header.Get("X-Agent-Key"))
		if st := int(a.failStatus.Load()); st != 0 {
			http.Error(w, `{"error":"induced"}`, st)
			return
		}
		if a.gateway.Load() {
			http.Error(w, `{"error":"bad gateway"}`, http.StatusBadGateway)
			return
		}
		ok := (goodKey != "" && r.Header.Get("X-Agent-Key") == goodKey) ||
			(!a.revoked.Load() && r.Header.Get("Authorization") == "Bearer "+goodToken) ||
			(a.acceptPrefix != "" && strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "+a.acceptPrefix))
		if !ok {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/agents/me":
			atomic.AddInt64(&a.meCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":           "11111111-1111-1111-1111-111111111111",
				"workspace_id": "22222222-2222-2222-2222-222222222222",
				"name":         "connector",
				"agent_type":   "agent",
			})
		case "/api/v1/agents/me/tasks":
			_ = json.NewEncoder(w).Encode(map[string]any{"tasks": []any{}, "count": 0})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.Close)
	return a
}

// newOAuthMux assembles the same pieces main() wires for the SSE-mode server:
// per-agent context injection, both profiles behind requireCredential, and the
// metadata handler.
func newOAuthMux(t *testing.T, api *oauthAwareAPI, cache *agentSessionCache, oauth *oauthResources, limiter *ipRateLimiter) http.Handler {
	t.Helper()
	reg := &serverRegistry{apiURL: api.URL}
	rc := mcpserver.NewRESTClient(api.URL, "")
	full := mcpserver.NewServer(mcpserver.ServerConfig{RESTClient: rc, Profile: mcpserver.ProfileFull})
	core := mcpserver.NewServer(mcpserver.ServerConfig{RESTClient: rc, Profile: mcpserver.ProfileCore})
	ctxFn := func(ctx context.Context, r *http.Request) context.Context {
		key := extractAgentKeyFromRequest(r)
		s, err := cache.GetOrAuthenticate(ctx, key)
		if err != nil {
			return ctx
		}
		ctx = mcpserver.ContextWithSession(ctx, s)
		return mcpserver.ContextWithRESTClient(ctx, reg.GetClient(key))
	}
	opts := []sdkserver.StreamableHTTPOption{sdkserver.WithStateLess(true), sdkserver.WithHTTPContextFunc(ctxFn)}
	mux := http.NewServeMux()
	mux.Handle(streamablePath, requireCredential(cache, limiter, "t", oauth, profileFull, sdkserver.NewStreamableHTTPServer(full.MCPServer(), opts...)))
	mux.Handle(coreBasePath, requireCredential(cache, limiter, "t", oauth, profileCore, sdkserver.NewStreamableHTTPServer(core.MCPServer(), opts...)))
	mux.Handle(protectedResourceWellKnown, oauth.handler())
	mux.Handle(protectedResourceWellKnown+"/", oauth.handler())
	return mux
}

func doReq(t *testing.T, method, url string, headers map[string]string, body string) (*http.Response, string) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, string(b)
}

const rpcHeaders = "application/json, text/event-stream"

func rpc(token string) map[string]string {
	h := map[string]string{"Content-Type": "application/json", "Accept": rpcHeaders}
	if token != "" {
		h["Authorization"] = "Bearer " + token
	}
	return h
}

const callGetMyTasks = `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"get_my_tasks","arguments":{}}}`

// Acceptance 1: no token -> 401 + resource_metadata, for both profiles, and
// the header points at a URL that really serves the metadata.
func TestOAuth_NoTokenGets401WithResourceMetadata(t *testing.T) {
	api := newOAuthAwareAPI(t, "agk_good", "mot_good")
	oauth := newOAuthResources(testPublicURL, "")
	ts := httptest.NewServer(newOAuthMux(t, api, &agentSessionCache{apiURL: api.URL}, oauth, nil))
	defer ts.Close()

	for _, tc := range []struct{ path, meta string }{
		{streamablePath, testFullMeta},
		{coreBasePath, testCoreMeta},
	} {
		resp, _ := doReq(t, http.MethodPost, ts.URL+tc.path, rpc(""), `{}`)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status %d, want 401", tc.path, resp.StatusCode)
		}
		if got := resp.Header.Get("WWW-Authenticate"); got != wantChallenge(tc.meta) {
			t.Errorf("%s: WWW-Authenticate = %q, want %q", tc.path, got, wantChallenge(tc.meta))
		}
	}
}

// RED CONTROL for the header assertion above: the wrapper the existing tests
// use (no OAuth configured) still answers with the old realm challenge, so an
// assertion on resource_metadata genuinely fails against the old behaviour.
func TestOAuth_RedControl_LegacyWrapperKeepsRealm(t *testing.T) {
	api := newOAuthAwareAPI(t, "agk_good", "mot_good")
	h := requireAgentKey(&agentSessionCache{apiURL: api.URL}, nil, "t", http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))
	got := rec.Header().Get("WWW-Authenticate")
	if got != `Bearer realm="evc-mesh"` {
		t.Fatalf("legacy challenge = %q", got)
	}
	if strings.Contains(got, "resource_metadata") {
		t.Fatal("legacy challenge must not satisfy the resource_metadata assertion")
	}
}

// Acceptance 2: a valid mot_ token reaches tools/call get_my_tasks with 200,
// and the API sees it as Authorization: Bearer (not X-Agent-Key).
func TestOAuth_MotTokenCallsTool(t *testing.T) {
	api := newOAuthAwareAPI(t, "agk_good", "mot_good")
	oauth := newOAuthResources(testPublicURL, "")
	ts := httptest.NewServer(newOAuthMux(t, api, &agentSessionCache{apiURL: api.URL}, oauth, nil))
	defer ts.Close()

	for _, path := range []string{streamablePath, coreBasePath} {
		resp, body := doReq(t, http.MethodPost, ts.URL+path, rpc("mot_good"), callGetMyTasks)
		if resp.StatusCode != http.StatusOK || strings.Contains(body, `"isError":true`) || !strings.Contains(body, `"result"`) {
			t.Fatalf("%s get_my_tasks under mot_: %d %s", path, resp.StatusCode, body)
		}
	}
	if got := api.lastAuth.Load().(string); got != "Bearer mot_good" {
		t.Errorf("tool call reached the API with Authorization=%q, want Bearer mot_good", got)
	}
	if got := api.lastKey.Load().(string); got != "" {
		t.Errorf("tool call reached the API with X-Agent-Key=%q, want none", got)
	}
}

// Acceptance 2 (negative): a revoked token gets 401 with the same header, once
// the short OAuth cache entry has lapsed — and not before it was ever valid.
func TestOAuth_RevokedTokenGets401SameHeader(t *testing.T) {
	api := newOAuthAwareAPI(t, "agk_good", "mot_good")
	oauth := newOAuthResources(testPublicURL, "")
	cache := &agentSessionCache{apiURL: api.URL, oauthTTL: 20 * time.Millisecond, negTTL: time.Millisecond}
	ts := httptest.NewServer(newOAuthMux(t, api, cache, oauth, nil))
	defer ts.Close()

	if resp, body := doReq(t, http.MethodPost, ts.URL+streamablePath, rpc("mot_good"), callGetMyTasks); resp.StatusCode != http.StatusOK {
		t.Fatalf("before revocation: %d %s", resp.StatusCode, body)
	}
	api.revoked.Store(true)
	time.Sleep(60 * time.Millisecond)

	resp, _ := doReq(t, http.MethodPost, ts.URL+streamablePath, rpc("mot_good"), callGetMyTasks)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked token: %d, want 401", resp.StatusCode)
	}
	if got := resp.Header.Get("WWW-Authenticate"); got != wantChallenge(testFullMeta) {
		t.Errorf("revoked token WWW-Authenticate = %q, want %q", got, wantChallenge(testFullMeta))
	}

	// A token the API never issued behaves the same.
	resp, _ = doReq(t, http.MethodPost, ts.URL+coreBasePath, rpc("mot_never_issued"), callGetMyTasks)
	if resp.StatusCode != http.StatusUnauthorized || resp.Header.Get("WWW-Authenticate") != wantChallenge(testCoreMeta) {
		t.Errorf("unknown token on core: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
}

// A gateway blip while verifying a valid token must not be reported as an
// invalid token: 401 would make the client discard and refresh it.
func TestOAuth_GatewayErrorIs503Not401(t *testing.T) {
	api := newOAuthAwareAPI(t, "agk_good", "mot_good")
	api.gateway.Store(true)
	oauth := newOAuthResources(testPublicURL, "")
	ts := httptest.NewServer(newOAuthMux(t, api, &agentSessionCache{apiURL: api.URL}, oauth, nil))
	defer ts.Close()

	resp, _ := doReq(t, http.MethodPost, ts.URL+streamablePath, rpc("mot_good"), callGetMyTasks)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("gateway error: %d, want 503", resp.StatusCode)
	}
	if resp.Header.Get("WWW-Authenticate") != "" {
		t.Error("a transient failure must not carry an authorization challenge")
	}
}

// Acceptance 3: agent keys keep working through both headers, and a rejected
// key keeps its 403 (no behaviour change for existing clients).
func TestOAuth_AgentKeyRegression(t *testing.T) {
	api := newOAuthAwareAPI(t, "agk_good", "mot_good")
	oauth := newOAuthResources(testPublicURL, "")
	ts := httptest.NewServer(newOAuthMux(t, api, &agentSessionCache{apiURL: api.URL}, oauth, nil))
	defer ts.Close()

	for name, headers := range map[string]map[string]string{
		"Authorization: Bearer agk_": rpc("agk_good"),
		"X-Agent-Key":                {"Content-Type": "application/json", "Accept": rpcHeaders, "X-Agent-Key": "agk_good"},
	} {
		resp, body := doReq(t, http.MethodPost, ts.URL+streamablePath, headers, callGetMyTasks)
		if resp.StatusCode != http.StatusOK || !strings.Contains(body, `"result"`) {
			t.Errorf("%s: %d %s", name, resp.StatusCode, body)
		}
	}
	if got := api.lastKey.Load().(string); got != "agk_good" {
		t.Errorf("agent key must still travel as X-Agent-Key, API saw %q", got)
	}
	if got := api.lastAuth.Load().(string); got != "" {
		t.Errorf("agent key must not be sent as Authorization, API saw %q", got)
	}
	if resp, _ := doReq(t, http.MethodPost, ts.URL+streamablePath, rpc("agk_bad"), callGetMyTasks); resp.StatusCode != http.StatusForbidden {
		t.Errorf("bad agent key: %d, want 403", resp.StatusCode)
	}
}

// An OAuth token is honoured only in Authorization: Bearer — never smuggled
// through X-Agent-Key or the query string, where it would be logged.
func TestOAuth_TokenOnlyAcceptedInBearerHeader(t *testing.T) {
	api := newOAuthAwareAPI(t, "agk_good", "mot_good")
	oauth := newOAuthResources(testPublicURL, "")
	ts := httptest.NewServer(newOAuthMux(t, api, &agentSessionCache{apiURL: api.URL}, oauth, nil))
	defer ts.Close()

	h := map[string]string{"Content-Type": "application/json", "Accept": rpcHeaders, "X-Agent-Key": "mot_good"}
	if resp, _ := doReq(t, http.MethodPost, ts.URL+streamablePath, h, callGetMyTasks); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("mot_ in X-Agent-Key: %d, want 401", resp.StatusCode)
	}
	// (The query string is refused wholesale on these endpoints — see requireCredential.)
	if resp, _ := doReq(t, http.MethodPost, ts.URL+streamablePath+"?agent_key=mot_good", rpc(""), callGetMyTasks); resp.StatusCode != http.StatusBadRequest {
		t.Errorf("mot_ in query: %d, want 400", resp.StatusCode)
	}
}

// Verifying an OAuth token must not spend the per-IP budget: tokens are
// re-verified about once a minute while in use, and many users share a few
// egress addresses, so charging verifications would let honest traffic starve
// itself. Rejections are what the budget is for.
func TestOAuth_VerificationsAreFreeRejectionsAreCharged(t *testing.T) {
	api := newOAuthAwareAPI(t, "agk_good", "mot_good")
	api.acceptPrefix = "mot_user"
	oauth := newOAuthResources(testPublicURL, "")
	limiter := newIPRateLimiter(2)
	cache := &agentSessionCache{apiURL: api.URL, negTTL: time.Millisecond}
	ts := httptest.NewServer(newOAuthMux(t, api, cache, oauth, limiter))
	defer ts.Close()

	// Far more distinct valid tokens than the budget, all from one address.
	for i := 0; i < 8; i++ {
		tok := "mot_user" + string(rune('a'+i))
		if resp, body := doReq(t, http.MethodPost, ts.URL+streamablePath, rpc(tok), callGetMyTasks); resp.StatusCode != http.StatusOK {
			t.Fatalf("valid token %d refused: %d %s", i, resp.StatusCode, body)
		}
	}

	// Rejections spend it: two are answered 401, then the address is 429 —
	// without another round trip to the API.
	before := atomic.LoadInt64(&api.meCalls)
	var codes []int
	for i := 0; i < 4; i++ {
		resp, _ := doReq(t, http.MethodPost, ts.URL+streamablePath, rpc("mot_junk"+string(rune('a'+i))), callGetMyTasks)
		codes = append(codes, resp.StatusCode)
	}
	want := []int{401, 401, 429, 429}
	for i := range want {
		if codes[i] != want[i] {
			t.Fatalf("unknown tokens answered %v, want %v", codes, want)
		}
	}
	if atomic.LoadInt64(&api.meCalls) != before {
		t.Error("rejected lookups must not have been counted as successful /agents/me calls")
	}
}

// Only a verdict on the credential (4xx) is reported as an invalid token and
// remembered against it. An API 429/500 says nothing about the token: 503, and
// the next request asks again instead of hitting a cached failure.
func TestOAuth_NonVerdictErrorsAre503AndNotCached(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway} {
		api := newOAuthAwareAPI(t, "agk_good", "mot_good")
		api.failStatus.Store(int64(status))
		oauth := newOAuthResources(testPublicURL, "")
		cache := &agentSessionCache{apiURL: api.URL} // default 30s negative TTL
		ts := httptest.NewServer(newOAuthMux(t, api, cache, oauth, nil))

		resp, _ := doReq(t, http.MethodPost, ts.URL+streamablePath, rpc("mot_good"), callGetMyTasks)
		if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("WWW-Authenticate") != "" {
			t.Errorf("API %d: got %d / challenge %q, want 503 without challenge", status, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
		// The API recovers: the very next request must succeed, not replay a cached failure.
		api.failStatus.Store(0)
		if resp, body := doReq(t, http.MethodPost, ts.URL+streamablePath, rpc("mot_good"), callGetMyTasks); resp.StatusCode != http.StatusOK {
			t.Errorf("API %d then recovered: %d %s — a non-verdict failure was cached against a valid token", status, resp.StatusCode, body)
		}
		ts.Close()
	}
}

func TestIsCredentialRejection(t *testing.T) {
	for status, want := range map[int]bool{400: true, 401: true, 403: true, 404: true, 408: false, 429: false, 500: false, 502: false, 503: false} {
		err := error(&mcpserver.APIError{StatusCode: status})
		if got := isCredentialRejection(err); got != want {
			t.Errorf("status %d: isCredentialRejection = %v, want %v", status, got, want)
		}
	}
	if isCredentialRejection(context.DeadlineExceeded) {
		t.Error("a network-level error is not a verdict on the credential")
	}
}

// SSE keeps a long-lived stream that a ~1h token would outlive; OAuth is
// refused there rather than half-working.
func TestOAuth_RefusedOnSSEConnect(t *testing.T) {
	rec := httptest.NewRecorder()
	if !refuseOAuthOnSSE(rec, "mot_x") || rec.Code != http.StatusUnauthorized {
		t.Errorf("mot_ on SSE: refused=%v code=%d, want refused 401", rec.Code == http.StatusUnauthorized, rec.Code)
	}
	rec = httptest.NewRecorder()
	if refuseOAuthOnSSE(rec, "agk_x") || rec.Code != http.StatusOK {
		t.Error("agent keys must not be refused on SSE")
	}
}

func TestExtractCredential_BearerSchemeIsCaseInsensitive(t *testing.T) {
	for _, scheme := range []string{"Bearer", "bearer", "BEARER"} {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		r.Header.Set("Authorization", scheme+" mot_abc")
		if got := extractAgentKeyFromRequest(r); got != "mot_abc" {
			t.Errorf("%q scheme: credential %q", scheme, got)
		}
	}
	r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	r.Header.Set("Authorization", "Basic mot_abc")
	if got := extractAgentKeyFromRequest(r); got != "" {
		t.Errorf("Basic scheme must not yield a credential, got %q", got)
	}
}

// OAuth tokens rotate about hourly per user; a client cache keyed by token
// would keep every past token in memory for the life of the process.
func TestServerRegistry_DoesNotRetainOAuthTokens(t *testing.T) {
	reg := &serverRegistry{apiURL: "http://127.0.0.1:1"}
	reg.GetClient("mot_a")
	reg.GetClient("mot_b")
	if len(reg.cache) != 0 {
		t.Errorf("registry retained %d OAuth token clients", len(reg.cache))
	}
	if reg.GetClient("agk_a") != reg.GetClient("agk_a") {
		t.Error("agent-key clients must still be cached")
	}
}

func TestOAuth_CacheTTLForTokensIsShorter(t *testing.T) {
	c := &agentSessionCache{}
	if got, want := c.entryTTL("agk_x"), defaultSessionCacheTTL; got != want {
		t.Errorf("agent key TTL = %v, want %v", got, want)
	}
	if got, want := c.entryTTL("mot_x"), defaultOAuthCacheTTL; got != want {
		t.Errorf("OAuth token TTL = %v, want %v", got, want)
	}
	if defaultOAuthCacheTTL >= defaultSessionCacheTTL {
		t.Error("OAuth token TTL must be shorter than the agent-key TTL")
	}
	// An operator who shortens the general TTL below the OAuth one must not be
	// overridden by it.
	c = &agentSessionCache{ttl: 5 * time.Second}
	if got := c.entryTTL("mot_x"); got != 5*time.Second {
		t.Errorf("OAuth TTL must not exceed the configured session TTL, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Metadata documents
// ---------------------------------------------------------------------------

type resourceDoc struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

func getDoc(t *testing.T, url string) (*http.Response, resourceDoc) {
	t.Helper()
	resp, body := doReq(t, http.MethodGet, url, nil, "")
	var d resourceDoc
	if resp.StatusCode == http.StatusOK {
		if err := json.Unmarshal([]byte(body), &d); err != nil {
			t.Fatalf("%s: not JSON: %v\n%s", url, err, body)
		}
	}
	return resp, d
}

func TestOAuthMetadata_DocumentsPerProfile(t *testing.T) {
	api := newOAuthAwareAPI(t, "", "mot_good")
	ts := httptest.NewServer(newOAuthMux(t, api, &agentSessionCache{apiURL: api.URL}, newOAuthResources(testPublicURL, ""), nil))
	defer ts.Close()

	for _, tc := range []struct{ path, resource string }{
		{"/.well-known/oauth-protected-resource/mcp", "https://mesh.example.com/mcp"},
		{"/.well-known/oauth-protected-resource/mcp/core", "https://mesh.example.com/mcp/core"},
		{"/.well-known/oauth-protected-resource", "https://mesh.example.com/mcp"}, // bare prefix: full profile
		{"/.well-known/oauth-protected-resource/mcp/", "https://mesh.example.com/mcp"},
	} {
		resp, d := getDoc(t, ts.URL+tc.path)
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s: %d", tc.path, resp.StatusCode)
			continue
		}
		if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
			t.Errorf("%s: Content-Type %q — a client must not be able to mistake this for an HTML shell", tc.path, ct)
		}
		if d.Resource != tc.resource {
			t.Errorf("%s: resource = %q, want %q", tc.path, d.Resource, tc.resource)
		}
		if len(d.AuthorizationServers) != 1 || d.AuthorizationServers[0] != "https://mesh.example.com" {
			t.Errorf("%s: authorization_servers = %v", tc.path, d.AuthorizationServers)
		}
		if len(d.ScopesSupported) != 1 || d.ScopesSupported[0] != "mesh" {
			t.Errorf("%s: scopes_supported = %v", tc.path, d.ScopesSupported)
		}
		if resp.Header.Get("Access-Control-Allow-Origin") != "*" {
			t.Errorf("%s: browser clients (MCP Inspector) need CORS on the metadata", tc.path)
		}
	}
}

// The URL in the challenge must be servable: follow it and check the document
// names the very resource that issued the challenge.
func TestOAuthMetadata_ChallengeURLResolvesToMatchingDocument(t *testing.T) {
	api := newOAuthAwareAPI(t, "", "mot_good")
	oauth := newOAuthResources(testPublicURL, "")
	ts := httptest.NewServer(newOAuthMux(t, api, &agentSessionCache{apiURL: api.URL}, oauth, nil))
	defer ts.Close()

	for _, tc := range []struct {
		path    string
		profile mcpProfile
	}{{streamablePath, profileFull}, {coreBasePath, profileCore}} {
		resp, _ := doReq(t, http.MethodPost, ts.URL+tc.path, rpc(""), `{}`)
		ch := resp.Header.Get("WWW-Authenticate")
		parts := strings.Split(ch, `resource_metadata="`)
		if len(parts) < 2 {
			t.Fatalf("%s: no resource_metadata in challenge %q", tc.path, ch)
		}
		metaURL := strings.Split(parts[1], `"`)[0]
		// The public host is not dialable in a test; the path is what routes.
		local := ts.URL + strings.TrimPrefix(metaURL, "https://mesh.example.com")
		_, d := getDoc(t, local)
		if d.Resource != oauth.resourceURL(nil, tc.profile).String() {
			t.Errorf("%s: challenge points at a document for %q", tc.path, d.Resource)
		}
	}
}

func TestOAuthMetadata_UnknownPathsAndMethods(t *testing.T) {
	api := newOAuthAwareAPI(t, "", "mot_good")
	ts := httptest.NewServer(newOAuthMux(t, api, &agentSessionCache{apiURL: api.URL}, newOAuthResources(testPublicURL, ""), nil))
	defer ts.Close()

	resp, body := doReq(t, http.MethodGet, ts.URL+"/.well-known/oauth-protected-resource/nope", nil, "")
	if resp.StatusCode != http.StatusNotFound || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Errorf("unknown resource path: %d %q %s", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	if resp, _ := doReq(t, http.MethodPost, ts.URL+"/.well-known/oauth-protected-resource/mcp", nil, "{}"); resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST metadata: %d, want 405", resp.StatusCode)
	}
	if resp, _ := doReq(t, http.MethodOptions, ts.URL+"/.well-known/oauth-protected-resource/mcp", nil, ""); resp.StatusCode != http.StatusNoContent {
		t.Errorf("OPTIONS metadata: %d, want 204", resp.StatusCode)
	}
	if resp, body := doReq(t, http.MethodHead, ts.URL+"/.well-known/oauth-protected-resource/mcp", nil, ""); resp.StatusCode != http.StatusOK || body != "" {
		t.Errorf("HEAD metadata: %d body=%q", resp.StatusCode, body)
	}
}

// Self-hosted variants: the issuer follows MESH_MCP_OAUTH_ISSUER when set, and
// otherwise the origin of MESH_MCP_PUBLIC_URL, whatever prefix the MCP root has.
func TestOAuthResources_Configuration(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	for _, tc := range []struct {
		name, public, issuer      string
		wantFull, wantCore, wantI string
		wantMeta                  string
	}{
		{"stock", "https://mesh.example.com/mcp", "", "https://mesh.example.com/mcp", "https://mesh.example.com/mcp/core", "https://mesh.example.com",
			"https://mesh.example.com/.well-known/oauth-protected-resource/mcp"},
		{"trailing slash", "https://mesh.example.com/mcp/", "", "https://mesh.example.com/mcp", "https://mesh.example.com/mcp/core", "https://mesh.example.com",
			"https://mesh.example.com/.well-known/oauth-protected-resource/mcp"},
		{"bare origin", "https://mcp.example.com", "", "https://mcp.example.com", "https://mcp.example.com/core", "https://mcp.example.com",
			"https://mcp.example.com/.well-known/oauth-protected-resource"},
		{"explicit issuer", "https://mcp.example.com/mcp", "https://mesh.example.com/", "https://mcp.example.com/mcp", "https://mcp.example.com/mcp/core", "https://mesh.example.com",
			"https://mcp.example.com/.well-known/oauth-protected-resource/mcp"},
		{"port kept", "http://localhost:9000/mcp", "", "http://localhost:9000/mcp", "http://localhost:9000/mcp/core", "http://localhost:9000",
			"http://localhost:9000/.well-known/oauth-protected-resource/mcp"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOAuthResources(tc.public, tc.issuer)
			if got := o.resourceURL(r, profileFull).String(); got != tc.wantFull {
				t.Errorf("full resource = %q, want %q", got, tc.wantFull)
			}
			if got := o.resourceURL(r, profileCore).String(); got != tc.wantCore {
				t.Errorf("core resource = %q, want %q", got, tc.wantCore)
			}
			if got := o.issuerURL(r); got != tc.wantI {
				t.Errorf("issuer = %q, want %q", got, tc.wantI)
			}
			if got := o.metadataURL(r, profileFull); got != tc.wantMeta {
				t.Errorf("metadata URL = %q, want %q", got, tc.wantMeta)
			}
		})
	}
}

// With MESH_MCP_PUBLIC_URL unset (server reached directly) the URLs follow the
// origin the request came in on and the paths this binary really serves.
func TestOAuthResources_DerivedFromRequestWhenPublicURLUnset(t *testing.T) {
	o := newOAuthResources("", "")
	r := httptest.NewRequest(http.MethodPost, "http://localhost:8081/mcp", nil)
	if got := o.resourceURL(r, profileFull).String(); got != "http://localhost:8081/mcp" {
		t.Errorf("direct: %q", got)
	}
	if got := o.resourceURL(r, profileCore).String(); got != "http://localhost:8081/core" {
		t.Errorf("direct core: %q", got)
	}
	if got := o.challenge(r, profileFull); got != wantChallenge("http://localhost:8081/.well-known/oauth-protected-resource/mcp") {
		t.Errorf("direct challenge: %q", got)
	}

	r.Header.Set("X-Forwarded-Proto", "https")
	r.Header.Set("X-Forwarded-Host", "mesh.example.com")
	if got := o.resourceURL(r, profileFull).String(); got != "https://mesh.example.com/mcp" {
		t.Errorf("forwarded: %q", got)
	}
	r.Header.Set("X-Forwarded-Proto", "javascript")
	if got := o.resourceURL(r, profileFull).String(); !strings.HasPrefix(got, "http://") {
		t.Errorf("a forwarded scheme other than http/https must be ignored, got %q", got)
	}
}

// The REST client puts each credential in the header the Mesh API reads it
// from, on all three request builders (JSON, multipart upload, raw).
func TestRESTClient_CredentialHeader(t *testing.T) {
	var gotAuth, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotKey = r.Header.Get("Authorization"), r.Header.Get("X-Agent-Key")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	for _, tc := range []struct{ cred, wantAuth, wantKey string }{
		{"mot_abc", "Bearer mot_abc", ""},
		{"agk_ws_abc", "", "agk_ws_abc"},
	} {
		gotAuth, gotKey = "", ""
		if _, err := mcpserver.NewRESTClient(srv.URL, tc.cred).GetAgentMe(context.Background()); err != nil {
			t.Fatal(err)
		}
		if gotAuth != tc.wantAuth || gotKey != tc.wantKey {
			t.Errorf("%s: Authorization=%q X-Agent-Key=%q, want %q / %q", tc.cred, gotAuth, gotKey, tc.wantAuth, tc.wantKey)
		}
	}
}

func TestSafeKeyPrefix_DoesNotLeakTokenBytes(t *testing.T) {
	if got := safeKeyPrefix("mot_SECRETSECRETSECRET"); strings.Contains(got, "SECRET") {
		t.Errorf("OAuth token bytes reached the log prefix: %q", got)
	}
	if got := safeKeyPrefix("agk_myws_abcdefgh"); got != "agk_myws_abc" {
		t.Errorf("agent key prefix changed: %q", got)
	}
}

// With MESH_MCP_PUBLIC_URL unset the host comes from request headers. A value
// that is not a plain host[:port] must never reach the quoted challenge
// parameter or the JSON — '"' would let a caller add auth-params of their own.
func TestOAuthResources_ForwardedHostCannotInjectIntoChallenge(t *testing.T) {
	o := newOAuthResources("", "")
	for _, evil := range []string{`a.com",scope="x`, `a.com" injected="1`, "a.com\r\nX-Evil: 1", `a.com/../x`, `<script>`, ``} {
		r := httptest.NewRequest(http.MethodPost, "http://localhost:8081/mcp", nil)
		r.Header.Set("X-Forwarded-Host", evil)
		ch := o.challenge(r, profileFull)
		if ch != wantChallenge("http://localhost:8081/.well-known/oauth-protected-resource/mcp") {
			t.Errorf("X-Forwarded-Host %q leaked into the challenge: %q", evil, ch)
		}
	}
	// Well-formed forwarded hosts are still honoured.
	for _, ok := range []string{"mesh.example.com", "mesh.example.com:8443", "[2001:db8::1]:443", "10.0.0.5"} {
		r := httptest.NewRequest(http.MethodPost, "http://localhost:8081/mcp", nil)
		r.Header.Set("X-Forwarded-Host", ok)
		if got := o.resourceURL(r, profileFull).Host; got != ok {
			t.Errorf("X-Forwarded-Host %q rejected, host = %q", ok, got)
		}
	}
}

// A document derived from the requester's own origin must not sit in a shared
// cache; one built from configuration may.
func TestOAuthMetadata_CacheControlDependsOnWhereTheURLCameFrom(t *testing.T) {
	for _, tc := range []struct{ public, want string }{
		{testPublicURL, "public, max-age=300"},
		{"", "no-store"},
	} {
		h := newOAuthResources(tc.public, "").handler()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://localhost:8081/.well-known/oauth-protected-resource", nil))
		if got := rec.Header().Get("Cache-Control"); got != tc.want {
			t.Errorf("public URL %q: Cache-Control = %q, want %q", tc.public, got, tc.want)
		}
	}
}
