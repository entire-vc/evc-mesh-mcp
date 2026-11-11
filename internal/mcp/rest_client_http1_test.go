package mcp

import (
	"context"
	"crypto/tls"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// h2Server is a TLS test server that negotiates h2 when the client offers it
// and echoes the negotiated protocol and the request body.
func h2Server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Proto", r.Proto)
		_, _ = w.Write(b)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// trustTestCert lets the scoped transports accept the httptest certificate
// while keeping every other transport setting from the real constructor.
func trustTestCert(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	st, ok := rt.(*scopedTransport)
	if !ok {
		t.Fatalf("transport is %T, want *scopedTransport", rt)
	}
	h1 := st.h1.(*http.Transport)
	h1.TLSClientConfig.InsecureSkipVerify = true
	st.h1Buf.(*http.Transport).TLSClientConfig.InsecureSkipVerify = true
	next := http.DefaultTransport.(*http.Transport).Clone()
	next.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	st.next = next
}

func postProto(t *testing.T, c *http.Client, target, body string) (proto, echoed string) {
	t.Helper()
	resp, err := c.Post(target, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.Proto, string(b)
}

func TestScopedTransport_UnsetKeepsDefault(t *testing.T) {
	if rt := newScopedTransport(""); rt != nil {
		t.Fatalf("empty list must yield nil transport, got %T", rt)
	}
	if rt := newScopedTransport(" , ,"); rt != nil {
		t.Fatalf("blank entries must yield nil transport, got %T", rt)
	}
	t.Setenv(http1HostsEnv, "")
	if NewRESTClient("https://x.invalid", "k").httpClient.Transport != nil {
		t.Fatal("default client must keep nil (DefaultTransport) transport")
	}
}

func TestScopedTransport_ListedHostUsesHTTP11_OthersKeepH2(t *testing.T) {
	srv := h2Server(t)
	u, _ := url.Parse(srv.URL)
	rt := newScopedTransport("Other.example, " + u.Hostname())
	trustTestCert(t, rt)
	c := &http.Client{Transport: rt, Timeout: 5 * time.Second}

	proto, echo := postProto(t, c, srv.URL, `{"a":1}`)
	if proto != "HTTP/1.1" || echo != `{"a":1}` {
		t.Fatalf("listed host: proto=%s echo=%q, want HTTP/1.1 and body intact", proto, echo)
	}

	// Negative control: host not in the list still negotiates h2.
	rt2 := newScopedTransport("other.example")
	trustTestCert(t, rt2)
	c2 := &http.Client{Transport: rt2, Timeout: 5 * time.Second}
	// the clone of DefaultTransport only attempts h2 when ForceAttemptHTTP2 is
	// set (a custom TLS config disables it), so mirror the stock settings.
	rt2.(*scopedTransport).next.(*http.Transport).ForceAttemptHTTP2 = true
	proto, _ = postProto(t, c2, srv.URL, `{"a":1}`)
	if proto != "HTTP/2.0" {
		t.Fatalf("unlisted host: proto=%s, want HTTP/2.0", proto)
	}
}

func TestScopedTransport_Semantics(t *testing.T) {
	// proxy from environment is preserved on the h1 transport
	if newHTTP1Transport().Proxy == nil {
		t.Fatal("h1 transport lost the environment proxy func")
	}
	// TLS verification stays on
	if newHTTP1Transport().TLSClientConfig.InsecureSkipVerify {
		t.Fatal("h1 transport must verify certificates")
	}

	// redirect and deadline semantics ride on the http.Client, untouched
	var srv *httptest.Server
	srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/slow":
			select {
			case <-r.Context().Done():
			case <-time.After(3 * time.Second):
			}
		case "/redir":
			http.Redirect(w, r, "/ok", http.StatusTemporaryRedirect)
		default:
			b, _ := io.ReadAll(r.Body)
			_, _ = w.Write(b)
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	rt := newScopedTransport(u.Hostname())
	trustTestCert(t, rt)
	c := &http.Client{Transport: rt, Timeout: 5 * time.Second}

	// 307 keeps the POST body across the redirect
	if _, echo := postProto(t, c, srv.URL+"/redir", "payload"); echo != "payload" {
		t.Fatalf("redirect body = %q", echo)
	}

	// context cancellation aborts promptly
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, srv.URL+"/slow", strings.NewReader("x"))
	start := time.Now()
	if _, err := c.Do(req); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("cancellation not honoured: err=%v after %v", err, time.Since(start))
	}

	// client Timeout still applies
	c.Timeout = 150 * time.Millisecond
	start = time.Now()
	if _, err := c.Post(srv.URL+"/slow", "text/plain", strings.NewReader("x")); err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("client timeout not honoured: err=%v after %v", err, time.Since(start))
	}
}

func TestNewRESTClient_EnvEnablesScopedTransport(t *testing.T) {
	t.Setenv(http1HostsEnv, "mesh.example.com")
	if _, ok := NewRESTClient("https://mesh.example.com", "k").httpClient.Transport.(*scopedTransport); !ok {
		t.Fatal("env set must install the scoped transport")
	}
}
