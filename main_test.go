package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	mcpserver "github.com/entire-vc/evc-mesh-mcp/internal/mcp"
	sdkserver "github.com/mark3labs/mcp-go/server"
)

// TestVersionFlag_PrintsBuildSHA builds the binary with an injected BuildSHA
// and confirms `--version`/`-version` print exactly that SHA and exit 0
// without requiring MESH_AGENT_KEY or network access — see task #1c602063
// (the flag didn't exist at all, so an agent had no way to tell an installed
// binary's origin short of `strings` on the host).
func TestVersionFlag_PrintsBuildSHA(t *testing.T) {
	bin := t.TempDir() + "/mesh-mcp-version-test"
	const wantSHA = "test-sha-1234567"

	build := exec.Command("go", "build",
		"-ldflags", "-X github.com/entire-vc/evc-mesh-mcp/internal/mcp.BuildSHA="+wantSHA,
		"-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	for _, flag := range []string{"--version", "-version"} {
		t.Run(flag, func(t *testing.T) {
			cmd := exec.Command(bin, flag)
			cmd.Env = []string{} // deliberately no MESH_AGENT_KEY / MESH_API_URL
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s exited with error: %v\noutput: %s", flag, err, out)
			}
			got := strings.TrimSpace(string(out))
			if got != wantSHA {
				t.Errorf("%s printed %q, want %q", flag, got, wantSHA)
			}
		})
	}
}

// TestVersionFlag_DefaultsToDev is the negative control: an unpinned build
// (no -ldflags override) must print "dev", never a blank string or a stale
// SHA left over from a previous build.
func TestVersionFlag_DefaultsToDev(t *testing.T) {
	bin := t.TempDir() + "/mesh-mcp-version-dev-test"
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("--version exited with error: %v\noutput: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "dev" {
		t.Errorf("unpinned build printed %q, want \"dev\"", got)
	}
}

// TestAuthenticateWithRetry_RecoversFromTransient502 is the positive control
// for task #8afc7aba: a transient 502 on the first attempt must not kill the
// process — the retry must succeed and return the agent info, exactly like a
// second, healthy spawn would have (the lucky self-heal path task #1f550cee
// hit by accident).
func TestAuthenticateWithRetry_RecoversFromTransient502(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"id": "agent-1", "name": "linus"})
	}))
	defer srv.Close()

	origBackoff := authRetryBackoff
	authRetryBackoff = time.Millisecond
	defer func() { authRetryBackoff = origBackoff }()

	client := mcpserver.NewRESTClient(srv.URL, "agk_test")
	agentInfo, err := authenticateWithRetry(context.Background(), client)
	if err != nil {
		t.Fatalf("authenticateWithRetry returned error after a transient 502: %v", err)
	}
	if agentInfo["name"] != "linus" {
		t.Errorf("got agent info %v, want name=linus", agentInfo)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Errorf("server received %d calls, want exactly 2 (one 502, one success)", got)
	}
}

// TestAuthenticateWithRetry_DoesNotRetry401 is the negative control: a bad
// credential is a real, permanent failure and must fail immediately on the
// first attempt — retrying it would only mask a config problem behind a few
// seconds of pointless waiting.
func TestAuthenticateWithRetry_DoesNotRetry401(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "invalid agent key"})
	}))
	defer srv.Close()

	origBackoff := authRetryBackoff
	authRetryBackoff = time.Millisecond
	defer func() { authRetryBackoff = origBackoff }()

	client := mcpserver.NewRESTClient(srv.URL, "agk_bad")
	_, err := authenticateWithRetry(context.Background(), client)
	if err == nil {
		t.Fatal("authenticateWithRetry succeeded on a 401, want error")
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Errorf("server received %d calls, want exactly 1 (no retry on 401)", got)
	}
}

