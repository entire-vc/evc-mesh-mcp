package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	mcpserver "github.com/entire-vc/evc-mesh-mcp/internal/mcp"

	sdkserver "github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// authRetryAttempts is the number of GetAgentMe attempts on startup before
// giving up. authRetryBackoff is the delay before the first retry, doubling
// each subsequent attempt (1s, 2s for 3 attempts). authRetryBackoff is a
// var, not a const, so tests can shrink it instead of eating the real delay.
const authRetryAttempts = 3

var authRetryBackoff = time.Second

// authenticateWithRetry calls GetAgentMe, retrying only on transient
// failures (gateway 502/503/504, or a network error before any response
// came back). A one-shot 502 from the Mesh API gateway used to be
// indistinguishable from a bad credential: log.Fatalf killed the process
// before a single MCP tool was registered, leaving the whole session
// without Mesh tools until a manual restart (task #8afc7aba). A 401/403/404
// is a real auth/config problem and returns immediately — retrying those
// would just mask the failure behind a few seconds of pointless waiting.
func authenticateWithRetry(ctx context.Context, restClient *mcpserver.RESTClient) (map[string]any, error) {
	backoff := authRetryBackoff

	var lastErr error
	for attempt := 1; attempt <= authRetryAttempts; attempt++ {
		agentInfo, err := restClient.GetAgentMe(ctx)
		if err == nil {
			return agentInfo, nil
		}
		lastErr = err

		if !isTransientAuthError(err) || attempt == authRetryAttempts {
			return nil, err
		}

		log.Printf("Agent authentication attempt %d/%d failed (%v), retrying in %s...", attempt, authRetryAttempts, err, backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		backoff *= 2
	}
	return nil, lastErr
}

// isTransientAuthError reports whether err is worth retrying: a 502/503/504
// from the gateway, or a network-level error that happened before the
// server ever responded (connection refused/reset, DNS failure, timeout —
// none of these come back as an *mcpserver.APIError, since that type only
// wraps a response the server actually sent). Any other APIError status
// (401/403/404/...) is a real, non-transient failure and must not be
// retried.
func isTransientAuthError(err error) bool {
	var apiErr *mcpserver.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
			return true
		default:
			return false
		}
	}
	return true
}

// isCredentialRejection reports whether err means the Mesh API looked at the
// credential and refused it: any 4xx it answered with, except 408/429, which
// say "try again later", not "this credential is bad". A 5xx, a network error
// or an unusable response is not a verdict on the credential at all, and must
// neither be remembered against it nor turned into a "your token is invalid"
// answer that sends an OAuth client off to re-authorize.
func isCredentialRejection(err error) bool {
	var apiErr *mcpserver.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	code := apiErr.StatusCode
	return code >= 400 && code < 500 && code != http.StatusRequestTimeout && code != http.StatusTooManyRequests
}

// unconfiguredHint is returned by every tool call when stdio mode starts
// without an agent key.
const unconfiguredHint = "EVC Mesh is not configured yet. Set MESH_API_URL to your Mesh instance " +
	"(for example https://mesh.example.com) and MESH_AGENT_KEY to an agent key (agk_...) " +
	"created in that workspace under Settings → Agents, then restart the MCP server."

