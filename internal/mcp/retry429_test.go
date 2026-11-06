package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// fastRetryClient is a real RESTClient against a real HTTP server; only the
// wait is shortened (time is a true external boundary).
func fastRetryClient(url string, budget time.Duration) *RESTClient {
	c := NewRESTClient(url, "k")
	p := defaultRetry429Policy()
	p.budget = budget
	p.fallback = time.Millisecond
	p.sleep = func(ctx context.Context, d time.Duration) error {
		if d > 50*time.Millisecond {
			d = 50 * time.Millisecond
		}
		return sleepCtx(ctx, d)
	}
	c.retry = p
	return c
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC)
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"3", 3 * time.Second, true},
		{" 0 ", 0, true},
		{now.Add(7 * time.Second).Format(http.TimeFormat), 7 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
		{"", 0, false},
		{"soon", 0, false},
		{"-5", 0, false},
	}
	for _, c := range cases {
		got, ok := parseRetryAfter(c.in, now)
		if got != c.want || ok != c.ok {
			t.Errorf("parseRetryAfter(%q) = %v,%v want %v,%v", c.in, got, ok, c.want, c.ok)
		}
	}
}

func TestRetry429_ThenSuccess(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) <= 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			_, _ = w.Write([]byte(`{"message":"rate limited"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "t1"})
	}))
	defer srv.Close()
	got, err := fastRetryClient(srv.URL, 10*time.Second).GetTask(context.Background(), "t1")
	if err != nil || got["id"] != "t1" {
		t.Fatalf("want success after 429,429,200; got %v, %v", got, err)
	}
	if n != 3 {
		t.Errorf("requests = %d, want 3", n)
	}
}

func TestRetry429_RepeatedIsBoundedAndStructured(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"message":"rate limited"}`))
	}))
	defer srv.Close()
	_, err := fastRetryClient(srv.URL, 10*time.Second).GetTask(context.Background(), "t1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 || apiErr.Retry == nil || !apiErr.Retry.Retriable || !apiErr.Retry.RetryAfterValid {
		t.Fatalf("want retriable 429 APIError with Retry-After, got %#v", err)
	}
	if n != retry429MaxAttempts {
		t.Errorf("requests = %d, want max attempts %d", n, retry429MaxAttempts)
	}
	if !strings.Contains(err.Error(), "retry after") {
		t.Errorf("error text lacks retry time: %v", err)
	}
}

func TestRetry429_RetryAfterBeyondBudgetReturnsImmediately(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Retry-After", "600")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	start := time.Now()
	_, err := fastRetryClient(srv.URL, 2*time.Second).GetTask(context.Background(), "t1")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Retry == nil || apiErr.Retry.RetryAfter != 600*time.Second {
		t.Fatalf("want structured error with 600s, got %#v", err)
	}
	if n != 1 || time.Since(start) > time.Second {
		t.Errorf("must not wait/retry: requests=%d elapsed=%v", n, time.Since(start))
	}
}

func TestRetry429_CancellationStopsWaiting(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Retry-After", "5")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	c := NewRESTClient(srv.URL, "k") // real sleeper
	p := defaultRetry429Policy()
	p.budget = 30 * time.Second
	c.retry = p
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(100 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := c.GetTask(ctx, "t1")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if time.Since(start) > 2*time.Second || n != 1 {
		t.Errorf("cancellation not prompt: elapsed=%v requests=%d", time.Since(start), n)
	}
}

func TestRetry429_NonRetryableAndWritesCalledOnce(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		call   func(c *RESTClient) error
	}{
		{"401 read", 401, func(c *RESTClient) error { _, err := c.GetTask(context.Background(), "t"); return err }},
		{"403 read", 403, func(c *RESTClient) error { _, err := c.GetTask(context.Background(), "t"); return err }},
		{"429 write", 429, func(c *RESTClient) error {
			_, err := c.AddComment(context.Background(), "t", map[string]any{"body": "x"})
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var n int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&n, 1)
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			if err := tc.call(fastRetryClient(srv.URL, 10*time.Second)); err == nil {
				t.Fatal("want error")
			}
			if n != 1 {
				t.Errorf("requests = %d, want exactly 1", n)
			}
		})
	}
}

func TestRetry429_NoHeaderUsesBoundedBackoff(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) == 1 {
			w.Header().Set("Retry-After", "garbage")
			w.WriteHeader(429)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "t1"})
	}))
	defer srv.Close()
	if _, err := fastRetryClient(srv.URL, 10*time.Second).GetTask(context.Background(), "t1"); err != nil || n != 2 {
		t.Fatalf("err=%v requests=%d", err, n)
	}
}

