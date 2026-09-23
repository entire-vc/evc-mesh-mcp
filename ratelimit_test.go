package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// countingMeshAPI accepts exactly one agent key on /api/v1/agents/me and
// counts every call it receives, so a test can assert how many times the
// cache actually reached the network — the whole point of task #887de18a.
func countingMeshAPI(t *testing.T, goodKey string, calls *int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(calls, 1)
		if r.URL.Path != "/api/v1/agents/me" || r.Header.Get("X-Agent-Key") != goodKey {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           "11111111-1111-1111-1111-111111111111",
			"workspace_id": "22222222-2222-2222-2222-222222222222",
			"name":         "probe",
			"agent_type":   "agent",
		})
	}))
}

// ---------------------------------------------------------------------------
// Negative cache — RED CONTROL first (CLAUDE-workflow.md §0x): before the
// fix, N bad-key requests against the same key produced N calls to
// /api/v1/agents/me. Confirmed below with negTTL=0 (fallback default, still
// active) vs a deliberately-disabled cache would both show N==attempts; the
// green case (default TTL active) must show calls==1. Task #887de18a.
// ---------------------------------------------------------------------------

func TestAgentSessionCache_NegativeCacheAbsorbsRepeatedBadKey(t *testing.T) {
	var calls int64
	api := countingMeshAPI(t, "agk_good", &calls)
	defer api.Close()

	cache := &agentSessionCache{apiURL: api.URL, negTTL: time.Minute}

	const attempts = 5
	for i := 0; i < attempts; i++ {
		if _, err := cache.GetOrAuthenticate(context.Background(), "agk_bad"); err == nil {
			t.Fatalf("attempt %d: expected error for bad key, got nil", i)
		}
	}

	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Errorf("%d attempts with the same bad key produced %d calls to /agents/me, want 1 (negative cache should have absorbed the other %d)", attempts, got, attempts-1)
	}
}

// A DIFFERENT bad key each time must still be rejected on its own — the
// negative cache must not accidentally treat "any bad key" as equivalent.
func TestAgentSessionCache_NegativeCacheIsPerKey(t *testing.T) {
	var calls int64
	api := countingMeshAPI(t, "agk_good", &calls)
	defer api.Close()

	cache := &agentSessionCache{apiURL: api.URL, negTTL: time.Minute}

	for _, key := range []string{"agk_bad1", "agk_bad2", "agk_bad3"} {
		if _, err := cache.GetOrAuthenticate(context.Background(), key); err == nil {
			t.Fatalf("key %s: expected error, got nil", key)
		}
	}

	if got := atomic.LoadInt64(&calls); got != 3 {
		t.Errorf("3 distinct bad keys produced %d calls to /agents/me, want 3 (one per distinct key)", got)
	}
}

// A transient gateway error (502/503/504) must NOT be cached as a rejection
// — otherwise a valid key would answer 403 for up to negTTL after Mesh API
// recovers from a blip that had nothing to do with the key itself.
func TestAgentSessionCache_TransientErrorNotNegativelyCached(t *testing.T) {
	var mode atomic.Int32 // 0 = 502, 1 = success
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if mode.Load() == 0 {
			http.Error(w, "bad gateway", http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           "11111111-1111-1111-1111-111111111111",
			"workspace_id": "22222222-2222-2222-2222-222222222222",
			"name":         "probe",
			"agent_type":   "agent",
		})
	}))
	defer srv.Close()

	cache := &agentSessionCache{apiURL: srv.URL, negTTL: time.Minute}

	if _, err := cache.GetOrAuthenticate(context.Background(), "agk_ok"); err == nil {
		t.Fatal("expected error on 502, got nil")
	}
	mode.Store(1)
	// If the 502 had been negatively cached, this would still fail even
	// though the key is now good and Mesh API is answering normally.
	if _, err := cache.GetOrAuthenticate(context.Background(), "agk_ok"); err != nil {
		t.Errorf("key rejected after transient 502 recovered: %v (a transient error must not poison the negative cache)", err)
	}
}

// ---------------------------------------------------------------------------
// Positive-entry TTL — a revoked key must stop working within ttl, without a
// restart. RED CONTROL: with ttl unset/zero-ish behavior removed (pre-fix),
// this would never re-check and the second call would keep succeeding
// forever. Task #887de18a.
// ---------------------------------------------------------------------------

