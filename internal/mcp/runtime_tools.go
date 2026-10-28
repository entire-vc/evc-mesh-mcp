package mcp

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// Runner tools act with the agent's own key on what its operator delegated
// through "Can manage My Runner" (the runtime_manager grant): the operator's
// runner settings and limits, and the runner catalog of workspaces where the
// operator is an admin. The server decides; these tools only carry the call.

// RuntimeCall sends one runner API request with the agent's key and decodes
// the answer as is: the documents are the server's, not re-shaped here.
func (c *RESTClient) RuntimeCall(ctx context.Context, method, path string, body any) (any, error) {
	var result any
	if err := c.doJSON(ctx, method, "/api/v1"+path, body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// runtimeResult returns the server's answer, and on a refusal the server's
// own status and message: a 403 names the missing grant, and rewording it
// would hide the one thing the agent has to act on.
func (s *Server) runtimeResult(ctx context.Context, method, path string, body any) (*mcpsdk.CallToolResult, error) {
	result, err := s.getRESTClient(ctx).RuntimeCall(ctx, method, path, body)
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return errResult("%d %s", apiErr.StatusCode, apiErr.Message)
	}
	if err != nil {
		return errResult("runner API call failed: %v", err)
	}
	return jsonResult(result)
}

// runtimeSaveBody builds {config, if_revision}; both are required by the API.
func runtimeSaveBody(request mcpsdk.CallToolRequest) (map[string]any, string) {
	config, ok := request.GetArguments()["config"].(map[string]any)
	if !ok {
		return nil, "config is required: the whole runner document as a JSON object"
	}
	if !hasArgument(request, "if_revision") {
		return nil, "if_revision is required: the revision you read (0 for the first save)"
	}
	return map[string]any{"config": config, "if_revision": int64(mcpsdk.ParseFloat64(request, "if_revision", 0))}, ""
}

func runtimeLimitQuery(request mcpsdk.CallToolRequest) string {
	if !hasArgument(request, "limit") {
		return ""
	}
	q := url.Values{}
	q.Set("limit", strconv.Itoa(int(mcpsdk.ParseFloat64(request, "limit", 0))))
	return "?" + q.Encode()
}

func (s *Server) handleGetOperatorRuntime(ctx context.Context, _ mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	return s.runtimeResult(ctx, http.MethodGet, "/agents/me/operator-runtime", nil)
}

func (s *Server) handleSaveOperatorRuntime(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	body, problem := runtimeSaveBody(request)
	if problem != "" {
		return errResult("%s", problem)
	}
	return s.runtimeResult(ctx, http.MethodPut, "/agents/me/operator-runtime", body)
}

func (s *Server) handleGetOperatorRuntimeHistory(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	return s.runtimeResult(ctx, http.MethodGet, "/agents/me/operator-runtime/history"+runtimeLimitQuery(request), nil)
}

func (s *Server) handleSetAgentLimits(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	agentID := mcpsdk.ParseString(request, "agent_id", "")
	if agentID == "" || !hasArgument(request, "max_concurrent_tasks") {
		return errResult("agent_id and max_concurrent_tasks are required")
	}
	body := map[string]any{"max_concurrent_tasks": int(mcpsdk.ParseFloat64(request, "max_concurrent_tasks", 0))}
	return s.runtimeResult(ctx, http.MethodPut, "/agents/me/operator-runtime/agents/"+url.PathEscape(agentID)+"/limits", body)
}

func (s *Server) handleGetWorkspaceRuntime(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	ws := mcpsdk.ParseString(request, "workspace_id", "")
	if ws == "" {
		return errResult("workspace_id is required")
	}
	return s.runtimeResult(ctx, http.MethodGet, "/workspaces/"+url.PathEscape(ws)+"/runtime", nil)
}

func (s *Server) handleGetWorkspaceRuntimeRevisions(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	ws := mcpsdk.ParseString(request, "workspace_id", "")
	if ws == "" {
		return errResult("workspace_id is required")
	}
	return s.runtimeResult(ctx, http.MethodGet, "/workspaces/"+url.PathEscape(ws)+"/runtime/revisions"+runtimeLimitQuery(request), nil)
}

func (s *Server) handleSaveWorkspaceRuntime(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	ws := mcpsdk.ParseString(request, "workspace_id", "")
	if ws == "" {
		return errResult("workspace_id is required")
	}
	body, problem := runtimeSaveBody(request)
	if problem != "" {
		return errResult("%s", problem)
	}
	return s.runtimeResult(ctx, http.MethodPut, "/workspaces/"+url.PathEscape(ws)+"/runtime", body)
}

const runtimeGrantNote = " Works with the agent's own key only while its operator keeps \"Can manage My Runner\" on; otherwise the server's 403 is returned word for word."

func (s *Server) registerRuntimeTools() {
	s.addTool(mcpsdk.NewTool("get_operator_runtime",
		mcpsdk.WithDescription("Read your operator's runner settings (My Runner): the whole document with its revision. "+
			"Pass that revision as if_revision to save_operator_runtime."+runtimeGrantNote),
	), s.tracked("get_operator_runtime", s.handleGetOperatorRuntime))

	s.addTool(mcpsdk.NewTool("save_operator_runtime",
		mcpsdk.WithDescription("Replace your operator's runner settings (My Runner) with a new document. "+
			"Optimistic: if_revision must be the revision you read, a stale one is refused with a conflict."+runtimeGrantNote),
		mcpsdk.WithObject("config", mcpsdk.Required(), mcpsdk.Description("The whole runner settings document, as read by get_operator_runtime and edited.")),
		mcpsdk.WithNumber("if_revision", mcpsdk.Required(), mcpsdk.Description("Revision the edit is based on (from get_operator_runtime; 0 for the first save).")),
	), s.tracked("save_operator_runtime", s.handleSaveOperatorRuntime))

	s.addTool(mcpsdk.NewTool("get_operator_runtime_history",
		mcpsdk.WithDescription("List earlier revisions of your operator's runner settings (My Runner), newest first, to see who changed what."+runtimeGrantNote),
		mcpsdk.WithNumber("limit", mcpsdk.Description("How many revisions to return (server default when omitted).")),
	), s.tracked("get_operator_runtime_history", s.handleGetOperatorRuntimeHistory))

	s.addTool(mcpsdk.NewTool("set_agent_limits",
		mcpsdk.WithDescription("Set how many tasks one of your operator's agents may run at once on the runner (max_concurrent_tasks)."+runtimeGrantNote),
		mcpsdk.WithString("agent_id", mcpsdk.Required(), mcpsdk.Description("UUID of the operator's agent whose limit changes.")),
		mcpsdk.WithNumber("max_concurrent_tasks", mcpsdk.Required(), mcpsdk.Description("Maximum tasks the agent runs in parallel.")),
	), s.tracked("set_agent_limits", s.handleSetAgentLimits))

	s.addTool(mcpsdk.NewTool("get_workspace_runtime",
		mcpsdk.WithDescription("Read a workspace's runner catalog (runners, pools, bindings) with its revision. "+
			"Allowed when your operator is an admin of that workspace and you are connected to it."+runtimeGrantNote),
		mcpsdk.WithString("workspace_id", mcpsdk.Required(), mcpsdk.Description("Workspace UUID whose runner catalog to read.")),
	), s.tracked("get_workspace_runtime", s.handleGetWorkspaceRuntime))

	s.addTool(mcpsdk.NewTool("get_workspace_runtime_revisions",
		mcpsdk.WithDescription("List stored revisions of a workspace's runner catalog, newest first. "+
			"Same access as get_workspace_runtime."+runtimeGrantNote),
		mcpsdk.WithString("workspace_id", mcpsdk.Required(), mcpsdk.Description("Workspace UUID whose catalog history to read.")),
		mcpsdk.WithNumber("limit", mcpsdk.Description("How many revisions to return (default 50, max 200).")),
	), s.tracked("get_workspace_runtime_revisions", s.handleGetWorkspaceRuntimeRevisions))

	s.addTool(mcpsdk.NewTool("save_workspace_runtime",
		mcpsdk.WithDescription("Replace a workspace's runner catalog with a new document; the server stores it as the next revision. "+
			"Optimistic: if_revision must be the revision you read. Same access as get_workspace_runtime."+runtimeGrantNote),
		mcpsdk.WithString("workspace_id", mcpsdk.Required(), mcpsdk.Description("Workspace UUID whose runner catalog to replace.")),
		mcpsdk.WithObject("config", mcpsdk.Required(), mcpsdk.Description("The whole runner catalog, as read by get_workspace_runtime and edited.")),
		mcpsdk.WithNumber("if_revision", mcpsdk.Required(), mcpsdk.Description("Revision the edit is based on (from get_workspace_runtime).")),
	), s.tracked("save_workspace_runtime", s.handleSaveWorkspaceRuntime))
}
