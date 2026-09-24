package main

import (
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// defaultAuthFailRPM is the per-IP budget for HTTP requests that need a
// fresh agent-key authentication (SSE connect, streamable /mcp and
// /mcp/core) — not for already-cached, previously-successful keys, which
// never reach this limiter at all (agentSessionCache.GetOrAuthenticate
// serves those from memory). Modeled on evc-mesh's own in-memory rate
// limiter (internal/middleware/ratelimit.go, the fallback RateLimit() uses
// when RedisClient is nil) — the closest existing precedent for "cheap
// public endpoint, brute-forceable, no Redis available": mesh-mcp is a
// standalone binary with no Redis client of its own. Override via
// MESH_MCP_AUTH_FAIL_RPM; 0 or negative disables limiting entirely.
const defaultAuthFailRPM = 20

// ipRateLimiter is an in-memory, fixed-window (1 minute), per-IP counter
// gating HTTP requests that need to authenticate a not-yet-cached agent
// key. It exists because the Streamable HTTP transport re-authenticates on
// every single request (main.go's streamOpts sets WithStateLess(true)):
// before this, a caller looping through bad or unknown agent keys forced
// one GET /api/v1/agents/me per request, 1:1, against Mesh API — SSE's
// agentSessionCache can't absorb that because it only ever caches SUCCESS
// (see GetOrAuthenticate's negTTL comment for the same problem approached
// from the caching side). Task #887de18a.
type ipRateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*ipBucket
	rpm     int
}

type ipBucket struct {
	windowStart int64 // time.Now().Unix() / 60
	count       int
}

// newIPRateLimiter builds a limiter and, when rpm > 0, starts a background
// goroutine that evicts buckets idle for more than 2 minutes — otherwise a
// stream of distinct source IPs (exactly the traffic this limiter exists to
// bound) would grow `buckets` without limit. rpm <= 0 means "disabled":
// allow() always returns true and nothing is tracked, so
// MESH_MCP_AUTH_FAIL_RPM=0 is a clean escape hatch without a redeploy.
func newIPRateLimiter(rpm int) *ipRateLimiter {
	l := &ipRateLimiter{buckets: make(map[string]*ipBucket), rpm: rpm}
	if rpm > 0 {
		go l.evictIdleLoop()
	}
	return l
}

// allow reports whether ip is still within its per-minute budget. Counts
// this call as one of the attempts — the caller must not call allow() and
// then skip the attempt it was checking for, or the budget undercounts.
func (l *ipRateLimiter) allow(ip string) bool {
	if l == nil || l.rpm <= 0 {
		return true
	}
	window := time.Now().Unix() / 60

	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	if !ok || b.windowStart != window {
		b = &ipBucket{windowStart: window}
		l.buckets[ip] = b
	}
	b.count++
	return b.count <= l.rpm
}

// over reports whether ip has already used up its budget for the current
// window, without counting a new attempt. It is the check for callers that
// charge the budget only after the fact (see requireCredential: an OAuth token
// costs budget when it is REJECTED, not when it is verified).
func (l *ipRateLimiter) over(ip string) bool {
	if l == nil || l.rpm <= 0 {
		return false
	}
	window := time.Now().Unix() / 60
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[ip]
	return ok && b.windowStart == window && b.count >= l.rpm
}

func (l *ipRateLimiter) evictIdleLoop() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Unix()/60 - 2
		l.mu.Lock()
		for ip, b := range l.buckets {
			if b.windowStart < cutoff {
				delete(l.buckets, ip)
			}
		}
		l.mu.Unlock()
	}
}

// clientIP resolves the real client address for a request that reached this
// process through Caddy on mesh-vm's loopback interface — r.RemoteAddr is
// always 127.0.0.1 there, never the actual caller. Caddy's own
// `trusted_proxies` config (deploy/caddy/mesh-vm.Caddyfile in evc-mesh)
// resolves the true client at the edge from hel01's X-Forwarded-For and
// forwards it downstream; this reads the FIRST entry (the original client,
// per the standard leftmost-is-origin convention for X-Forwarded-For — each
// hop appends itself, none of them prepend). Falls back to RemoteAddr for a
// direct/dev connection with no proxy in front (e.g. `go run . --transport
// sse` locally, or a unit test's httptest.Server).
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first := strings.TrimSpace(strings.SplitN(xff, ",", 2)[0]); first != "" {
			return first
		}
	}
	if xrip := strings.TrimSpace(r.Header.Get("X-Real-Ip")); xrip != "" {
		return xrip
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// checkAuthRateLimit rejects a request whose source IP has exhausted its
// per-minute authentication-attempt budget, writing 429 and returning
// false. A nil limiter (rate limiting disabled, or a test that doesn't care)
// always allows. The whole point is to reject BEFORE GetOrAuthenticate runs
// — callers must check this first and skip the auth call entirely on false.
func checkAuthRateLimit(w http.ResponseWriter, r *http.Request, limiter *ipRateLimiter) bool {
	if limiter == nil {
		return true
	}
	if limiter.allow(clientIP(r)) {
		return true
	}
	w.Header().Set("Retry-After", "60")
	http.Error(w, "Too many authentication attempts from this address, retry in a minute", http.StatusTooManyRequests)
	return false
}

// envIntOrDefault parses the environment variable name as an int, returning
// def if it is unset or not a valid integer. Unlike envDurationMinutes/
// envDurationSeconds below, this does NOT reject <= 0 — callers that treat
// 0/negative as a meaningful "disabled" value (e.g. MESH_MCP_AUTH_FAIL_RPM)
// need it to pass through.
func envIntOrDefault(name string, def int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return def
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return def
	}
	return n
}

// envDurationMinutes/envDurationSeconds resolve name to a Duration, falling
// back to def when unset, non-numeric, or <= 0. Both TTL env vars this repo
// defines (MESH_MCP_SESSION_CACHE_TTL_MIN, MESH_MCP_AUTH_FAIL_CACHE_SEC) are
// cache lifetimes: a 0 or negative value is ambiguous between "cache nothing"
// and "cache forever" depending on which zero-check reads it, so it's safer
// to just fall back to a known-good default than let a malformed env var
// pick one of those two very different behaviors implicitly.
func envDurationMinutes(name string, def time.Duration) time.Duration {
	n := envIntOrDefault(name, -1)
	if n <= 0 {
		return def
	}
	return time.Duration(n) * time.Minute
}

func envDurationSeconds(name string, def time.Duration) time.Duration {
	n := envIntOrDefault(name, -1)
	if n <= 0 {
		return def
	}
	return time.Duration(n) * time.Second
}
