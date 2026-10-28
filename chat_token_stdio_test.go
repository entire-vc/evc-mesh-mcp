package main

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestChatTokenStdio_StartsWithoutAgentKey runs the built binary in stdio mode
// with MESH_CHAT_TOKEN_FILE and no MESH_AGENT_KEY: it must authenticate with
// the chat token and answer tools/list.
func TestChatTokenStdio_StartsWithoutAgentKey(t *testing.T) {
	var mu sync.Mutex
	var meAuth []string
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/agents/me" {
			mu.Lock()
			meAuth = append(meAuth, r.Header.Get("Authorization")+"|"+r.Header.Get("X-Agent-Key"))
			mu.Unlock()
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"0b5f6d2e-3c1a-4e8b-9f7d-2a6c4e8b1d3f","workspace_id":"5e7a9c1b-2d4f-4a6c-8e0b-1d3f5a7c9e2b","name":"chat-agent","agent_type":"claude_code"}`))
	}))
	defer api.Close()

	dir := t.TempDir()
	bin := filepath.Join(dir, "mesh-mcp-chat-test")
	if out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}
	tokenFile := filepath.Join(dir, "chat-token")
	if err := os.WriteFile(tokenFile, []byte("cst_stdio_test\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command(bin)
	cmd.Env = []string{"MESH_API_URL=" + api.URL, "MESH_CHAT_TOKEN_FILE=" + tokenFile}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()

	for _, msg := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"test","version":"0"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
	} {
		if _, err := io.WriteString(stdin, msg+"\n"); err != nil {
			t.Fatal(err)
		}
	}

	got := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(stdout)
		sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
		for sc.Scan() {
			var resp struct {
				ID     int `json:"id"`
				Result struct {
					Tools []json.RawMessage `json:"tools"`
				} `json:"result"`
			}
			if json.Unmarshal(sc.Bytes(), &resp) == nil && resp.ID == 2 {
				got <- len(resp.Result.Tools)
				return
			}
		}
	}()
	select {
	case n := <-got:
		if n == 0 {
			t.Fatal("tools/list returned no tools")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("no tools/list response within 30s")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(meAuth) == 0 {
		t.Fatal("the server never authenticated against /api/v1/agents/me")
	}
	if meAuth[0] != "Bearer cst_stdio_test|" {
		t.Errorf("agents/me auth = %q, want the chat token as bearer and no agent key", meAuth[0])
	}
}