func main() {
	// All logging goes to stderr so that stdout is reserved for MCP JSON-RPC.
	log.SetOutput(os.Stderr)

	// Parse CLI flags.
	transportFlag := flag.String("transport", "", "Transport mode: stdio or sse (overrides MESH_MCP_TRANSPORT)")
	versionFlag := flag.Bool("version", false, "Print the build git SHA and exit")
	flag.Parse()

	// Print version and exit before anything that requires network/env setup
	// (MESH_AGENT_KEY, API connectivity) — origin of the installed binary must
	// be checkable offline. See task #1c602063.
	if *versionFlag {
		fmt.Println(mcpserver.BuildSHA)
		return
	}

	// 1. Determine transport mode from flag or env var.
	transport := "stdio"
	if envTransport := os.Getenv("MESH_MCP_TRANSPORT"); envTransport != "" {
		transport = strings.ToLower(envTransport)
	}
	if *transportFlag != "" {
		transport = strings.ToLower(*transportFlag)
	}
	if transport != "stdio" && transport != "sse" {
		log.Fatalf("Invalid transport %q: must be 'stdio' or 'sse'", transport)
	}

	// 2. Get REST API base URL.
	apiURL := os.Getenv("MESH_API_URL")
	if apiURL == "" {
		apiURL = "http://localhost:8005"
	}

	// 3. For stdio mode, MESH_AGENT_KEY identifies the agent. Without it the
	//    server still starts, in unconfigured mode: it initializes and lists
	//    its tools, and every tool call returns setup instructions. That is
	//    what MCP clients and catalogs need to inspect the server before the
	//    user has entered credentials.
	//    For SSE mode, agent keys are provided per-connection via HTTP headers/query params.
	agentKey := os.Getenv("MESH_AGENT_KEY")

	// Tool profile for stdio mode (SSE serves both profiles on separate paths).
	profile := strings.ToLower(strings.TrimSpace(os.Getenv("MESH_MCP_PROFILE")))
	if profile == "" {
		profile = mcpserver.ProfileFull
	}
	if profile != mcpserver.ProfileCore && profile != mcpserver.ProfileFull {
		log.Fatalf("Invalid MESH_MCP_PROFILE %q: must be 'core' or 'full'", profile)
	}

	// 4. Start transport.
	switch transport {
	case "stdio":
		if agentKey == "" {
			log.Println("MESH_AGENT_KEY is not set — starting unconfigured: tools are listed, calls return setup instructions")
			srv := mcpserver.NewServer(mcpserver.ServerConfig{
				Profile:   profile,
				SetupHint: unconfiguredHint,
			})
			if err := sdkserver.ServeStdio(srv.MCPServer()); err != nil {
				log.Fatalf("MCP server error: %v", err)
			}
			return
		}
		restClient := mcpserver.NewRESTClient(apiURL, agentKey)

		// Verify connectivity and get agent info.
		log.Printf("Connecting to Mesh API at %s...", apiURL)
		agentInfo, err := authenticateWithRetry(context.Background(), restClient)
		if err != nil {
			// Keep serving instead of exiting: the client still sees the tools,
			// and every call retries authentication and, until it succeeds,
			// returns the reason. A transient API outage at startup used to
			// leave the client with no Mesh tools for the whole session.
			log.Printf("Agent authentication failed: %v — serving tools, will retry on each call", err)
			srv := mcpserver.NewServer(mcpserver.ServerConfig{
				RESTClient: restClient,
				Profile:    profile,
				Authenticate: func(ctx context.Context) (*mcpserver.AgentSession, error) {
					info, err := restClient.GetAgentMe(ctx)
					if err != nil {
						return nil, err
					}
					id, _ := info["id"].(string)
					ws, _ := info["workspace_id"].(string)
					name, _ := info["name"].(string)
					typ, _ := info["agent_type"].(string)
					return buildSession(id, ws, name, typ)
				},
			})
			if err := sdkserver.ServeStdio(srv.MCPServer()); err != nil {
				log.Fatalf("MCP server error: %v", err)
			}
			return
		}

		agentID, _ := agentInfo["id"].(string)
		agentName, _ := agentInfo["name"].(string)
		agentType, _ := agentInfo["agent_type"].(string)
		workspaceID, _ := agentInfo["workspace_id"].(string)

		log.Printf("Authenticated as agent: %s (ID: %s, type: %s)", agentName, agentID, agentType)

		// Parse UUIDs.
		session, err := buildSession(agentID, workspaceID, agentName, agentType)
		if err != nil {
			log.Fatalf("Invalid agent data from API: %v", err)
		}

		cfg := mcpserver.ServerConfig{
			Session:    session,
			RESTClient: restClient,
			Profile:    profile,
		}

		srv := mcpserver.NewServer(cfg)
		log.Println("Starting MCP server on stdio transport...")
		if err := sdkserver.ServeStdio(srv.MCPServer()); err != nil {
			log.Fatalf("MCP server error: %v", err)
		}

	case "sse":
		// Public origin this instance is reachable at, if any. Resolved here
		// (rather than down where it was previously only used for the SSE
		// `endpoint` event) because agentSessionCache/serverRegistry below
		// also need it: their RESTClients dial apiURL directly (typically a
		// colocated loopback address), and without a forwarded-origin header
		// the backend echoes that loopback address back into every task/doc
		// URL it hands an SSE client (task #fe507dc9).
		publicURL := strings.TrimSpace(os.Getenv("MESH_MCP_PUBLIC_URL"))

		// SSE mode: per-connection authentication via HTTP headers/query params.
		// Create session cache that authenticates via REST API.
		sessionCache := newAgentSessionCache(apiURL, publicURL)

		// The Mesh API is the OAuth authorization server; this process is only
		// the resource server. See oauth_resource.go.
		oauth := newOAuthResources(publicURL, os.Getenv("MESH_MCP_OAUTH_ISSUER"))
		oauth.warnIfMisconfigured()

		// Per-IP budget for authentication attempts against a not-yet-cached
		// key, shared across /sse, /core/sse, /mcp and /mcp/core — see
		// ipRateLimiter's doc comment (ratelimit.go) for why this exists.
		authLimiter := newIPRateLimiter(envIntOrDefault("MESH_MCP_AUTH_FAIL_RPM", defaultAuthFailRPM))

		// For SSE mode, create a server without a static session.
		// Per-connection sessions are injected via the SSE context function.
		// The server's RESTClient will be overridden per-connection via context,
		// so we create a placeholder server — the actual REST client is per-connection.
		//
		// Since the mcp-go Server holds the RESTClient, we create a single server
		// that reads the session from context. The RESTClient in the server is
		// unused for SSE mode — handlers use the agent key from the session context
		// combined with the configured API URL.
		//
		// For SSE multi-agent: each connection's agent key is authenticated once,
		// and the session (including agent ID and workspace) is stored in context.
		// The shared RESTClient uses no default agent key (will be set per-request
		// via context-level agent key injection).
		//
		// Implementation note: the shared RESTClient will not work for multi-agent
		// SSE since it has a single agent key. Instead, we cache a RESTClient per
		// agent key and inject it into context via ContextWithRESTClient.
		//
		// We create the base server with an empty agent key; per-connection REST
		// clients are stored in the session cache and accessed via context.

		// We need a server with per-session REST clients for SSE mode.
		// Use a server registry: map agentKey -> *Server.
		srvRegistry := &serverRegistry{
			apiURL:    apiURL,
			publicURL: publicURL,
		}

		// Build a "router" server that dispatches to per-agent servers.
		// Since mcp-go SSE doesn't support per-connection server selection,
		// we create ONE shared server but override the RESTClient per request
		// by storing it in the context. The Server.getRESTClient() will read it.
		//
		// Simplification: use a single shared server with a per-request REST client
		// stored in context. Add a restClientKey to context for SSE mode.

		// Create a shared REST client (unused directly for tool calls — per-agent
		// clients are injected via context above; NewServer just needs one to
		// build a valid ServerConfig).
		sharedRestClient := mcpserver.NewRESTClient(apiURL, "")
		sharedRestClient.SetForwardedOrigin(publicURL)

		// Two servers, two profiles: full (default, backward compatible — every
		// existing client connects here) and core (a lighter tool set for
		// lightweight/embedded agents). This mirrors evc-mesh/cmd/mcp's
		// already-deployed dual-profile SSE setup, so mesh-vm can run this
		// binary instead of maintaining a second copy of the same MCP tools
		// (task #3bc9f59d).
		fullSrv := mcpserver.NewServer(mcpserver.ServerConfig{
			RESTClient: sharedRestClient,
			Profile:    mcpserver.ProfileFull,
		})
		coreSrv := mcpserver.NewServer(mcpserver.ServerConfig{
			RESTClient: sharedRestClient,
			Profile:    mcpserver.ProfileCore,
		})

		host := os.Getenv("MESH_MCP_HOST")
		if host == "" {
			host = "0.0.0.0"
		}
		port := os.Getenv("MESH_MCP_PORT")
		if port == "" {
			port = "8081"
		}
		addr := host + ":" + port

		// Shared SSE context function: injects the authenticated agent session
		// and per-agent REST client. Used by both profile servers — which
		// profile a connection lands on is decided by which mux route it hit,
		// not by anything in this function.
		sseContextFunc := func(ctx context.Context, r *http.Request) context.Context {
			key := extractAgentKeyFromRequest(r)
			if key == "" {
				log.Printf("SSE request without agent key from %s", r.RemoteAddr)
				return ctx
			}

			session, err := sessionCache.GetOrAuthenticate(ctx, key)
			if err != nil {
				log.Printf("SSE auth failed for key %s...: %v", safeKeyPrefix(key), err)
				return ctx
			}

			// Inject per-agent REST client and session into context.
			perAgentClient := srvRegistry.GetClient(key)
			ctx = mcpserver.ContextWithSession(ctx, session)
			ctx = mcpserver.ContextWithRESTClient(ctx, perAgentClient)
			return ctx
		}

		// sseOpts builds the option list for one profile's SSE server.
		// advertiseOptions decides which URL/path the `endpoint` event hands
		// back to the client — see its doc comment for why that is not simply
		// derived from the listen address. A fresh slice per call: appending to
		// one shared slice would let the two servers share a backing array.
		sseOpts := func(basePath string) []sdkserver.SSEOption {
			opts := []sdkserver.SSEOption{
				sdkserver.WithKeepAlive(true),
				sdkserver.WithSSEContextFunc(sseContextFunc),
			}
			return append(opts, advertiseOptions(publicURL, basePath)...)
		}

		fullSSE := sdkserver.NewSSEServer(fullSrv.MCPServer(), sseOpts("")...)
		coreSSE := sdkserver.NewSSEServer(coreSrv.MCPServer(), sseOpts(coreBasePath)...)

		// Start periodic flush of read-counter to disk (every 5 minutes).
		// Tracked on the full-profile server only: it is the backward-compatible
		// default every existing client already connects to, so this preserves
		// today's counter semantics unchanged. The core profile is a brand-new
		// endpoint on this binary and does not feed the same counter file —
		// giving it independent accounting (or merging the two) is follow-up
		// work, not something this change claims to have done.
		counterFile := readCounterFilePath()
		go func() {
			ticker := time.NewTicker(5 * time.Minute)
			defer ticker.Stop()
			for range ticker.C {
				if err := fullSrv.ReadCounter.WriteFile(counterFile); err != nil {
					log.Printf("read-counter flush error: %v", err)
				}
			}
		}()

		mux := http.NewServeMux()

		// Full profile (default, backward compatible): /sse and /message.
		mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
			key := extractAgentKeyFromRequest(r)
			if key == "" {
				http.Error(w, "Missing agent key: provide Authorization: Bearer <OAuth access token or agk_ key>, X-Agent-Key header, or ?agent_key query param", http.StatusUnauthorized)
				return
			}
			if refuseOAuthOnSSE(w, key) {
				return
			}
			if !sessionCache.peekValid(key) && !checkAuthRateLimit(w, r, authLimiter) {
				return
			}

			// Validate the key at connection time to fail fast.
			_, err := sessionCache.GetOrAuthenticate(r.Context(), key)
			if err != nil {
				log.Printf("SSE connection auth failed for key %s...: %v", safeKeyPrefix(key), err)
				http.Error(w, fmt.Sprintf("Authentication failed: %v", err), http.StatusForbidden)
				return
			}

			// Proxy to the real SSE handler.
			fullSSE.SSEHandler().ServeHTTP(w, r)
		})
		mux.Handle("/message", fullSSE.MessageHandler())

		// Core profile (lightweight tool set): /core/sse and /core/message.
		mux.HandleFunc(coreBasePath+"/sse", func(w http.ResponseWriter, r *http.Request) {
			key := extractAgentKeyFromRequest(r)
			if key == "" {
				http.Error(w, "Missing agent key: provide Authorization: Bearer <OAuth access token or agk_ key>, X-Agent-Key header, or ?agent_key query param", http.StatusUnauthorized)
				return
			}
			if refuseOAuthOnSSE(w, key) {
				return
			}
			if !sessionCache.peekValid(key) && !checkAuthRateLimit(w, r, authLimiter) {
				return
			}

			// Validate the key at connection time to fail fast.
			_, err := sessionCache.GetOrAuthenticate(r.Context(), key)
			if err != nil {
				log.Printf("SSE core connection auth failed for key %s...: %v", safeKeyPrefix(key), err)
				http.Error(w, fmt.Sprintf("Authentication failed: %v", err), http.StatusForbidden)
				return
			}

			coreSSE.SSEHandler().ServeHTTP(w, r)
		})
		mux.Handle(coreBasePath+"/message", coreSSE.MessageHandler())

		// Streamable HTTP transport (the current MCP spec transport), served
		// next to SSE by the same two profile servers, with the same per-request
		// agent-key authentication. Stateless: every request carries its key and
		// is authenticated on its own, so nothing ties a client to one process.
		//   full profile: /mcp   (public: https://<host>/mcp when the proxy
		//                         forwards that path unchanged)
		//   core profile: /core  (public: https://<host>/mcp/core behind the
		//                         existing /mcp/* prefix-stripping route)
		streamOpts := []sdkserver.StreamableHTTPOption{
			sdkserver.WithStateLess(true),
			sdkserver.WithHTTPContextFunc(sseContextFunc),
		}
		fullHTTP := sdkserver.NewStreamableHTTPServer(fullSrv.MCPServer(), streamOpts...)
		coreHTTP := sdkserver.NewStreamableHTTPServer(coreSrv.MCPServer(), streamOpts...)
		mux.Handle(streamablePath, requireCredential(sessionCache, authLimiter, "streamable", oauth, profileFull, fullHTTP))
		mux.Handle(coreBasePath, requireCredential(sessionCache, authLimiter, "streamable core", oauth, profileCore, coreHTTP))

		// OAuth protected-resource metadata (RFC 9728), one document per
		// profile. Unauthenticated by design: a client reads it to learn how
		// to authenticate at all.
		mux.Handle(protectedResourceWellKnown, oauth.handler())
		mux.Handle(protectedResourceWellKnown+"/", oauth.handler())

		// /read-counter — unauthenticated JSON snapshot for nightly cron / Grafana scrape.
		mux.HandleFunc("/read-counter", func(w http.ResponseWriter, r *http.Request) {
			snap := fullSrv.ReadCounter.Snapshot()
			data, err := json.Marshal(snap)
			if err != nil {
				http.Error(w, "marshal error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(data)
		})

		// Prometheus metrics — no auth required (IP-locked via Caddy on :9105).
		mux.Handle("/metrics", promhttp.Handler())

		log.Printf("Starting MCP SSE server on %s (multi-agent mode)", addr)
		log.Printf("  Full profile SSE endpoint: %s/sse", dialableURL(publicURL, host, port))
		log.Printf("  Core profile SSE endpoint: %s%s/sse", dialableURL(publicURL, host, port), coreBasePath)
		log.Printf("  Full profile streamable HTTP: %s%s", dialableURL(publicURL, host, port), streamablePath)
		log.Printf("  Core profile streamable HTTP: %s%s", dialableURL(publicURL, host, port), coreBasePath)
		if publicURL == "" {
			log.Printf("  Message endpoint is advertised relative to the URL each client connects to.")
			log.Printf("  Set MESH_MCP_PUBLIC_URL if your clients require an absolute endpoint URL.")
		} else {
			log.Printf("  Message endpoint is advertised under MESH_MCP_PUBLIC_URL=%s", publicURL)
		}
		log.Printf("  Read counter:     %s/read-counter", dialableURL(publicURL, host, port))
		log.Printf("  Metrics:          %s/metrics", dialableURL(publicURL, host, port))
		log.Printf("  Counter file:     %s", counterFile)
		log.Printf("  Auth: Authorization: Bearer <OAuth access token (mot_...) or agk_ key>, X-Agent-Key, or ?agent_key=agk_... (SSE connect only)")
		log.Printf("  OAuth resource metadata: %s%s", dialableURL(publicURL, host, port), protectedResourceWellKnown)

		httpServer := &http.Server{
			Addr:    addr,
			Handler: mux,
		}
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("MCP SSE server error: %v", err)
		}
	}
}