func TestRetry429_ConcurrentWaitersCapped(t *testing.T) {
	var inWait, maxWait int32
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
	}))
	defer srv.Close()
	c := fastRetryClient(srv.URL, 10*time.Second)
	c.retry.sleep = func(ctx context.Context, d time.Duration) error {
		cur := atomic.AddInt32(&inWait, 1)
		mu.Lock()
		if cur > maxWait {
			maxWait = cur
		}
		mu.Unlock()
		defer atomic.AddInt32(&inWait, -1)
		return sleepCtx(ctx, 20*time.Millisecond)
	}
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = c.GetTask(context.Background(), "t") }()
	}
	wg.Wait()
	if maxWait > retry429MaxConcurrent || maxWait == 0 {
		t.Errorf("max simultaneous retry waiters = %d, want 1..%d", maxWait, retry429MaxConcurrent)
	}
}

// Composite get_task: a throttled part is retried alone; parts that already
// succeeded are not requested again; a part that stays throttled is reported as
// missing, not as empty.
func TestGetTask_PartialSuccessRequestCounts(t *testing.T) {
	taskID := "11111111-1111-1111-1111-111111111111"
	var mu sync.Mutex
	counts := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := "task"
		switch {
		case strings.HasSuffix(r.URL.Path, "/comments"):
			key = "comments"
		case strings.HasSuffix(r.URL.Path, "/dependencies"):
			key = "deps"
		}
		mu.Lock()
		counts[key]++
		n := counts[key]
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if key == "comments" && n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			return
		}
		if key == "deps" {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429) // never recovers
			return
		}
		if key == "comments" {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "total_count": 0, "has_more": false})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID, "title": "t", "updated_at": "2026-11-06T10:00:00Z"})
	}))
	defer srv.Close()

	s := &Server{restClient: fastRetryClient(srv.URL, 10*time.Second), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID, "include_comments": true, "include_dependencies": true}
	res, err := s.handleGetTask(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("partial result expected, got err=%v res=%+v", err, res)
	}
	var out map[string]any
	if e := json.Unmarshal([]byte(res.Content[0].(mcpsdk.TextContent).Text), &out); e != nil {
		t.Fatal(e)
	}
	if out["incomplete"] != true {
		t.Errorf("response must be flagged incomplete: %v", out)
	}
	if _, has := out["dependencies"]; has {
		t.Errorf("missing dependencies must be absent, not empty: %v", out["dependencies"])
	}
	if _, has := out["comments"]; !has {
		t.Errorf("successful comments part lost")
	}
	pe, _ := out["part_errors"].(map[string]any)["dependencies"].(map[string]any)
	if pe == nil || pe["retriable"] != true || pe["retry_after_seconds"] != float64(1) {
		t.Errorf("part_errors.dependencies = %v", pe)
	}
	if counts["task"] != 1 || counts["comments"] != 2 || counts["deps"] != retry429MaxAttempts {
		t.Errorf("request counts task/comments/deps = %d/%d/%d, want 1/2/%d", counts["task"], counts["comments"], counts["deps"], retry429MaxAttempts)
	}
}

func TestParseRetryAfter_HugeValueKeptAsOverBudget(t *testing.T) {
	d, ok := parseRetryAfter("99999999999", time.Now())
	if !ok || d < 365*24*time.Hour {
		t.Fatalf("huge delta-seconds must stay valid and large, got %v %v", d, ok)
	}
}

// Under slot contention a call must not outlive its budget nor fire another
// request after it.
func TestRetry429_BudgetHeldUnderSlotContention(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"message":"rl"}`))
	}))
	defer srv.Close()
	c := fastRetryClient(srv.URL, 300*time.Millisecond)
	c.retryPolicy()
	for i := 0; i < retry429MaxConcurrent; i++ { // all slots held by other callers
		c.retrySlots <- struct{}{}
	}
	start := time.Now()
	_, err := c.GetTask(context.Background(), "t")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 {
		t.Fatalf("want 429 APIError, got %v", err)
	}
	if el := time.Since(start); el > time.Second || n != 1 {
		t.Errorf("budget exceeded under contention: elapsed=%v requests=%d", el, n)
	}
}

// A wait that exactly consumes the budget must not be followed by another request.
func TestRetry429_NoRequestAfterBudgetExpires(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.Header().Set("Retry-After", "1")
		w.WriteHeader(429)
		_, _ = w.Write([]byte(`{"message":"rl"}`))
	}))
	defer srv.Close()
	c := NewRESTClient(srv.URL, "k")
	p := defaultRetry429Policy()
	p.budget = time.Second // Retry-After == budget
	p.jitter = func(time.Duration) time.Duration { return 0 }
	c.retry = p
	_, err := c.GetTask(context.Background(), "t")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != 429 || apiErr.Message != "rl" {
		t.Fatalf("want 429 APIError with server body, got %v", err)
	}
	if n != 1 {
		t.Fatalf("requests = %d, want 1 (no request past the deadline)", n)
	}
}
