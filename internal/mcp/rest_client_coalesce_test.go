package mcp

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// writeRecorder wraps the client side of the TLS connection and records the size
// of every plaintext Write the HTTP/1.1 transport issues, i.e. the boundary the
// incident probe measured (4096 + remainder with the stock 4KB buffer).
type writeRecorder struct {
	net.Conn
	mu     *sync.Mutex
	writes *[]int
}

func (w writeRecorder) Write(b []byte) (int, error) {
	w.mu.Lock()
	*w.writes = append(*w.writes, len(b))
	w.mu.Unlock()
	return w.Conn.Write(b)
}

// recordWrites makes tr dial TLS itself (certificate check relaxed for the test
// server only) and returns a func yielding the recorded write sizes. The
// recorder hides *tls.Conn from net/http, which is fine: h1 only.
func recordWrites(tr *http.Transport) func() []int {
	var mu sync.Mutex
	var writes []int
	tr.DialTLSContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, _, _ := net.SplitHostPort(addr)
		c, err := (&tls.Dialer{Config: &tls.Config{InsecureSkipVerify: true, ServerName: host, NextProtos: []string{"http/1.1"}}}).DialContext(ctx, network, addr)
		if err != nil {
			return nil, err
		}
		return writeRecorder{Conn: c, mu: &mu, writes: &writes}, nil
	}
	return func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), writes...)
	}
}

func echoServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Proto", r.Proto)
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	u, _ := url.Parse(srv.URL)
	return srv, u.Hostname()
}

func jsonBody(n int) string {
	const pre, post = `{"content":"`, `"}`
	return pre + strings.Repeat("a", n-len(pre)-len(post)) + post
}

func newRecordedTransport(t *testing.T, host string) (*scopedTransport, func() []int, func() []int) {
	rt := newScopedTransport(host).(*scopedTransport)
	plain := recordWrites(rt.h1.(*http.Transport))
	buf := recordWrites(rt.h1Buf.(*http.Transport))
	return rt, plain, buf
}

func post(t *testing.T, c *http.Client, target, ct string, body io.Reader) string {
	t.Helper()
	resp, err := c.Post(target, ct, body)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.ProtoMajor != 1 || resp.ProtoMinor != 1 {
		t.Fatalf("proto = %s, want HTTP/1.1", resp.Proto)
	}
	return string(b)
}

func TestCoalesce_BoundedJSONIsOneWrite(t *testing.T) {
	srv, host := echoServer(t)
	for _, n := range []int{600, 6355, 9947, coalesceMaxBody} {
		rt, plain, buf := newRecordedTransport(t, host)
		c := &http.Client{Transport: rt, Timeout: 5 * time.Second}
		body := jsonBody(n)
		if got := post(t, c, srv.URL, "application/json", strings.NewReader(body)); got != body {
			t.Fatalf("n=%d: endpoint received %d bytes, want exact %d", n, len(got), n)
		}
		if w := buf(); len(w) != 1 || w[0] < n {
			t.Fatalf("n=%d: coalescing transport writes = %v, want exactly one write >= body", n, w)
		}
		if w := plain(); len(w) != 0 {
			t.Fatalf("n=%d: stock h1 transport must be unused, writes=%v", n, w)
		}
	}
}

// Negative control: the stock 4KB buffer splits the same bodies, which is the
// defect being mitigated. If this stops failing to coalesce, the test above no
// longer proves anything.
func TestCoalesce_NegativeControl_StockBufferSplits(t *testing.T) {
	srv, host := echoServer(t)
	rt, plain, _ := newRecordedTransport(t, host)
	rt.h1Buf = rt.h1 // force the stock-buffer path
	c := &http.Client{Transport: rt, Timeout: 5 * time.Second}
	body := jsonBody(6355)
	if got := post(t, c, srv.URL, "application/json", strings.NewReader(body)); got != body {
		t.Fatal("body not intact on stock path")
	}
	w := plain()
	if len(w) < 2 || w[0] != 4096 {
		t.Fatalf("stock writes = %v, want first write 4096 then remainder", w)
	}
}