func TestAgentSessionCache_TTLExpiryRevokesWithoutRestart(t *testing.T) {
	var allowed atomic.Bool
	allowed.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !allowed.Load() {
			http.Error(w, `{"error":"revoked"}`, http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":           "11111111-1111-1111-1111-111111111111",
			"workspace_id": "22222222-2222-2222-2222-222222222222",
			"name":         "probe",
			"agent_type":   "agent",
		})
	}))
	defer srv.Close()

	// A tiny TTL so the test doesn't have to sleep for the real default.
	cache := &agentSessionCache{apiURL: srv.URL, ttl: 10 * time.Millisecond}

	if _, err := cache.GetOrAuthenticate(context.Background(), "agk_key"); err != nil {
		t.Fatalf("first auth: %v", err)
	}
	// Revoke the key server-side, but the process keeps running — no restart.
	allowed.Store(false)

	// Still within TTL: served from the (now stale) positive cache.
	if _, err := cache.GetOrAuthenticate(context.Background(), "agk_key"); err != nil {
		t.Errorf("within TTL: expected cached success, got error: %v", err)
	}

	time.Sleep(20 * time.Millisecond) // past the 10ms TTL

	if _, err := cache.GetOrAuthenticate(context.Background(), "agk_key"); err == nil {
		t.Error("after TTL expiry: revoked key still authenticated — cache never re-checked it")
	}
}

// A bare &agentSessionCache{} (as existing tests in this package construct
// it) must not panic or behave as "cache nothing"/"cache forever" — TTLs
// fall back to the package defaults.
func TestAgentSessionCache_ZeroValueTTLsFallBackToDefaults(t *testing.T) {
	cache := &agentSessionCache{}
	if got := cache.sessionTTL(); got != defaultSessionCacheTTL {
		t.Errorf("sessionTTL() with zero-value ttl = %v, want default %v", got, defaultSessionCacheTTL)
	}
	if got := cache.failTTL(); got != defaultAuthFailCacheTTL {
		t.Errorf("failTTL() with zero-value negTTL = %v, want default %v", got, defaultAuthFailCacheTTL)
	}
}

// ---------------------------------------------------------------------------
// Per-IP rate limiting on /mcp and /mcp/core (streamable, via requireAgentKey)
// ---------------------------------------------------------------------------

func TestIPRateLimiter_BlocksOverBudget(t *testing.T) {
	l := newIPRateLimiter(3)
	for i := 0; i < 3; i++ {
		if !l.allow("203.0.113.1") {
			t.Fatalf("attempt %d within budget was blocked", i+1)
		}
	}
	if l.allow("203.0.113.1") {
		t.Error("4th attempt within the same window should have been blocked")
	}
	// A different IP has its own independent budget.
	if !l.allow("203.0.113.2") {
		t.Error("a different IP was blocked by another IP's exhausted budget")
	}
}

func TestIPRateLimiter_ZeroRPMDisables(t *testing.T) {
	l := newIPRateLimiter(0)
	for i := 0; i < 100; i++ {
		if !l.allow("203.0.113.1") {
			t.Fatalf("rpm=0 should disable limiting, but attempt %d was blocked", i+1)
		}
	}
}