// coreBasePath is the path prefix the lightweight (core-profile) SSE endpoints
// are mounted at. It has to be known to both the mux and the URL the server
// advertises to clients, so it lives in one place.
const coreBasePath = "/core"

// streamablePath is where the full-profile Streamable HTTP endpoint is mounted.
const streamablePath = "/mcp"

// requireAgentKey rejects a request that carries no agent key (401), one
// whose source IP has exhausted its authentication-attempt budget (429,
// without even asking Mesh API — see checkAuthRateLimit), or a key the Mesh
// API does not accept (403), before it reaches an MCP transport. limiter may
// be nil to disable the rate-limit check (e.g. in a test that isn't
// exercising it).
//
// The Streamable HTTP endpoints are stateless, so the key travels with every
// request. They accept it from headers only: a key in the query string would
// be repeated in every request URL, where proxies and tools tend to log it.
// (The legacy SSE connect step keeps the query fallback for EventSource.)
func requireAgentKey(cache *agentSessionCache, limiter *ipRateLimiter, what string, next http.Handler) http.Handler {
	return requireCredential(cache, limiter, what, nil, profileFull, next)
}

// requireCredential is requireAgentKey with OAuth support. With oauth set, a
// request that carries no credential, or an OAuth access token the Mesh API
// rejects, gets 401 and a WWW-Authenticate header pointing at the profile's
// protected-resource metadata — the only signal an OAuth client has that it
// should start (or restart) the authorization flow. A rejected agent key
// keeps its 403. A Mesh API that could not be reached is 503 for an OAuth
// token, not 401: 401 tells the client its token is dead and sends it off to
// refresh, which a gateway blip must not do to a valid one.
func requireCredential(cache *agentSessionCache, limiter *ipRateLimiter, what string, oauth *oauthResources, profile mcpProfile, next http.Handler) http.Handler {
	challenge := func(w http.ResponseWriter, r *http.Request) {
		if oauth != nil {
			w.Header().Set("WWW-Authenticate", oauth.challenge(r, profile))
		} else {
			w.Header().Set("WWW-Authenticate", `Bearer realm="evc-mesh"`)
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("agent_key") {
			http.Error(w, "Pass the agent key in a header (Authorization: Bearer agk_... or X-Agent-Key), not in the URL", http.StatusBadRequest)
			return
		}
		key := extractAgentKeyFromRequest(r)
		if key == "" {
			challenge(w, r)
			http.Error(w, "Missing credentials: provide Authorization: Bearer <OAuth access token or agk_ key> or X-Agent-Key header", http.StatusUnauthorized)
			return
		}
		viaOAuth := oauth != nil && isOAuthToken(key)
		if viaOAuth {
			// An OAuth token is re-verified about once a minute for as long as
			// it is in use (short cache, see defaultOAuthCacheTTL), so charging
			// the per-IP budget for every verification would let ordinary
			// traffic from a shared egress address starve itself. The budget
			// exists to bound guessing, so it is charged for rejections only —
			// below — and merely consulted here.
			if !cache.peekValid(key) && limiter.over(clientIP(r)) {
				w.Header().Set("Retry-After", "60")
				http.Error(w, "Too many authentication attempts from this address, retry in a minute", http.StatusTooManyRequests)
				return
			}
		} else if !cache.peekValid(key) && !checkAuthRateLimit(w, r, limiter) {
			return
		}
		if _, err := cache.GetOrAuthenticate(r.Context(), key); err != nil {
			log.Printf("%s auth failed for key %s...: %v", what, safeKeyPrefix(key), err)
			if viaOAuth {
				if !isCredentialRejection(err) {
					w.Header().Set("Retry-After", "5")
					http.Error(w, "Authentication temporarily unavailable", http.StatusServiceUnavailable)
					return
				}
				limiter.allow(clientIP(r)) // charge the rejection
				challenge(w, r)
				http.Error(w, "Invalid or expired access token", http.StatusUnauthorized)
				return
			}
			http.Error(w, fmt.Sprintf("Authentication failed: %v", err), http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// refuseOAuthOnSSE rejects an OAuth access token presented at an SSE connect.
// The OAuth flow is advertised for the Streamable HTTP endpoints only: SSE
// keeps a long-lived stream, and a token that expires mid-stream would turn
// every later /message POST into a tool error instead of the 401 an OAuth
// client can act on.
func refuseOAuthOnSSE(w http.ResponseWriter, key string) bool {
	if !isOAuthToken(key) {
		return false
	}
	http.Error(w, "OAuth access tokens are supported on the streamable HTTP endpoints only; use an agent key for SSE", http.StatusUnauthorized)
	return true
}

// advertiseOptions configures which URL the SSE server hands a client in its
// `endpoint` event — the address the client will POST every subsequent JSON-RPC
// message to.
//
// This is deliberately not derived from the listen address. The server listens
// on 0.0.0.0 so that a published container port works at all, but 0.0.0.0 is a
// wildcard bind, not a destination: a client told to POST to
// http://0.0.0.0:8081/message has been handed an address it cannot dial. That
// is what made a correctly published MCP port look unreachable from outside the
// host while working fine from inside it.
//
// With MESH_MCP_PUBLIC_URL unset we advertise a relative path ("/message?...").
// Every MCP client resolves it against the URL it connected to, so the answer is
// automatically correct for localhost, for a published container port, and for
// any reverse proxy — none of which the server can guess on its own.
//
// Set MESH_MCP_PUBLIC_URL (e.g. https://mesh.example.com/mcp) to advertise
// absolute URLs instead, for clients that reject relative endpoints or for a
// proxy that rewrites the path.
func advertiseOptions(publicURL, basePath string) []sdkserver.SSEOption {
	var opts []sdkserver.SSEOption
	if basePath != "" {
		opts = append(opts, sdkserver.WithStaticBasePath(basePath))
	}
	if publicURL == "" {
		return append(opts, sdkserver.WithUseFullURLForMessageEndpoint(false))
	}
	return append(opts, sdkserver.WithBaseURL(strings.TrimSuffix(publicURL, "/")))
}

// dialableURL returns a URL an operator can paste into a client, for logging
// only. A wildcard listen host is reported as localhost, because that is the
// address that actually works from the machine reading the log.
func dialableURL(publicURL, host, port string) string {
	if publicURL != "" {
		return strings.TrimSuffix(publicURL, "/")
	}
	switch host {
	case "0.0.0.0", "::", "[::]", "":
		host = "localhost"
	}
	return fmt.Sprintf("http://%s:%s", host, port)
}

// readCounterFilePath returns the path for the read-counter JSON file.
// Override via MESH_MCP_COUNTER_FILE env var; defaults to ~/.openclaw/metrics/mcp-read-counter.json.
func readCounterFilePath() string {
	if p := os.Getenv("MESH_MCP_COUNTER_FILE"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "/tmp/mcp-read-counter.json"
	}
	return filepath.Join(home, ".openclaw", "metrics", "mcp-read-counter.json")
}

// buildSession creates an AgentSession from API response strings.
func buildSession(agentID, workspaceID, agentName, agentType string) (*mcpserver.AgentSession, error) {
	session, err := mcpserver.NewAgentSession(agentID, workspaceID, agentName, agentType)
	if err != nil {
		return nil, err
	}
	return &session, nil
}

// isOAuthToken reports whether credential is an OAuth access token (mot_...)
// rather than an agent key (agk_...). OAuth tokens are accepted only in the
// Authorization: Bearer header — never X-Agent-Key or the query string, the
// same places the Mesh API itself reads them from.
func isOAuthToken(credential string) bool {
	return strings.HasPrefix(credential, mcpserver.OAuthAccessTokenPrefix)
}

// extractAgentKeyFromRequest extracts the credential identifying the calling
// agent from an HTTP request: an agent API key (agk_...) from Authorization:
// Bearer, X-Agent-Key or (SSE connect only) ?agent_key=, or an OAuth access
// token (mot_...) from Authorization: Bearer.
func extractAgentKeyFromRequest(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		const bearerPrefix = "Bearer "
		// The auth scheme is case-insensitive (RFC 9110 §11.1); generic OAuth
		// libraries are free to send "bearer".
		if len(auth) > len(bearerPrefix) && strings.EqualFold(auth[:len(bearerPrefix)], bearerPrefix) {
			token := strings.TrimSpace(auth[len(bearerPrefix):])
			if strings.HasPrefix(token, "agk_") || isOAuthToken(token) {
				return token
			}
		}
	}
	if key := r.Header.Get("X-Agent-Key"); key != "" && strings.HasPrefix(key, "agk_") {
		return key
	}
	if key := r.URL.Query().Get("agent_key"); key != "" && strings.HasPrefix(key, "agk_") {
		return key
	}
	return ""
}

// safeKeyPrefix returns a safe prefix of the key for logging. An agent key's
// leading bytes are a workspace slug and a type marker; an OAuth token has no
// such structure — every byte after "mot_" is secret — so only its marker is
// ever logged.
func safeKeyPrefix(key string) string {
	if isOAuthToken(key) {
		return mcpserver.OAuthAccessTokenPrefix
	}
	if len(key) > 12 {
		return key[:12]
	}
	return key
}

// defaultSessionCacheTTL bounds how long a successful authentication is
// trusted before the key is re-checked against Mesh API. Without this, a
// revoked agent key kept authenticating successfully here indefinitely —
// GetOrAuthenticate never re-verified a key it had already accepted once,
// so revocation only took effect on the next process restart. Override via
// MESH_MCP_SESSION_CACHE_TTL_MIN. Task #887de18a.
const defaultSessionCacheTTL = 15 * time.Minute

// defaultAuthFailCacheTTL bounds how long a FAILED authentication is
// remembered, so a client (or attacker) retrying the exact same bad key
// doesn't force a fresh GET /api/v1/agents/me on every single request.
// Streamable HTTP (/mcp, /mcp/core) re-authenticates on every request
// (WithStateLess(true)), so before this a bad-key loop turned 1:1 into load
// on Mesh API. Override via MESH_MCP_AUTH_FAIL_CACHE_SEC. Task #887de18a.
const defaultAuthFailCacheTTL = 30 * time.Second

// defaultOAuthCacheTTL bounds how long a successfully verified OAuth access
// token is trusted. Much shorter than defaultSessionCacheTTL on purpose: an
// agent key is revoked by an administrator and lag is tolerable, but an OAuth
// grant is revoked by the user from a settings page that promises the
// connection stops, and an access token also expires by itself (about an
// hour), so a long cache would keep honouring a dead one. Override via
// MESH_MCP_OAUTH_CACHE_TTL_SEC.
const defaultOAuthCacheTTL = 60 * time.Second

// agentSessionCache caches authenticated agent sessions by agent key.
//
// Two independent TTLs bound how long an entry is trusted without asking
// Mesh API again — see defaultSessionCacheTTL and defaultAuthFailCacheTTL
// for what each defends against. Zero-value ttl/negTTL (e.g. a bare
// &agentSessionCache{apiURL: ...} in a test) fall back to those defaults via
// sessionTTL()/failTTL() rather than caching nothing or everything forever;
// use newAgentSessionCache to build one from the environment instead.
type agentSessionCache struct {
	mu        sync.RWMutex
	cache     map[string]sessionCacheEntry
	negCache  map[string]negCacheEntry
	apiURL    string
	publicURL string
	ttl       time.Duration // positive entry TTL; <=0 = use defaultSessionCacheTTL
	oauthTTL  time.Duration // positive TTL for OAuth access tokens; <=0 = use defaultOAuthCacheTTL
	negTTL    time.Duration // negative entry TTL; <=0 = use defaultAuthFailCacheTTL
}

type sessionCacheEntry struct {
	session   *mcpserver.AgentSession
	expiresAt time.Time
}

type negCacheEntry struct {
	err       error
	expiresAt time.Time
}

// newAgentSessionCache builds a cache with TTLs read from the environment
// (or their defaults) and starts a background goroutine that evicts expired
// entries. Without that eviction, a stream of distinct never-valid keys —
// exactly the traffic negCache exists to absorb — would grow it without
// bound between successful lookups (which are the only other place entries
// are removed, via the delete(c.negCache, key) in GetOrAuthenticate).
func newAgentSessionCache(apiURL, publicURL string) *agentSessionCache {
	c := &agentSessionCache{
		apiURL:    apiURL,
		publicURL: publicURL,
		ttl:       envDurationMinutes("MESH_MCP_SESSION_CACHE_TTL_MIN", defaultSessionCacheTTL),
		oauthTTL:  envDurationSeconds("MESH_MCP_OAUTH_CACHE_TTL_SEC", defaultOAuthCacheTTL),
		negTTL:    envDurationSeconds("MESH_MCP_AUTH_FAIL_CACHE_SEC", defaultAuthFailCacheTTL),
	}
	go c.evictExpiredLoop()
	return c
}

func (c *agentSessionCache) sessionTTL() time.Duration {
	if c.ttl <= 0 {
		return defaultSessionCacheTTL
	}
	return c.ttl
}

// entryTTL is how long a fresh successful verification of key is trusted.
func (c *agentSessionCache) entryTTL(key string) time.Duration {
	if !isOAuthToken(key) {
		return c.sessionTTL()
	}
	ttl := c.oauthTTL
	if ttl <= 0 {
		ttl = defaultOAuthCacheTTL
	}
	if base := c.sessionTTL(); base < ttl {
		ttl = base
	}
	return ttl
}

func (c *agentSessionCache) failTTL() time.Duration {
	if c.negTTL <= 0 {
		return defaultAuthFailCacheTTL
	}
	return c.negTTL
}

func (c *agentSessionCache) evictExpiredLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		now := time.Now()
		c.mu.Lock()
		for k, e := range c.cache {
			if now.After(e.expiresAt) {
				delete(c.cache, k)
			}
		}
		for k, e := range c.negCache {
			if now.After(e.expiresAt) {
				delete(c.negCache, k)
			}
		}
		c.mu.Unlock()
	}
}

// peekValid reports whether key already has a live, unexpired entry in the
// positive cache — no network access, no side effects. Callers use it to
// decide whether a request needs to spend per-IP rate-limit budget at all:
// a request presenting an already-known-good key must not compete for the
// same budget that exists to bound authentication attempts against a
// NOT-yet-cached key (see checkAuthRateLimit's call sites in main.go).
// Without this, the Streamable HTTP transport — which re-authenticates on
// every single tool call by design — would spend budget on ordinary,
// already-authenticated traffic and could spuriously 429 a well-behaved
// client well under any actual abuse.
func (c *agentSessionCache) peekValid(key string) bool {
	now := time.Now()
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.cache == nil {
		return false
	}
	entry, ok := c.cache[key]
	return ok && now.Before(entry.expiresAt)
}

// GetOrAuthenticate returns a cached session or authenticates and caches it.
func (c *agentSessionCache) GetOrAuthenticate(ctx context.Context, key string) (*mcpserver.AgentSession, error) {
	now := time.Now()

	c.mu.RLock()
	if c.cache != nil {
		if entry, ok := c.cache[key]; ok && now.Before(entry.expiresAt) {
			c.mu.RUnlock()
			return entry.session, nil
		}
	}
	if c.negCache != nil {
		if entry, ok := c.negCache[key]; ok && now.Before(entry.expiresAt) {
			c.mu.RUnlock()
			return nil, entry.err
		}
	}
	c.mu.RUnlock()

	// Authenticate via REST API.
	client := mcpserver.NewRESTClient(c.apiURL, key)
	client.SetForwardedOrigin(c.publicURL)
	agentInfo, err := client.GetAgentMe(ctx)
	if err != nil {
		authErr := fmt.Errorf("authentication failed: %w", err)
		// Only a real rejection (a 4xx verdict — see isCredentialRejection) is
		// worth remembering as a negative entry. A transient gateway blip
		// (502/503/504, or a network error before any response), an API 500,
		// or its own rate limiter answering 429 would otherwise get a valid
		// credential stuck failing for up to failTTL() after Mesh API
		// recovers.
		if isCredentialRejection(err) {
			c.mu.Lock()
			if c.negCache == nil {
				c.negCache = make(map[string]negCacheEntry)
			}
			c.negCache[key] = negCacheEntry{err: authErr, expiresAt: time.Now().Add(c.failTTL())}
			c.mu.Unlock()
		}
		return nil, authErr
	}

	agentID, _ := agentInfo["id"].(string)
	workspaceID, _ := agentInfo["workspace_id"].(string)
	agentName, _ := agentInfo["name"].(string)
	agentType, _ := agentInfo["agent_type"].(string)

	session, err := mcpserver.NewAgentSession(agentID, workspaceID, agentName, agentType)
	if err != nil {
		return nil, fmt.Errorf("invalid agent data: %w", err)
	}

	c.mu.Lock()
	if c.cache == nil {
		c.cache = make(map[string]sessionCacheEntry)
	}
	c.cache[key] = sessionCacheEntry{session: &session, expiresAt: time.Now().Add(c.entryTTL(key))}
	if c.negCache != nil {
		delete(c.negCache, key)
	}
	c.mu.Unlock()

	log.Printf("SSE: authenticated agent %s (ID: %s)", agentName, agentID)
	return &session, nil
}

// serverRegistry caches per-agent REST clients keyed by agent API key.
type serverRegistry struct {
	mu        sync.RWMutex
	cache     map[string]*mcpserver.RESTClient
	apiURL    string
	publicURL string
}

// GetClient returns a cached REST client for the given agent key, creating one if needed.
func (r *serverRegistry) GetClient(key string) *mcpserver.RESTClient {
	if isOAuthToken(key) {
		// Not cached: OAuth tokens rotate about hourly per user, so a cache
		// keyed by token would grow for the life of the process and keep every
		// past token in memory. A client is cheap (it shares the default
		// transport), and the session behind it is cached separately.
		client := mcpserver.NewRESTClient(r.apiURL, key)
		client.SetForwardedOrigin(r.publicURL)
		return client
	}
	r.mu.RLock()
	if r.cache != nil {
		if client, ok := r.cache[key]; ok {
			r.mu.RUnlock()
			return client
		}
	}
	r.mu.RUnlock()

	client := mcpserver.NewRESTClient(r.apiURL, key)
	client.SetForwardedOrigin(r.publicURL)

	r.mu.Lock()
	if r.cache == nil {
		r.cache = make(map[string]*mcpserver.RESTClient)
	}
	r.cache[key] = client
	r.mu.Unlock()

	return client
}