func TestCoalesce_FallbackKeepsStockBuffer(t *testing.T) {
	srv, host := echoServer(t)
	cases := []struct {
		name string
		ct   string
		body io.Reader
	}{
		{"over-limit", "application/json", strings.NewReader(jsonBody(coalesceMaxBody + 1))},
		{"stream-unknown-length", "application/json", io.NopCloser(strings.NewReader(jsonBody(6355)))},
		{"multipart", "multipart/form-data; boundary=x", strings.NewReader(jsonBody(6355))},
		{"non-json", "text/plain", strings.NewReader(jsonBody(6355))},
	}
	for _, tc := range cases {
		rt, plain, buf := newRecordedTransport(t, host)
		c := &http.Client{Transport: rt, Timeout: 5 * time.Second}
		want, _ := io.ReadAll(tc.body)
		var rd io.Reader = strings.NewReader(string(want))
		if tc.name == "stream-unknown-length" {
			rd = io.NopCloser(strings.NewReader(string(want)))
		}
		if got := post(t, c, srv.URL, tc.ct, rd); got != string(want) {
			t.Fatalf("%s: body not intact", tc.name)
		}
		if len(buf()) != 0 || len(plain()) == 0 {
			t.Fatalf("%s: must use stock h1 transport (buf=%v plain=%v)", tc.name, buf(), plain())
		}
	}
}

func TestCoalesce_UnlistedHostAndGETUntouched(t *testing.T) {
	srv, host := echoServer(t)
	rt, plain, buf := newRecordedTransport(t, host)
	c := &http.Client{Transport: rt, Timeout: 5 * time.Second}
	resp, err := c.Get(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(buf()) != 0 || len(plain()) == 0 {
		t.Fatalf("GET must stay on stock h1 (buf=%v plain=%v)", buf(), plain())
	}
	if useCoalescedWrite(&http.Request{Method: http.MethodPost, URL: &url.URL{Path: "/x"}, ContentLength: 10, Header: http.Header{"Content-Type": {"application/json; charset=utf-8"}}}) != true {
		t.Fatal("json with charset must qualify")
	}
	// unlisted host: never reaches either h1 transport
	rt2 := newScopedTransport("other.example").(*scopedTransport)
	if _, ok := rt2.hosts[host]; ok {
		t.Fatal("host unexpectedly listed")
	}
}

func TestCoalesce_TransportPins(t *testing.T) {
	tr := newHTTP1BufferedTransport()
	if tr.WriteBufferSize != h1WriteBufferSize || h1WriteBufferSize < 2*coalesceMaxBody {
		t.Fatalf("buffer %d must leave header headroom over %d", tr.WriteBufferSize, coalesceMaxBody)
	}
	if len(tr.TLSNextProto) != 0 || tr.ForceAttemptHTTP2 || tr.Proxy == nil || tr.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("buffered transport lost h1-only / proxy / verify settings")
	}
	if newHTTP1Transport().WriteBufferSize != 0 {
		t.Fatal("stock h1 transport must keep the default buffer")
	}
	if http.DefaultTransport.(*http.Transport).WriteBufferSize != 0 {
		t.Fatal("global default transport must not be modified")
	}
}

// A qualifying body with oversized headers or URL would not fit the buffer and
// must fall back to the stock transport instead of silently splitting.
func TestCoalesce_LargeHeadersOrURLFallBack(t *testing.T) {
	mk := func(path string, hdr int) *http.Request {
		r, _ := http.NewRequest(http.MethodPost, "https://h.example"+path, strings.NewReader(jsonBody(coalesceMaxBody)))
		r.Header.Set("Content-Type", "application/json")
		if hdr > 0 {
			r.Header.Set("X-Big", strings.Repeat("h", hdr))
		}
		return r
	}
	if !useCoalescedWrite(mk("/p", 0)) {
		t.Fatal("max body with normal headers must coalesce")
	}
	if useCoalescedWrite(mk("/p", 40<<10)) {
		t.Fatal("40KB header must fall back")
	}
	if useCoalescedWrite(mk("/"+strings.Repeat("u", 40<<10), 0)) {
		t.Fatal("40KB URL must fall back")
	}
	srv, host := echoServer(t)
	rt, plain, buf := newRecordedTransport(t, host)
	c := &http.Client{Transport: rt, Timeout: 5 * time.Second}
	req, _ := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(jsonBody(coalesceMaxBody)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Big", strings.Repeat("h", 40<<10))
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(buf()) != 0 || len(plain()) == 0 {
		t.Fatalf("large-header request must use stock h1 (buf=%v plain=%v)", buf(), plain())
	}
}
