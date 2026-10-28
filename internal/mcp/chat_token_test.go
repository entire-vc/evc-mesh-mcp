package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// chatTokenServer records the auth headers of every request it receives.
type chatTokenServer struct {
	mu        sync.Mutex
	bearers   []string
	agentKeys []string
}

func (s *chatTokenServer) handler(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	s.bearers = append(s.bearers, r.Header.Get("Authorization"))
	s.agentKeys = append(s.agentKeys, r.Header.Get("X-Agent-Key"))
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"agent-1"}`))
}

func writeChatToken(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}
}

func TestChatToken_BearerIsFileContent(t *testing.T) {
	rec := &chatTokenServer{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "chat-token")
	writeChatToken(t, path, "  cst_first\n")

	c := NewChatTokenRESTClient(srv.URL, path)
	if _, err := c.GetAgentMe(context.Background()); err != nil {
		t.Fatalf("GetAgentMe: %v", err)
	}
	if rec.bearers[0] != "Bearer cst_first" {
		t.Errorf("Authorization = %q, want %q", rec.bearers[0], "Bearer cst_first")
	}
	if rec.agentKeys[0] != "" {
		t.Errorf("X-Agent-Key = %q, want none", rec.agentKeys[0])
	}
}

func TestChatToken_RewrittenFileIsPickedUp(t *testing.T) {
	rec := &chatTokenServer{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "chat-token")
	writeChatToken(t, path, "cst_old")
	c := NewChatTokenRESTClient(srv.URL, path)
	if _, err := c.GetAgentMe(context.Background()); err != nil {
		t.Fatalf("first call: %v", err)
	}

	writeChatToken(t, path, "cst_new")
	if _, err := c.GetAgentMe(context.Background()); err != nil {
		t.Fatalf("second call: %v", err)
	}
	if rec.bearers[1] != "Bearer cst_new" {
		t.Errorf("after rewrite Authorization = %q, want %q", rec.bearers[1], "Bearer cst_new")
	}
}

// With both credentials configured, the chat token wins and the agent key is
// never sent — not as a header next to the token, not as a fallback.
func TestChatToken_WinsOverAgentKey(t *testing.T) {
	t.Setenv("MESH_AGENT_KEY", "agk_should_not_be_sent")
	t.Setenv("MESH_CHAT_TOKEN_FILE", "")
	rec := &chatTokenServer{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	path := filepath.Join(t.TempDir(), "chat-token")
	writeChatToken(t, path, "cst_chat")
	t.Setenv("MESH_CHAT_TOKEN_FILE", path)

	c := NewRESTClientFromEnv(srv.URL)
	if _, err := c.GetAgentMe(context.Background()); err != nil {
		t.Fatalf("GetAgentMe: %v", err)
	}
	if rec.bearers[0] != "Bearer cst_chat" || rec.agentKeys[0] != "" {
		t.Errorf("got Authorization=%q X-Agent-Key=%q, want the chat token only", rec.bearers[0], rec.agentKeys[0])
	}
}

func TestChatToken_EmptyOrMissingFileIsAnError(t *testing.T) {
	rec := &chatTokenServer{}
	srv := httptest.NewServer(http.HandlerFunc(rec.handler))
	defer srv.Close()

	dir := t.TempDir()
	empty := filepath.Join(dir, "empty")
	writeChatToken(t, empty, " \n")

	for name, path := range map[string]string{"empty": empty, "missing": filepath.Join(dir, "absent")} {
		t.Run(name, func(t *testing.T) {
			c := NewChatTokenRESTClient(srv.URL, path)
			_, err := c.GetAgentMe(context.Background())
			if err == nil {
				t.Fatal("GetAgentMe succeeded, want an error")
			}
			if !strings.Contains(err.Error(), "MESH_CHAT_TOKEN_FILE") {
				t.Errorf("error %q does not name MESH_CHAT_TOKEN_FILE", err)
			}
		})
	}
	if len(rec.bearers) != 0 {
		t.Errorf("server received %d requests, want none without a token", len(rec.bearers))
	}
}

// A chat token may write only its chat's project memory, and the API refuses any
// other scope or project. remember under a chat token therefore sends project
// scope with no project_id (the API fills in the chat's project) whatever the
// model asked for; with an agent key the requested scope goes through unchanged.
func TestChatToken_RememberIsPinnedToChatProject(t *testing.T) {
	path := filepath.Join(t.TempDir(), "chat-token")
	writeChatToken(t, path, "cst_x")
	cases := []struct {
		name      string
		chat      bool
		args      map[string]any
		wantScope string
		wantProj  bool
	}{
		{"chat, workspace scope", true, map[string]any{"scope": "workspace"}, "project", false},
		{"chat, agent scope", true, map[string]any{"scope": "agent"}, "project", false},
		{"chat, foreign project", true, map[string]any{"project_id": "11111111-1111-1111-1111-111111111111"}, "project", false},
		{"agent key, workspace scope", false, map[string]any{"scope": "workspace"}, "workspace", false},
		{"agent key, project", false, map[string]any{"project_id": "11111111-1111-1111-1111-111111111111"}, "project", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server, srv, received := setupContextInjectServer(t)
			defer srv.Close()
			if tc.chat {
				server.restClient = NewChatTokenRESTClient(srv.URL, path)
			}
			args := map[string]any{"key": "k", "content": "c", "attach_context": false}
			for k, v := range tc.args {
				args[k] = v
			}
			res, err := server.handleRemember(agentCtx(uuid.New()), buildRememberRequest(args))
			if err != nil || res.IsError {
				t.Fatalf("handleRemember: %v %+v", err, res)
			}
			body := <-received
			if body["scope"] != tc.wantScope {
				t.Errorf("scope = %v, want %s", body["scope"], tc.wantScope)
			}
			if _, has := body["project_id"]; has != tc.wantProj {
				t.Errorf("project_id sent = %v (%v), want sent=%v", has, body["project_id"], tc.wantProj)
			}
		})
	}
}