// RED CONTROL then GREEN: without checkAuthRateLimit gating the call, N
// distinct-bad-key requests from one IP would each reach /agents/me (the
// exact "нагрузку на Mesh API стало проще раскачать" scenario from the task
// description, task #887de18a). With the limiter, requests beyond the
// per-minute budget are rejected 429 and never call GetOrAuthenticate.
func TestRequireAgentKey_RateLimitsPerIP(t *testing.T) {
	var calls int64
	api := countingMeshAPI(t, "agk_good", &calls)
	defer api.Close()

	cache := &agentSessionCache{apiURL: api.URL, negTTL: time.Millisecond} // don't let neg-cache mask the rate limiter
	// Budget covers the priming call PLUS 3 bad-key attempts (4 total) — see
	// the comment at each step below for how it's spent.
	limiter := newIPRateLimiter(4)

	handler := requireAgentKey(cache, limiter, "test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts := httptest.NewServer(handler)
	defer ts.Close()

	get := func(key string) int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
		req.Header.Set("X-Agent-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// Cache the good key FIRST, before it's known good it still competes
	// for budget like any other not-yet-cached key (spends 1/4) — this is
	// the case the fix (peekValid) exists for AFTER this point: ordinary
	// Streamable HTTP traffic re-authenticates on every request with an
	// already-known-good key, and from here on must never compete for the
	// same budget that bounds attempts against a NOT-yet-cached key.
	if code := get("agk_good"); code != http.StatusOK {
		t.Fatalf("priming call with the good key: got %d, want 200", code)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("calls to /agents/me after priming = %d, want 1", got)
	}

	// 3 distinct bad (not-yet-cached) keys spend the remaining 3/4 budget,
	// each reaching /agents/me and getting their real 403.
	for i, key := range []string{"agk_bad1", "agk_bad2", "agk_bad3"} {
		if code := get(key); code != http.StatusForbidden {
			t.Fatalf("attempt %d (%s): got %d, want 403", i+1, key, code)
		}
	}
	if got := atomic.LoadInt64(&calls); got != 4 { // 1 priming + 3 bad-key attempts
		t.Fatalf("calls to /agents/me after 1 priming + 3 budgeted bad-key attempts = %d, want 4", got)
	}

	// Budget is now fully spent (4/4). A 4th bad key must be rejected 429
	// WITHOUT reaching /agents/me.
	if code := get("agk_bad4"); code != http.StatusTooManyRequests {
		t.Errorf("bad-key attempt over budget: got %d, want 429", code)
	}
	if got := atomic.LoadInt64(&calls); got != 4 {
		t.Errorf("calls to /agents/me after the over-budget attempt = %d, want still 4 (429 must short-circuit before authenticating)", got)
	}

	// The per-IP budget is now fully spent on bad keys. The already-cached
	// good key must STILL work — it never competes for that budget at all,
	// because peekValid() short-circuits checkAuthRateLimit for a request
	// presenting a key the cache already knows is good.
	if code := get("agk_good"); code != http.StatusOK {
		t.Errorf("already-cached good key after budget exhausted by bad keys: got %d, want 200 (a valid cached key must not be rate-limited)", code)
	}
	if got := atomic.LoadInt64(&calls); got != 4 {
		t.Errorf("calls to /agents/me after the cached-good-key request = %d, want still 4 (a cache hit must never touch /agents/me)", got)
	}
}

// A key that is NOT yet cached still spends rate-limit budget even if it
// will turn out to be valid — the budget is spent on "is this key known
// good", not on the outcome. Only after the first success is it exempt (see
// TestRequireAgentKey_RateLimitsPerIP above). This matches evc-mesh's own
// /auth/login limiter, which also gates the attempt before the outcome is
// known.
func TestRequireAgentKey_UnknownGoodKeyStillSpendsBudgetBeforeFirstSuccess(t *testing.T) {
	var calls int64
	api := countingMeshAPI(t, "agk_good", &calls)
	defer api.Close()

	cache := &agentSessionCache{apiURL: api.URL}
	limiter := newIPRateLimiter(1)

	handler := requireAgentKey(cache, limiter, "test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	ts := httptest.NewServer(handler)
	defer ts.Close()

	get := func(key string) int {
		req, _ := http.NewRequest(http.MethodGet, ts.URL, nil)
		req.Header.Set("X-Agent-Key", key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// Budget of 1, spent by a bad key first.
	if code := get("agk_bad"); code != http.StatusForbidden {
		t.Fatalf("bad key: got %d, want 403", code)
	}
	// The good key has never been seen/cached yet, so it still competes for
	// the (now exhausted) budget.
	if code := get("agk_good"); code != http.StatusTooManyRequests {
		t.Errorf("never-before-seen good key over budget: got %d, want 429", code)
	}
}

func TestClientIP_PrefersForwardedFor(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 127.0.0.1")
	if got := clientIP(req); got != "203.0.113.9" {
		t.Errorf("clientIP() = %q, want the original client 203.0.113.9 (leftmost XFF entry)", got)
	}
}

func TestClientIP_FallsBackToRemoteAddr(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.7:54321"
	if got := clientIP(req); got != "198.51.100.7" {
		t.Errorf("clientIP() = %q, want 198.51.100.7 from RemoteAddr", got)
	}
}

// Sanity: mcpserver.AgentSession must round-trip through the cache without
// nil-pointer surprises when a test builds one directly (guards against a
// refactor of sessionCacheEntry accidentally storing the wrong pointer).
func TestAgentSessionCache_ReturnsSameSessionData(t *testing.T) {
	var calls int64
	api := countingMeshAPI(t, "agk_good", &calls)
	defer api.Close()

	cache := &agentSessionCache{apiURL: api.URL}
	s1, err := cache.GetOrAuthenticate(context.Background(), "agk_good")
	if err != nil {
		t.Fatalf("GetOrAuthenticate: %v", err)
	}
	s2, err := cache.GetOrAuthenticate(context.Background(), "agk_good")
	if err != nil {
		t.Fatalf("GetOrAuthenticate (cached): %v", err)
	}
	if s1.AgentID != s2.AgentID {
		t.Errorf("cached session AgentID = %v, want %v", s2.AgentID, s1.AgentID)
	}
}
