package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// heartbeatHarness stands up a fake API that always 200s the heartbeat POST.
func heartbeatHarness(t *testing.T) (*Server, func()) {
	t.Helper()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/agents/heartbeat" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{}`))
	}))

	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
	return server, srv.Close
}

// #8f441c40: the only way to tell "feature not deployed" apart from "feature
// absent" used to be `strings` on the installed binary. heartbeat must carry
// the build's git SHA so an agent session can check this without a shell.
func TestHandleHeartbeat_ReportsMeshVersion(t *testing.T) {
	server, closeSrv := heartbeatHarness(t)
	defer closeSrv()

	originalSHA := BuildSHA
	BuildSHA = "abc1234"
	defer func() { BuildSHA = originalSHA }()

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{}
	result, err := server.handleHeartbeat(context.Background(), req)
	if err != nil {
		t.Fatalf("handleHeartbeat returned error: %v", err)
	}
	if result.IsError {
		t.Fatalf("tool returned an error result: %v", result.Content)
	}

	text, ok := result.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("content[0] is not TextContent, got %T", result.Content[0])
	}
	if !strings.Contains(text.Text, `"mesh_version":"abc1234"`) {
		t.Errorf("heartbeat response %q does not contain mesh_version=abc1234", text.Text)
	}
}

// The unpinned default must be visibly different from a real SHA, or a
// forgotten -ldflags build looks identical to a genuinely pinned one.
func TestBuildSHA_DefaultsToDevWhenUnset(t *testing.T) {
	// This only holds if nothing else in the package mutated the package-level
	// var and forgot to restore it — a canary for test pollution across files.
	if BuildSHA == "" {
		t.Error("BuildSHA is empty — should default to \"dev\", never a blank string")
	}
}