// TestIsTransientAuthError classifies which failures are worth retrying.
func TestIsTransientAuthError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"502 bad gateway", &mcpserver.APIError{StatusCode: http.StatusBadGateway}, true},
		{"503 unavailable", &mcpserver.APIError{StatusCode: http.StatusServiceUnavailable}, true},
		{"504 gateway timeout", &mcpserver.APIError{StatusCode: http.StatusGatewayTimeout}, true},
		{"401 unauthorized", &mcpserver.APIError{StatusCode: http.StatusUnauthorized}, false},
		{"403 forbidden", &mcpserver.APIError{StatusCode: http.StatusForbidden}, false},
		{"404 not found", &mcpserver.APIError{StatusCode: http.StatusNotFound}, false},
		{"network error (not an APIError)", context.DeadlineExceeded, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isTransientAuthError(tc.err); got != tc.want {
				t.Errorf("isTransientAuthError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// advertiseOptions — which endpoint URL the SSE handshake hands the client
// (task #3bc9f59d / #2a0ec14c — dual-profile SSE + MESH_MCP_PUBLIC_URL wiring)
// ---------------------------------------------------------------------------

// newTestSSEServer builds an SSE server the way main() does, so the assertions
// below are about the real handshake behaviour rather than a stand-in.
func newTestSSEServer(t *testing.T, publicURL, basePath string) *sdkserver.SSEServer {
	t.Helper()
	srv := mcpserver.NewServer(mcpserver.ServerConfig{
		RESTClient: mcpserver.NewRESTClient("http://localhost:8005", ""),
		Profile:    mcpserver.ProfileCore,
	})
	return sdkserver.NewSSEServer(srv.MCPServer(), advertiseOptions(publicURL, basePath)...)
}

// A client that connects over a published container port or a reverse proxy has
// to be told where to POST its messages. The listen address must never be the
// answer: 0.0.0.0 is a wildcard bind, not somewhere a client can dial.
func TestAdvertiseOptions_NoPublicURL_AdvertisesRelativePath(t *testing.T) {
	sse := newTestSSEServer(t, "", "")

	req := httptest.NewRequest(http.MethodGet, "/sse", http.NoBody)
	endpoint := sse.GetMessageEndpointForClient(req, "sid-1")

	if want := "/message?sessionId=sid-1"; endpoint != want {
		t.Errorf("endpoint = %q, want %q", endpoint, want)
	}
	if strings.Contains(endpoint, "0.0.0.0") {
		t.Errorf("endpoint %q leaks the wildcard bind address", endpoint)
	}
}

func TestAdvertiseOptions_NoPublicURL_CoreProfileKeepsItsBasePath(t *testing.T) {
	sse := newTestSSEServer(t, "", coreBasePath)

	req := httptest.NewRequest(http.MethodGet, "/core/sse", http.NoBody)
	endpoint := sse.GetMessageEndpointForClient(req, "sid-2")

	if want := "/core/message?sessionId=sid-2"; endpoint != want {
		t.Errorf("endpoint = %q, want %q", endpoint, want)
	}
}

func TestAdvertiseOptions_PublicURL_AdvertisesAbsoluteURL(t *testing.T) {
	sse := newTestSSEServer(t, "https://mesh.example.com/mcp", "")

	req := httptest.NewRequest(http.MethodGet, "/sse", http.NoBody)
	endpoint := sse.GetMessageEndpointForClient(req, "sid-3")

	if want := "https://mesh.example.com/mcp/message?sessionId=sid-3"; endpoint != want {
		t.Errorf("endpoint = %q, want %q", endpoint, want)
	}
}

func TestAdvertiseOptions_PublicURL_CoreProfileIsNestedUnderIt(t *testing.T) {
	sse := newTestSSEServer(t, "https://mesh.example.com/mcp", coreBasePath)

	req := httptest.NewRequest(http.MethodGet, "/core/sse", http.NoBody)
	endpoint := sse.GetMessageEndpointForClient(req, "sid-4")

	if want := "https://mesh.example.com/mcp/core/message?sessionId=sid-4"; endpoint != want {
		t.Errorf("endpoint = %q, want %q", endpoint, want)
	}
}

func TestAdvertiseOptions_PublicURL_TrailingSlashDoesNotDoubleUp(t *testing.T) {
	sse := newTestSSEServer(t, "https://mesh.example.com/mcp/", "")

	req := httptest.NewRequest(http.MethodGet, "/sse", http.NoBody)
	endpoint := sse.GetMessageEndpointForClient(req, "sid-5")

	if want := "https://mesh.example.com/mcp/message?sessionId=sid-5"; endpoint != want {
		t.Errorf("endpoint = %q, want %q", endpoint, want)
	}
}

// ---------------------------------------------------------------------------
// dialableURL — the URL printed in the startup log
// ---------------------------------------------------------------------------

func TestDialableURL(t *testing.T) {
	cases := []struct {
		name      string
		publicURL string
		host      string
		port      string
		want      string
	}{
		{"wildcard host is reported as localhost", "", "0.0.0.0", "8081", "http://localhost:8081"},
		{"ipv6 wildcard too", "", "::", "8081", "http://localhost:8081"},
		{"explicit host is kept", "", "127.0.0.1", "9000", "http://127.0.0.1:9000"},
		{"public URL wins", "https://mesh.example.com/mcp", "0.0.0.0", "8081", "https://mesh.example.com/mcp"},
		{"public URL loses its trailing slash", "https://mesh.example.com/mcp/", "0.0.0.0", "8081", "https://mesh.example.com/mcp"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := dialableURL(tc.publicURL, tc.host, tc.port); got != tc.want {
				t.Errorf("dialableURL(%q, %q, %q) = %q, want %q", tc.publicURL, tc.host, tc.port, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Dual-profile server: core must register strictly fewer tools than full —
// this is the property the whole point of having two profiles rests on. A
// bare 200/OK from both SSE endpoints would hide a profile that silently
// didn't apply; this asserts on the actual registered tool count instead.
// ---------------------------------------------------------------------------

func TestDualProfileServers_CoreHasFewerToolsThanFull(t *testing.T) {
	restClient := mcpserver.NewRESTClient("http://localhost:8005", "")
	fullSrv := mcpserver.NewServer(mcpserver.ServerConfig{RESTClient: restClient, Profile: mcpserver.ProfileFull})
	coreSrv := mcpserver.NewServer(mcpserver.ServerConfig{RESTClient: restClient, Profile: mcpserver.ProfileCore})

	fullTools := fullSrv.MCPServer().ListTools()
	coreTools := coreSrv.MCPServer().ListTools()

	if len(coreTools) == 0 {
		t.Fatal("core profile registered zero tools — profile likely not wired at all")
	}
	if len(fullTools) == 0 {
		t.Fatal("full profile registered zero tools — regression unrelated to this change")
	}
	if len(coreTools) >= len(fullTools) {
		t.Errorf("core profile has %d tools, full has %d — core must be strictly smaller, otherwise routing a client to /core/sse silently gives them the full surface", len(coreTools), len(fullTools))
	}
}
