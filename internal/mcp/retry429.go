package mcp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Bounded retry of rate-limited (HTTP 429) reads.
//
// The Mesh API answers an actor that exceeded its limit with 429 and a
// Retry-After header. Before this, the client dropped the header and failed the
// call at once, so a composite read such as get_task(include_comments,
// include_dependencies) — three or more HTTP calls — lost the whole result when
// any one of them was throttled.
//
// Only GET/HEAD are retried: they are idempotent. A write that got a 429 is
// returned to the caller untouched (called once), because the client cannot know
// whether a throttled write had any effect.
const (
	// retry429MaxAttempts is the total number of HTTP attempts for one call
	// (the first try plus retries).
	retry429MaxAttempts = 4
	// retry429DefaultBudget bounds the total time one call may spend waiting
	// on Retry-After. MESH_MCP_429_BUDGET_SECS overrides it (1..120).
	retry429DefaultBudget = 15 * time.Second
	// retry429FallbackBase is the first backoff step when Retry-After is
	// absent or invalid; it doubles per attempt.
	retry429FallbackBase = 500 * time.Millisecond
	// retry429MaxConcurrent caps how many calls of one client may sit in a
	// retry wait at once, so a burst of throttled calls does not all wake and
	// re-hit the API in the same instant.
	retry429MaxConcurrent = 4
)

// retry429Policy is the tunable part, replaceable in tests.
type retry429Policy struct {
	maxAttempts int
	budget      time.Duration
	fallback    time.Duration
	now         func() time.Time
	jitter      func(d time.Duration) time.Duration // returns extra delay in [0, d/4]
	sleep       func(ctx context.Context, d time.Duration) error
}

func defaultRetry429Policy() retry429Policy {
	return retry429Policy{
		maxAttempts: retry429MaxAttempts,
		budget:      retry429BudgetFromEnv(),
		fallback:    retry429FallbackBase,
		now:         time.Now,
		jitter: func(d time.Duration) time.Duration {
			if d <= 0 {
				return 0
			}
			return time.Duration(rand.Int63n(int64(d/4) + 1))
		},
		sleep: sleepCtx,
	}
}

func retry429BudgetFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("MESH_MCP_429_BUDGET_SECS"))
	if raw == "" {
		return retry429DefaultBudget
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs < 1 || secs > 120 {
		return retry429DefaultBudget
	}
	return time.Duration(secs) * time.Second
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// parseRetryAfter reads a Retry-After header: delta-seconds or an HTTP-date.
// ok is false for an absent, malformed or negative value; a date in the past
// yields 0.
func parseRetryAfter(h string, now time.Time) (d time.Duration, ok bool) {
	h = strings.TrimSpace(h)
	if h == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(h, 10, 64); err == nil {
		if secs < 0 {
			return 0, false
		}
		// A valid but absurd delay is kept (clamped to avoid Duration
		// overflow) so it surfaces as an over-budget retriable error.
		const maxSecs = 10 * 365 * 24 * 3600
		if secs > maxSecs {
			secs = maxSecs
		}
		return time.Duration(secs) * time.Second, true
	}
	if t, err := http.ParseTime(h); err == nil {
		if d := t.Sub(now); d > 0 {
			return d, true
		}
		return 0, true
	}
	return 0, false
}

func isIdempotentRead(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

// retryDelayFor decides how long to wait before attempt number `attempt`
// (1-based: the attempt that just failed) given the 429 response headers.
func (p retry429Policy) retryDelayFor(h http.Header, attempt int) (wait time.Duration, headerValid bool) {
	if d, ok := parseRetryAfter(h.Get("Retry-After"), p.now()); ok {
		return d, true
	}
	// Unknown or invalid delay: bounded exponential backoff.
	d := p.fallback << (attempt - 1)
	if max := 8 * time.Second; d > max || d <= 0 {
		d = 8 * time.Second
	}
	return d, false
}

// do429 wraps one HTTP call with the bounded retry. send performs a single
// attempt. For non-idempotent methods it is called exactly once.
func (c *RESTClient) do429(ctx context.Context, method string, send func() (*http.Response, error)) (*http.Response, error) {
	if !isIdempotentRead(method) {
		return send()
	}
	p := c.retryPolicy()
	start := p.now()
	for attempt := 1; ; attempt++ {
		resp, err := send()
		if err != nil || resp.StatusCode != http.StatusTooManyRequests {
			return resp, err
		}
		wait, _ := p.retryDelayFor(resp.Header, attempt)
		remaining := p.budget - p.now().Sub(start)
		// Out of attempts, or the server asks for longer than we may wait:
		// hand the 429 (with its Retry-After intact) to the caller.
		if attempt >= p.maxAttempts || wait > remaining {
			return resp, nil
		}
		wait += p.jitter(wait)
		if wait > remaining {
			wait = remaining
		}
		// Keep the body: if the retry is abandoned below, the caller still
		// decodes the server's error from this very response.
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		restore := func() *http.Response {
			resp.Body = io.NopCloser(bytes.NewReader(buf))
			return resp
		}

		// Bound simultaneous retry waiters so clients do not re-fire together.
		// Slot acquisition itself is limited by the call's remaining budget,
		// and the budget is re-checked once the slot is held.
		slotCtx, cancel := context.WithTimeout(ctx, wait0(remaining))
		serr := c.acquireRetrySlot(slotCtx)
		cancel()
		if serr != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return restore(), nil
		}
		remaining = p.budget - p.now().Sub(start)
		if wait > remaining {
			c.releaseRetrySlot()
			return restore(), nil
		}
		serr = p.sleep(ctx, wait)
		c.releaseRetrySlot()
		if serr != nil {
			return nil, serr
		}
		// The wait may have consumed the whole budget (wait == remaining):
		// never start a request past the deadline.
		if p.now().Sub(start) >= p.budget {
			return restore(), nil
		}
	}
}

func wait0(d time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	return d
}

func (c *RESTClient) retryPolicy() retry429Policy {
	c.retryOnce.Do(func() {
		if c.retry.sleep == nil {
			c.retry = defaultRetry429Policy()
		}
		c.retrySlots = make(chan struct{}, retry429MaxConcurrent)
	})
	return c.retry
}

func (c *RESTClient) acquireRetrySlot(ctx context.Context) error {
	select {
	case c.retrySlots <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (c *RESTClient) releaseRetrySlot() { <-c.retrySlots }

// RetryInfo is the structured form of a rate-limit refusal.
type RetryInfo struct {
	Retriable       bool
	RetryAfter      time.Duration
	RetryAfterValid bool // false when the server sent no usable Retry-After
	RetryAt         time.Time
}

// rateLimitInfo builds the retry metadata of a 429 response.
func rateLimitInfo(h http.Header, now time.Time) *RetryInfo {
	d, ok := parseRetryAfter(h.Get("Retry-After"), now)
	ri := &RetryInfo{Retriable: true, RetryAfter: d, RetryAfterValid: ok}
	if ok {
		ri.RetryAt = now.Add(d).UTC()
	}
	return ri
}

func (r *RetryInfo) String() string {
	if r == nil {
		return ""
	}
	if !r.RetryAfterValid {
		return "retriable, no valid Retry-After"
	}
	return fmt.Sprintf("retriable, retry after %ds (at %s)", int(r.RetryAfter.Round(time.Second).Seconds()), r.RetryAt.Format(time.RFC3339))
}
