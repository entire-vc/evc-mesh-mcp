package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

const noGrantMessage = "agent has no runtime_manager grant from its operator (Can manage My Runner is off)"

type runtimeCall struct {
	method, path, query string
	body                map[string]any
}

// runtimeHarness stands in for the runner API. With denied=true every route
// answers the server's real no-grant 403, so the test sees what an agent
// without "Can manage My Runner" sees.
func runtimeHarness(t *testing.T, denied bool) (*Server, *[]runtimeCall) {
	t.Helper()
	var calls []runtimeCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		call := runtimeCall{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery}
		_ = json.NewDecoder(r.Body).Decode(&call.body)
		calls = append(calls, call)
		if denied {
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": noGrantMessage})
			return
		}
		switch {
		case strings.HasSuffix(r.URL.Path, "/history"), strings.HasSuffix(r.URL.Path, "/revisions"):
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"revision": 3}, map[string]any{"revision": 2}})
		case strings.HasSuffix(r.URL.Path, "/limits"):
			_ = json.NewEncoder(w).Encode(map[string]any{"agent_id": "a", "max_concurrent_tasks": call.body["max_concurrent_tasks"]})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"revision": 3, "config": map[string]any{"runners": []any{"mini"}}})
		}
	}))
	t.Cleanup(srv.Close)
	return &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}, &calls
}

func callRuntimeTool(t *testing.T, h func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error), args map[string]any) (bool, string) {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	txt, _ := res.Content[0].(mcpsdk.TextContent)
	return res.IsError, txt.Text
}

func TestRuntimeTools_CallTheRunnerAPI(t *testing.T) {
	ws, agent := uuid.New().String(), uuid.New().String()
	cfg := map[string]any{"runners": []any{"mini"}}
	cases := []struct {
		name    string
		handler func(*Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error)
		args    map[string]any
		want    runtimeCall
	}{
		{"get_operator_runtime", func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return s.handleGetOperatorRuntime
		}, nil, runtimeCall{method: "GET", path: "/api/v1/agents/me/operator-runtime"}},
		{"save_operator_runtime", func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return s.handleSaveOperatorRuntime
		}, map[string]any{"config": cfg, "if_revision": float64(3)}, runtimeCall{method: "PUT", path: "/api/v1/agents/me/operator-runtime",
			body: map[string]any{"config": cfg, "if_revision": float64(3)}}},
		{"get_operator_runtime_history", func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return s.handleGetOperatorRuntimeHistory
		}, map[string]any{"limit": float64(5)}, runtimeCall{method: "GET", path: "/api/v1/agents/me/operator-runtime/history", query: "limit=5"}},
		{"set_agent_limits", func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return s.handleSetAgentLimits
		}, map[string]any{"agent_id": agent, "max_concurrent_tasks": float64(2)}, runtimeCall{method: "PUT",
			path: "/api/v1/agents/me/operator-runtime/agents/" + agent + "/limits", body: map[string]any{"max_concurrent_tasks": float64(2)}}},
		{"get_workspace_runtime", func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return s.handleGetWorkspaceRuntime
		}, map[string]any{"workspace_id": ws}, runtimeCall{method: "GET", path: "/api/v1/workspaces/" + ws + "/runtime"}},
		{"get_workspace_runtime_revisions", func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return s.handleGetWorkspaceRuntimeRevisions
		}, map[string]any{"workspace_id": ws, "limit": float64(10)}, runtimeCall{method: "GET", path: "/api/v1/workspaces/" + ws + "/runtime/revisions", query: "limit=10"}},
		{"save_workspace_runtime", func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return s.handleSaveWorkspaceRuntime
		}, map[string]any{"workspace_id": ws, "config": cfg, "if_revision": float64(7)}, runtimeCall{method: "PUT", path: "/api/v1/workspaces/" + ws + "/runtime",
			body: map[string]any{"config": cfg, "if_revision": float64(7)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name+"/ok", func(t *testing.T) {
			s, calls := runtimeHarness(t, false)
			isErr, text := callRuntimeTool(t, tc.handler(s), tc.args)
			if isErr {
				t.Fatalf("tool failed: %s", text)
			}
			if len(*calls) != 1 {
				t.Fatalf("want one API call, got %d", len(*calls))
			}
			got := (*calls)[0]
			if got.method != tc.want.method || got.path != tc.want.path || got.query != tc.want.query {
				t.Fatalf("called %s %s?%s, want %s %s?%s", got.method, got.path, got.query, tc.want.method, tc.want.path, tc.want.query)
			}
			gb, _ := json.Marshal(got.body)
			wb, _ := json.Marshal(tc.want.body)
			if string(gb) != string(wb) {
				t.Fatalf("body %s, want %s", gb, wb)
			}
			if !strings.Contains(text, "revision") && !strings.Contains(text, "max_concurrent_tasks") {
				t.Fatalf("server payload not passed through: %s", text)
			}
		})
		t.Run(tc.name+"/403_verbatim", func(t *testing.T) {
			s, _ := runtimeHarness(t, true)
			isErr, text := callRuntimeTool(t, tc.handler(s), tc.args)
			if !isErr {
				t.Fatalf("403 reported as success: %s", text)
			}
			if !strings.Contains(text, noGrantMessage) || !strings.Contains(text, "403") {
				t.Fatalf("403 not passed through verbatim: %q", text)
			}
		})
	}
}

func TestRuntimeTools_RequiredArguments(t *testing.T) {
	s, calls := runtimeHarness(t, false)
	for name, c := range map[string]struct {
		h    func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error)
		args map[string]any
	}{
		"save_operator_runtime without config":  {s.handleSaveOperatorRuntime, map[string]any{"if_revision": float64(1)}},
		"set_agent_limits without agent_id":     {s.handleSetAgentLimits, map[string]any{"max_concurrent_tasks": float64(1)}},
		"set_agent_limits without max":          {s.handleSetAgentLimits, map[string]any{"agent_id": uuid.New().String()}},
		"get_workspace_runtime without ws":      {s.handleGetWorkspaceRuntime, nil},
		"save_workspace_runtime without config": {s.handleSaveWorkspaceRuntime, map[string]any{"workspace_id": uuid.New().String()}},
		"get_workspace_runtime_revisions no ws": {s.handleGetWorkspaceRuntimeRevisions, nil},
	} {
		if isErr, text := callRuntimeTool(t, c.h, c.args); !isErr {
			t.Errorf("%s: accepted: %s", name, text)
		}
	}
	if len(*calls) != 0 {
		t.Fatalf("invalid calls reached the API: %d", len(*calls))
	}
}
