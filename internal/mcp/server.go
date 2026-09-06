// Package mcp contains MCP (Model Context Protocol) tool handlers.
// These tools provide an agent-friendly interface for task management,
// event bus interaction, and artifact handling.
//
// Architecture: tools call the REST API via RESTClient instead of
// accessing the database directly. This decouples the MCP server from
// the data layer and allows the MCP server to run as a lightweight proxy.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// Profile constants for MCP server tool sets.
const (
	// ProfileCore registers only the 25 essential tools for lightweight agents.
	ProfileCore = "core"
	// ProfileFull registers all tools (core + advanced). This is the default.
	ProfileFull = "full"
)

// AgentSession holds the authenticated agent context for the MCP session.
type AgentSession struct {
	AgentID     uuid.UUID
	WorkspaceID uuid.UUID
	AgentName   string
	AgentType   string
}

// agentSessionKey is the context key for storing AgentSession in context.
type agentSessionKey struct{}

// restClientKey is the context key for storing a per-request RESTClient in context (SSE mode).
type restClientKey struct{}

// ContextWithSession returns a new context with the given AgentSession attached.
func ContextWithSession(ctx context.Context, session *AgentSession) context.Context {
	return context.WithValue(ctx, agentSessionKey{}, session)
}

// SessionFromContext retrieves the AgentSession from the context, or nil if not present.
func SessionFromContext(ctx context.Context) *AgentSession {
	if session, ok := ctx.Value(agentSessionKey{}).(*AgentSession); ok {
		return session
	}
	return nil
}

// ContextWithRESTClient returns a new context with the given RESTClient attached.
// Used in SSE mode to inject per-connection REST clients.
func ContextWithRESTClient(ctx context.Context, client *RESTClient) context.Context {
	return context.WithValue(ctx, restClientKey{}, client)
}

// RESTClientFromContext retrieves a per-request RESTClient from context.
// Returns nil if none was injected (stdio mode uses the server's shared client).
func RESTClientFromContext(ctx context.Context) *RESTClient {
	if client, ok := ctx.Value(restClientKey{}).(*RESTClient); ok {
		return client
	}
	return nil
}

// NewAgentSession creates an AgentSession by parsing UUID strings from the REST API response.
// Returns an error if any UUID is malformed.
func NewAgentSession(agentID, workspaceID, agentName, agentType string) (AgentSession, error) {
	aID, err := uuid.Parse(agentID)
	if err != nil {
		return AgentSession{}, fmt.Errorf("invalid agent_id %q: %w", agentID, err)
	}
	wsID, err := uuid.Parse(workspaceID)
	if err != nil {
		return AgentSession{}, fmt.Errorf("invalid workspace_id %q: %w", workspaceID, err)
	}
	return AgentSession{
		AgentID:     aID,
		WorkspaceID: wsID,
		AgentName:   agentName,
		AgentType:   agentType,
	}, nil
}

// Server wraps an mcp-go MCPServer with a REST API client.
type Server struct {
	mcpServer   *mcpserver.MCPServer
	session     *AgentSession // static session for stdio mode; nil for SSE mode
	restClient  *RESTClient   // default REST client; may be overridden per-request in SSE mode
	tracker     *SessionTracker
	ReadCounter *ReadCounter // per-agent/per-tool read counter; exported for main.go endpoint
	profile     string
	// checkouts stores checkout_token keyed by task_id for graceful release.
	// Populated by handleCheckoutTask; consumed and cleared by handleReleaseTask.
	checkouts sync.Map
	// activeProjects maps agentID (uuid.UUID) to the project_id of the most recently
	// checked-out task. Used by handleRemember to auto-populate project_id when absent.
	activeProjects sync.Map
	// activeTaskIDs maps agentID (uuid.UUID) to the task_id of the most recently
	// checked-out task. Used by handleRemember to auto-populate source_task_id when
	// absent, enabling Amendment 2/3 edge hooks (thread + task-graph bridges).
	activeTaskIDs sync.Map
}

// getSession returns the AgentSession for the current request.
// It first checks the context (for SSE per-connection sessions),
// then falls back to the static session (for stdio mode).
func (s *Server) getSession(ctx context.Context) *AgentSession {
	if session := SessionFromContext(ctx); session != nil {
		return session
	}
	return s.session
}

// getRESTClient returns the REST client for the current request.
// In SSE mode, a per-connection client may be stored in context.
// Falls back to the shared client for stdio mode.
func (s *Server) getRESTClient(ctx context.Context) *RESTClient {
	if client := RESTClientFromContext(ctx); client != nil {
		return client
	}
	return s.restClient
}

// ServerConfig holds all configuration needed to build the MCP server.
type ServerConfig struct {
	// Session is the static agent session for stdio mode. Nil for SSE mode.
	Session *AgentSession
	// RESTClient is the HTTP client used to call the Mesh REST API.
	RESTClient *RESTClient
	// Profile controls which tools are registered: "core" (25 essential tools)
	// or "full" (all tools, default).
	Profile string
}

// NewServer creates a new MCP server with tools registered according to the profile.
func NewServer(cfg ServerConfig) *Server {
	profile := cfg.Profile
	if profile == "" {
		profile = ProfileFull
	}

	serverName := "evc-mesh-mcp"
	if profile == ProfileCore {
		serverName = "evc-mesh-mcp-core"
	}

	s := &Server{
		mcpServer:   mcpserver.NewMCPServer(serverName, "0.1.0"),
		session:     cfg.Session,
		restClient:  cfg.RESTClient,
		tracker:     NewSessionTracker(),
		ReadCounter: NewReadCounter(),
		profile:     profile,
	}

	s.registerCoreTools()
	if profile == ProfileFull {
		s.registerAdvancedTools()
	}
	return s
}

// tracked wraps a tool handler to record the tool call in the session tracker.
func (s *Server) tracked(name string, handler func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error)) func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
		if s.tracker != nil {
			s.tracker.RecordToolCall(name)
		}
		return handler(ctx, req)
	}
}

// recordMemoryRead increments the ReadCounter for the calling agent and tool.
// Call this after a successful memory-read tool invocation.
func (s *Server) recordMemoryRead(ctx context.Context, toolName string) {
	if s.ReadCounter == nil {
		return
	}
	session := s.getSession(ctx)
	agentID := "unknown"
	if session != nil {
		agentID = session.AgentID.String()
	}
	s.ReadCounter.Inc(agentID, toolName)
}

// contextWarning returns a non-empty warning string when the agent has not yet
// loaded project context (via get_project_knowledge, recall, or get_context)
// in the current session. Returns an empty string once context has been loaded.
func (s *Server) contextWarning() string {
	if s.tracker == nil {
		return ""
	}
	_, detail := s.tracker.ComplianceScore()
	if !detail["called_get_project_knowledge"] && !detail["called_get_context"] {
		return "Warning: You haven't loaded project context. Call get_project_knowledge() or get_context() first to avoid conflicting with existing decisions."
	}
	return ""
}

// MCPServer returns the underlying mcp-go server for use with transports.
func (s *Server) MCPServer() *mcpserver.MCPServer {
	return s.mcpServer
}

// registerCoreTools registers the 25 essential tools with optimized, directive descriptions.
func (s *Server) registerCoreTools() {
	// --- ACP / Session tools ---
	s.mcpServer.AddTool(mcpsdk.NewTool("heartbeat",
		mcpsdk.WithDescription("Send heartbeat to stay visible. Call at session START with status=online, periodically during work with status=busy. Reports current_task_id, message, and metadata."),
		mcpsdk.WithString("current_task_id", mcpsdk.Description("ID of the task currently being worked on.")),
		mcpsdk.WithString("status", mcpsdk.Description("Agent status: online, busy, error.")),
		mcpsdk.WithString("message", mcpsdk.Description("Short human-readable status message (e.g. 'running tests', 'waiting for review').")),
		mcpsdk.WithObject("metadata", mcpsdk.Description("Arbitrary JSON metadata to store with the heartbeat.")),
	), s.tracked("heartbeat", s.handleHeartbeat))

	s.mcpServer.AddTool(mcpsdk.NewTool("get_project_knowledge",
		mcpsdk.WithDescription("Get ALL PERMANENT KNOWLEDGE for a project: decisions, conventions, accumulated context. Call at session start (ACP Step 2). Returns workspace-level + project-level memories. For RECENT events, use get_context instead."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project UUID.")),
		mcpsdk.WithNumber("limit", mcpsdk.Description("Max workspace-tier memories (default 100, max 500).")),
		mcpsdk.WithNumber("offset", mcpsdk.Description("Pagination offset for workspace-tier (default 0).")),
		mcpsdk.WithNumber("min_importance", mcpsdk.Description("Minimum importance_score for workspace-tier (default 0 = all).")),
		mcpsdk.WithString("tags_any", mcpsdk.Description("Comma-separated tag OR-filter for workspace-tier, e.g. 'kind:decision,kind:incident'.")),
	), s.tracked("get_project_knowledge", s.handleGetProjectKnowledge))

	s.mcpServer.AddTool(mcpsdk.NewTool("get_my_rules",
		mcpsdk.WithDescription("Get ALL governance rules that apply to you: workflow constraints, assignment policies, behavioral requirements. Includes workspace and project-level rules with source annotations. Call at session start (ACP Step 3)."),
		mcpsdk.WithString("project_id", mcpsdk.Description("Optional project ID to get project-specific effective rules.")),
	), s.tracked("get_my_rules", s.handleGetMyRules))

	s.mcpServer.AddTool(mcpsdk.NewTool("get_context",
		mcpsdk.WithDescription("Get RECENT ACTIVITY for a project (last 24h by default): event stream with summaries, decisions, errors, plus accumulated project knowledge. Use for ACP Step 4 — what happened recently. For searching specific knowledge, use recall."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project ID.")),
		mcpsdk.WithString("since", mcpsdk.Description("Only events after this timestamp (RFC3339).")),
		mcpsdk.WithArray("event_types", mcpsdk.Description("Filter by event types."), mcpsdk.WithStringItems()),
		mcpsdk.WithArray("tags", mcpsdk.Description("Filter by tags."), mcpsdk.WithStringItems()),
		mcpsdk.WithNumber("limit", mcpsdk.Description("Max events to return (default 50).")),
	), s.tracked("get_context", s.handleGetContext))

	s.mcpServer.AddTool(mcpsdk.NewTool("get_my_tasks",
		mcpsdk.WithDescription("Get YOUR assigned tasks (ACP Step 5). Filter by status_category to focus on active work. Use at session start and after completing tasks to pick up the next assignment."),
		mcpsdk.WithString("status_category", mcpsdk.Description("Filter by status category: backlog, todo, in_progress, review, done, cancelled.")),
		mcpsdk.WithString("project_id", mcpsdk.Description("Filter by project.")),
		mcpsdk.WithNumber("limit", mcpsdk.Description("Max results (default 50).")),
	), s.tracked("get_my_tasks", s.handleGetMyTasks))

	// --- Task CRUD ---
	s.mcpServer.AddTool(mcpsdk.NewTool("list_tasks",
		mcpsdk.WithDescription("List tasks with filters. Provide project_id for project-scoped listing or workspace_id for global search across all projects (requires search parameter)."),
		mcpsdk.WithString("project_id", mcpsdk.Description("Project ID (required unless workspace_id is provided).")),
		mcpsdk.WithString("workspace_id", mcpsdk.Description("Workspace ID for global cross-project search (requires search parameter).")),
		mcpsdk.WithString("status_category", mcpsdk.Description("Filter by status category: backlog, todo, in_progress, review, done, cancelled.")),
		mcpsdk.WithString("assignee_type", mcpsdk.Description("Filter by assignee type: user, agent, unassigned.")),
		mcpsdk.WithString("priority", mcpsdk.Description("Filter by priority: urgent, high, medium, low, none.")),
		mcpsdk.WithArray("labels", mcpsdk.Description("Filter by labels."), mcpsdk.WithStringItems()),
		mcpsdk.WithString("search", mcpsdk.Description("Search in title and description.")),
		mcpsdk.WithNumber("limit", mcpsdk.Description("Max results to return (default 50, max 200).")),
		mcpsdk.WithString("sort", mcpsdk.Description("Sort field: created_at, updated_at, priority, due_date.")),
		mcpsdk.WithString("order", mcpsdk.Description("Sort direction: asc (default) or desc. Without this, a project larger than `limit` returns its OLDEST tasks, so \"what changed recently\" walks come back empty and look clean. An invalid value is REFUSED by the API, not silently treated as asc.")),
		mcpsdk.WithNumber("page", mcpsdk.Description("1-based page number (default 1). The response reports total_pages; without this parameter every page beyond the first was unreachable while the envelope kept advertising them.")),
		mcpsdk.WithNumber("list_revision", mcpsdk.Description("The list_revision echoed back on a previous page of this same project-scoped walk (see the response's list_revision field). Pass it back to continue that walk. If the project's tasks changed since that page was issued, the call is REFUSED with list_revision_stale (HTTP 410) instead of silently returning an inconsistent page — restart pagination from page 1 (omit this field) on that error. Omit on the first page of a fresh walk. Ignored for workspace_id search.")),
	), s.tracked("list_tasks", s.handleListTasks))

	s.mcpServer.AddTool(mcpsdk.NewTool("get_task",
		mcpsdk.WithDescription("Get full task details with optional comments, artifacts, dependencies, and VCS links."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID (full UUID or 6–12 char hex short-ID prefix).")),
		mcpsdk.WithBoolean("include_comments", mcpsdk.Description("Include comments."), mcpsdk.DefaultBool(false)),
		mcpsdk.WithBoolean("include_artifacts", mcpsdk.Description("Include artifacts."), mcpsdk.DefaultBool(false)),
		mcpsdk.WithBoolean("include_dependencies", mcpsdk.Description("Include dependencies."), mcpsdk.DefaultBool(false)),
		mcpsdk.WithBoolean("include_vcs_links", mcpsdk.Description("Include linked PRs/MRs/commits/branches (id, provider, link_type, external_id, url, status, created_at) — use this instead of a raw REST call to diagnose a misclassified or stuck-status link."), mcpsdk.DefaultBool(false)),
	), s.tracked("get_task", s.handleGetTask))

	s.mcpServer.AddTool(mcpsdk.NewTool("create_task",
		mcpsdk.WithDescription("Create a new task. Check get_my_tasks and list_tasks FIRST to avoid duplicates. Set status_slug for initial status (defaults to project's first status)."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project ID.")),
		mcpsdk.WithString("title", mcpsdk.Required(), mcpsdk.Description("Task title.")),
		mcpsdk.WithString("description", mcpsdk.Description("Task description.")),
		mcpsdk.WithString("status_slug", mcpsdk.Description("Status slug (e.g. 'todo'). Uses project default if omitted.")),
		mcpsdk.WithString("priority", mcpsdk.Description("Priority: urgent, high, medium, low, none."), mcpsdk.DefaultString("medium")),
		mcpsdk.WithString("assignee_id", mcpsdk.Description("Assignee ID (user or agent UUID).")),
		mcpsdk.WithString("assignee_type", mcpsdk.Description("Assignee type: user, agent."), mcpsdk.DefaultString("unassigned")),
		mcpsdk.WithArray("labels", mcpsdk.Description("Task labels."), mcpsdk.WithStringItems()),
		mcpsdk.WithObject("custom_fields", mcpsdk.Description("Custom field values as key-value pairs.")),
		mcpsdk.WithString("parent_task_id", mcpsdk.Description("Parent task ID for subtask.")),
		mcpsdk.WithString("due_date", mcpsdk.Description("Due date in RFC3339 format.")),
		mcpsdk.WithNumber("estimated_hours", mcpsdk.Description("Estimated hours for the task.")),
		mcpsdk.WithString("delegation_level", mcpsdk.Description("Delegation level: auto, review, supervised.")),
	), s.tracked("create_task", s.handleCreateTask))

	s.mcpServer.AddTool(mcpsdk.NewTool("update_task",
		mcpsdk.WithDescription("Update task fields."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID.")),
		mcpsdk.WithString("title", mcpsdk.Description("New title.")),
		mcpsdk.WithString("description", mcpsdk.Description("New description.")),
		mcpsdk.WithString("priority", mcpsdk.Description("New priority.")),
		mcpsdk.WithArray("labels", mcpsdk.Description("New labels."), mcpsdk.WithStringItems()),
		mcpsdk.WithObject("custom_fields", mcpsdk.Description("Custom field values to update.")),
		mcpsdk.WithString("due_date", mcpsdk.Description("Due date in RFC3339 format.")),
		mcpsdk.WithNumber("estimated_hours", mcpsdk.Description("Estimated hours.")),
		mcpsdk.WithString("delegation_level", mcpsdk.Description("Routing after work: auto, review, or supervised.")),
		mcpsdk.WithBoolean("completion_signal", mcpsdk.Description("Mark agent-side work as finished.")),
	), s.tracked("update_task", s.handleUpdateTask))

	s.mcpServer.AddTool(mcpsdk.NewTool("move_task",
		mcpsdk.WithDescription("Change task status (e.g. todo → in_progress → done). Use status SLUGS (not UUIDs). On move to 'review', task auto-reassigns to creator unless assignee_id is provided."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID.")),
		mcpsdk.WithString("status_slug", mcpsdk.Required(), mcpsdk.Description("Target status slug (e.g. 'in_progress', 'done').")),
		mcpsdk.WithString("comment", mcpsdk.Description("Optional comment to add when moving.")),
		mcpsdk.WithString("assignee_id", mcpsdk.Description("Reassign to this agent/user on move. Overrides auto-reassign to creator on review.")),
		mcpsdk.WithString("assignee_type", mcpsdk.Description("Assignee type if assignee_id is set: user or agent."), mcpsdk.DefaultString("agent")),
	), s.tracked("move_task", s.handleMoveTask))

	s.mcpServer.AddTool(mcpsdk.NewTool("assign_task",
		mcpsdk.WithDescription("Assign a task to a user or agent."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID.")),
		mcpsdk.WithString("assignee_id", mcpsdk.Description("Assignee UUID. Omit to unassign.")),
		mcpsdk.WithString("assignee_type", mcpsdk.Description("Assignee type: user, agent."), mcpsdk.DefaultString("agent")),
		mcpsdk.WithBoolean("assign_to_self", mcpsdk.Description("Assign to the calling agent."), mcpsdk.DefaultBool(false)),
	), s.tracked("assign_task", s.handleAssignTask))

	s.mcpServer.AddTool(mcpsdk.NewTool("get_task_context",
		mcpsdk.WithDescription("Get EVERYTHING about ONE TASK in a single call: full details + comments + artifacts + dependencies + activity. Use when working on a specific task instead of calling get_task + list_comments + list_artifacts separately."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID.")),
	), s.tracked("get_task_context", s.handleGetTaskContext))

	// --- Communication ---
	s.mcpServer.AddTool(mcpsdk.NewTool("add_comment",
		mcpsdk.WithDescription("Add a comment to a task."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID.")),
		mcpsdk.WithString("body", mcpsdk.Required(), mcpsdk.Description("Comment body (markdown supported).")),
		mcpsdk.WithBoolean("is_internal", mcpsdk.Description("Mark as internal (agent-only visible)."), mcpsdk.DefaultBool(false)),
		mcpsdk.WithString("parent_comment_id", mcpsdk.Description("Parent comment ID for threading.")),
		mcpsdk.WithObject("metadata", mcpsdk.Description("Additional metadata as key-value pairs.")),
	), s.tracked("add_comment", s.handleAddComment))

	s.mcpServer.AddTool(mcpsdk.NewTool("add_vcs_link",
		mcpsdk.WithDescription("Link a task to a pull request, commit, or branch. This is what makes the task↔PR join real: a task with no VCS link cannot be matched to the code that implements it, so PR-driven status automation and any 'what shipped for this task?' report simply will not see it. Call it as soon as the PR exists. Only task_id and url are needed — provider, link_type and external_id are inferred from a GitHub or GitLab URL. If the PR is ALREADY merged (or closed) by the time you call this — e.g. you finished, merged, and are linking retroactively — pass status='merged' (or 'closed'). Without it the link starts as 'open' and the done-evidence gate will block move→done on it forever: no GitHub webhook fires for a merge that happened before the link existed."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID.")),
		mcpsdk.WithString("url", mcpsdk.Required(), mcpsdk.Description("Link URL, e.g. https://github.com/owner/repo/pull/123.")),
		mcpsdk.WithString("provider", mcpsdk.Description("VCS provider: github, gitlab. Inferred from the URL host; defaults to github.")),
		mcpsdk.WithString("link_type", mcpsdk.Description("What the URL points at: pr (alias: pull_request), commit, branch. Inferred from the URL path; defaults to pr.")),
		mcpsdk.WithString("external_id", mcpsdk.Description("PR number, commit SHA, or branch name. Inferred from the URL; only needed when the URL is not a recognised PR/commit/branch link.")),
		mcpsdk.WithString("title", mcpsdk.Description("Human-readable label, e.g. the PR title.")),
		mcpsdk.WithString("status", mcpsdk.Description("PR status, if you already know it: open, merged, closed. Pass 'merged' when linking a PR that was merged before this call — that is the one case a webhook can never backfill. Omit it to let the link start as 'open' (the safe default when the PR is still active). Calling add_vcs_link again on the same PR with a status update is safe — it corrects the existing link rather than failing.")),
	), s.tracked("add_vcs_link", s.handleAddVCSLink))

	s.mcpServer.AddTool(mcpsdk.NewTool("publish_event",
		mcpsdk.WithDescription("Publish an event to the event bus. For summaries, use event_type='summary'. Add memory={persist:true, key:'decision-name'} to also save as permanent memory. Replaces the deprecated publish_summary tool."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project ID.")),
		mcpsdk.WithString("event_type", mcpsdk.Required(), mcpsdk.Description("Event type: summary, status_change, context_update, error, dependency_resolved, custom.")),
		mcpsdk.WithString("subject", mcpsdk.Required(), mcpsdk.Description("Event subject line.")),
		mcpsdk.WithObject("payload", mcpsdk.Required(), mcpsdk.Description("Event payload as key-value pairs.")),
		mcpsdk.WithString("task_id", mcpsdk.Description("Related task ID.")),
		mcpsdk.WithArray("tags", mcpsdk.Description("Event tags for filtering."), mcpsdk.WithStringItems()),
		mcpsdk.WithNumber("ttl_hours", mcpsdk.Description("Time-to-live in hours (default 24).")),
		mcpsdk.WithObject("memory", mcpsdk.Description("Optional memory hint to persist alongside the event (e.g. key decisions, conventions).")),
	), s.tracked("publish_event", s.handlePublishEvent))

	// --- Memory ---
	s.mcpServer.AddTool(mcpsdk.NewTool("recall",
		mcpsdk.WithDescription("SEARCH memory by keywords. Use to find a SPECIFIC piece of knowledge, e.g. 'API convention' or 'license decision'. Returns ranked results with scores. For loading ALL project knowledge at session start, use get_project_knowledge instead. Set include_archived=true to retrieve archived memories."),
		mcpsdk.WithString("query", mcpsdk.Required(), mcpsdk.Description("Full-text search query.")),
		mcpsdk.WithString("project_id", mcpsdk.Description("Filter to a specific project.")),
		mcpsdk.WithString("scope", mcpsdk.Description("Filter by scope: workspace, project, agent, or all (default).")),
		mcpsdk.WithArray("tags", mcpsdk.Description("AND-filter: memory must contain ALL listed tags."), mcpsdk.WithStringItems()),
		mcpsdk.WithArray("tags_any", mcpsdk.Description("OR-filter: memory must contain AT LEAST ONE of these tags."), mcpsdk.WithStringItems()),
		mcpsdk.WithString("created_by", mcpsdk.Description("Filter by agent ID (UUID).")),
		mcpsdk.WithString("since", mcpsdk.Description("Return memories created at or after this RFC3339 timestamp.")),
		mcpsdk.WithString("until", mcpsdk.Description("Return memories created at or before this RFC3339 timestamp.")),
		mcpsdk.WithNumber("relevance_min", mcpsdk.Description("Minimum relevance score (0-1).")),
		mcpsdk.WithNumber("min_importance", mcpsdk.Description("Minimum importance_score threshold (0-1, default 0.4). Entries below this are excluded (e.g. kind:session-checkpoint = 0.3). Pass 0 to disable filtering and retrieve all entries including low-importance ones.")),
		mcpsdk.WithBoolean("apply_recency_decay", mcpsdk.Description("Sort by relevance * 0.95^days_since_created."), mcpsdk.DefaultBool(false)),
		mcpsdk.WithString("order_by", mcpsdk.Description("Sort order: created_at:desc (default), created_at:asc, relevance:desc, decayed_relevance:desc.")),
		mcpsdk.WithBoolean("include_expired", mcpsdk.Description("Include expired memories (default false)."), mcpsdk.DefaultBool(false)),
		mcpsdk.WithBoolean("include_archived", mcpsdk.Description("Include archived memories in results (default false)."), mcpsdk.DefaultBool(false)),
		mcpsdk.WithNumber("limit", mcpsdk.Description("Max results (default 10, max 50). This is a hard bound: the response never contains more than limit items. When knowledge-graph boost is enabled, a share of the page (limit/4, at least 1 when limit>=2) may be filled with graph-expanded neighbours, marked graph_boost=true and provenance=via:graph — they take the tail slots instead of being added on top. Rows that fail scope/tags are dropped, never returned unmarked, whether they arrived by retrieval, by pinning, or by graph expansion.")),
		mcpsdk.WithNumber("offset", mcpsdk.Description("Pagination offset (default 0).")),
	), s.tracked("recall", s.handleRecall))

	s.mcpServer.AddTool(mcpsdk.NewTool("recall_with_graph",
		mcpsdk.WithDescription("Search memory with Knowledge Graph expansion. Seeds from hybrid recall, then BFS-traverses memory_edges up to hops depth. Returns memories ranked by composite score with hop_distance and provenance fields. Use when you want broader context — related decisions, connected incidents, derived learnings."),
		mcpsdk.WithString("q", mcpsdk.Required(), mcpsdk.Description("Search query (keywords or natural language).")),
		mcpsdk.WithString("project_id", mcpsdk.Description("Filter to a specific project.")),
		mcpsdk.WithString("task_id", mcpsdk.Description("Optional task ID — used as cache key discriminator for session-scoped traversal.")),
		mcpsdk.WithNumber("hops", mcpsdk.Description("Graph traversal depth (default 2, max 5).")),
		mcpsdk.WithNumber("weight_threshold", mcpsdk.Description("Minimum edge weight to follow (default 0.3).")),
	), s.tracked("recall_with_graph", s.handleRecallWithGraph))

	s.mcpServer.AddTool(mcpsdk.NewTool("remember",
		mcpsdk.WithDescription("Save knowledge to persistent memory. Use for decisions, conventions, preferences. UPSERT by key — calling with same key updates the existing entry. "+
			"Content is screened on write and REFUSED with a named reason (never silently stripped or stored) if it contains invisible/bidi characters, an LLM role tag, an instruction to ignore previous/system instructions, a PEM private key, a prefixed API token (sk-/ghp_/xox*/AKIA), or a literal assignment to a *_PASSWORD/_SECRET/_TOKEN/_API_KEY name. "+
			"LIMITATION — this screen is partial and must not be relied on as a secret filter: it CANNOT see a secret that has no recognisable prefix and no field name next to it (a bare value pasted on its own line), nor names it does not know. Do not paste credentials here on the assumption they will be caught; record where a secret lives, never its value."),
		mcpsdk.WithString("key", mcpsdk.Required(), mcpsdk.Description("Slug key for UPSERT (e.g. 'api-convention', 'license-decision').")),
		mcpsdk.WithString("content", mcpsdk.Required(), mcpsdk.Description("What to remember (markdown).")),
		mcpsdk.WithString("scope", mcpsdk.Description("workspace | project | agent (default: project).")),
		mcpsdk.WithString("project_id", mcpsdk.Description("Project ID (required for project scope).")),
		mcpsdk.WithArray("tags", mcpsdk.Description("Tags for categorization and filtering."), mcpsdk.WithStringItems()),
		mcpsdk.WithNumber("relevance", mcpsdk.Description("Relevance score 0-1 (default 1.0).")),
		mcpsdk.WithString("expires_at", mcpsdk.Description("RFC3339 timestamp or Go duration (e.g. '72h') when this memory should expire.")),
		mcpsdk.WithString("source_url", mcpsdk.Description("Optional URL/path to the source of this knowledge (task ID, PR, file path).")),
		mcpsdk.WithString("source_task_id", mcpsdk.Description("UUID of the Mesh task that produced this memory. Auto-populated from the fiddler side-channel (FIDDLER_STATE_FILE) or active checkout. Enables Amendment 2/3 KG edge hooks.")),
		mcpsdk.WithString("thread_id", mcpsdk.Description("Thread identifier for same-session memory grouping. Auto-populated from the fiddler side-channel when omitted.")),
		mcpsdk.WithBoolean("attach_context", mcpsdk.Description("When false, disables auto-injection of thread_id and source_task_id. Use for cross-cutting records not tied to the active task."), mcpsdk.DefaultBool(true)),
		mcpsdk.WithString("reason", mcpsdk.Description("Why this memory is worth writing — what a future thread should be able to do with it, or what changed if you are correcting an existing key. Recorded on the revision alongside the content, so a later reader can judge whether the entry still applies instead of guessing from the text alone. Optional today and about to become required; write it now.")),
		mcpsdk.WithNumber("expected_version", mcpsdk.Description("Make the write conditional: it succeeds only if the stored version still matches this number, and is REFUSED with both version numbers if someone else wrote to the key in between. Pass the version returned by your previous remember/recall of this key. Omit for last-write-wins.")),
	), s.tracked("remember", s.handleRemember))

	s.mcpServer.AddTool(mcpsdk.NewTool("forget",
		mcpsdk.WithDescription("Delete a memory entry. Agents can only delete their own agent-scope memories."),
		mcpsdk.WithString("memory_id", mcpsdk.Required(), mcpsdk.Description("UUID of the memory to delete.")),
	), s.tracked("forget", s.handleForget))

	s.mcpServer.AddTool(mcpsdk.NewTool("set_project_knowledge",
		mcpsdk.WithDescription("Write a structured fact to project knowledge. UPSERT by key — calling with same key updates the existing entry. Use for deploy URLs, stack conventions, gotchas. These facts are visible via get_project_knowledge."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project ID to store knowledge for.")),
		mcpsdk.WithString("key", mcpsdk.Required(), mcpsdk.Description("Slug key for UPSERT (e.g. 'deploy-url', 'stack-convention').")),
		mcpsdk.WithString("value", mcpsdk.Required(), mcpsdk.Description("The knowledge to store (markdown, max 4000 chars).")),
		mcpsdk.WithString("category", mcpsdk.Description("Optional category: deploy, stack, conventions, gotchas, api, auth, etc.")),
		mcpsdk.WithArray("tags", mcpsdk.Description("Additional tags for filtering."), mcpsdk.WithStringItems()),
		mcpsdk.WithString("source_url", mcpsdk.Description("Optional URL/path to the source of this knowledge.")),
		mcpsdk.WithString("source_task_id", mcpsdk.Description("UUID of the Mesh task that produced this fact. Auto-populated from the fiddler side-channel when omitted.")),
		mcpsdk.WithString("thread_id", mcpsdk.Description("Thread identifier. Auto-populated from the fiddler side-channel when omitted.")),
		mcpsdk.WithBoolean("attach_context", mcpsdk.Description("When false, disables auto-injection of thread_id and source_task_id."), mcpsdk.DefaultBool(true)),
	), s.tracked("set_project_knowledge", s.handleSetProjectKnowledge))

	s.mcpServer.AddTool(mcpsdk.NewTool("pavel_decision",
		mcpsdk.WithDescription("Record a Pavel directive as a canonical decision in project_knowledge. Broadcasts to specified agents via propagate_to tags. privacy:private records are stored but EXCLUDED from get_canonical_updates. Auto-flags private if text contains secrets. If task_id is given, also records this as a human_gate decision on that task (docs/human-gate-decision-recorded.md in evc-mesh) — releases the gate as a consequence if it's currently live, and links back via canonical_key. Best-effort: a failure here is reported in the result but does not undo the canonical write."),
		mcpsdk.WithString("text", mcpsdk.Required(), mcpsdk.Description("Full text of the decision/directive.")),
		mcpsdk.WithString("summary", mcpsdk.Required(), mcpsdk.Description("One-line summary used as UPSERT key (dedupes same decision on same day).")),
		mcpsdk.WithArray("propagate_to", mcpsdk.Description("Agent slugs to propagate to, e.g. ['linus','bill']. Use ['all'] for workspace-wide broadcast."), mcpsdk.WithStringItems()),
		mcpsdk.WithString("scope", mcpsdk.Description("Optional project_id UUID. Omit for workspace-level decisions.")),
		mcpsdk.WithString("privacy", mcpsdk.Description("'public' (default, visible in change-feed) or 'private' (recorded but hidden)."), mcpsdk.DefaultString("public")),
		mcpsdk.WithString("task_id", mcpsdk.Description("Optional task UUID this decision answers. When set, also records a human_gate decision on that task (provenance=attested, channel=telegram, quote=text) — releasing a live human_gate as a consequence. Omit for a plain canonical-only record (unchanged behavior).")),
	), s.tracked("pavel_decision", s.handlePavelDecision))

	s.mcpServer.AddTool(mcpsdk.NewTool("get_canonical_updates",
		mcpsdk.WithDescription("Fetch canonical decisions broadcast since a given time. Call at ACP step 6 (session start) to catch up on Pavel directives since your previous session. Returns only privacy:public records targeted at you or all agents."),
		mcpsdk.WithString("since", mcpsdk.Description("RFC3339 cursor. Defaults to your previous session's start time (server-resolved). Omit on first call.")),
		mcpsdk.WithString("agent", mcpsdk.Description("Your agent slug (e.g. 'linus'). Used to filter propagate_to:<slug> records. Omit to get only propagate_to:all records.")),
		mcpsdk.WithString("scope", mcpsdk.Description("Optional project UUID to restrict to project-scoped decisions.")),
	), s.tracked("get_canonical_updates", s.handleGetCanonicalUpdates))

	// --- Utility ---
	s.mcpServer.AddTool(mcpsdk.NewTool("list_projects",
		mcpsdk.WithDescription("List available projects in the workspace."),
		mcpsdk.WithString("workspace_id", mcpsdk.Description("Workspace ID. Defaults to agent's workspace.")),
		mcpsdk.WithBoolean("include_archived", mcpsdk.Description("Include archived projects."), mcpsdk.DefaultBool(false)),
	), s.tracked("list_projects", s.handleListProjects))

	s.mcpServer.AddTool(mcpsdk.NewTool("report_error",
		mcpsdk.WithDescription("Report an error encountered during work."),
		mcpsdk.WithString("task_id", mcpsdk.Description("Related task ID.")),
		mcpsdk.WithString("error_message", mcpsdk.Required(), mcpsdk.Description("Error message.")),
		mcpsdk.WithString("stack_trace", mcpsdk.Description("Stack trace or details.")),
		mcpsdk.WithString("severity", mcpsdk.Description("Severity: low, medium, high, critical."), mcpsdk.DefaultString("medium")),
		mcpsdk.WithBoolean("recoverable", mcpsdk.Description("Whether the error is recoverable."), mcpsdk.DefaultBool(true)),
	), s.tracked("report_error", s.handleReportError))

	s.mcpServer.AddTool(mcpsdk.NewTool("session_report",
		mcpsdk.WithDescription("Report session metrics. Call before session end. Returns compliance score and session stats."),
		mcpsdk.WithString("model", mcpsdk.Description("LLM model used (e.g. 'claude-sonnet-4').")),
		mcpsdk.WithNumber("tokens_in", mcpsdk.Description("Total input tokens this session.")),
		mcpsdk.WithNumber("tokens_out", mcpsdk.Description("Total output tokens this session.")),
		mcpsdk.WithNumber("estimated_cost", mcpsdk.Description("Estimated cost in USD.")),
	), s.tracked("session_report", s.handleSessionReport))
}

// registerAdvancedTools registers tools beyond the core set.
// These are available in the full profile only.
func (s *Server) registerAdvancedTools() {
	// --- Documents ---
	//
	// Every tool description sits in every agent's system context permanently,
	// so the surface is narrow by construction: read the map (list_docs), find a
	// page by content (search_docs), then read the part you need (get_doc).
	s.mcpServer.AddTool(mcpsdk.NewTool("list_docs",
		mcpsdk.WithDescription("List a project's documents — id, title, slug path, version, who touched them last. Carries NO document bodies, so it is safe to call on a whole project: use it as the map, then get_doc for one page. Returns path and has_children for navigating the tree."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project UUID.")),
		mcpsdk.WithBoolean("include_archived", mcpsdk.Description("Include archived documents."), mcpsdk.DefaultBool(false)),
	), s.tracked("list_docs", s.handleListDocs))

	s.mcpServer.AddTool(mcpsdk.NewTool("get_doc",
		mcpsdk.WithDescription("Read a document. By DEFAULT returns metadata plus the outline (headings) and NOT the body — a document is far larger than a task, and a body you read stays in your context for the rest of the session. Read the outline first, then pass section=\"<heading>\" for just that part; body=true returns the whole page and should be the exception. The returned version is what update_doc takes as base_version."),
		mcpsdk.WithString("doc", mcpsdk.Required(), mcpsdk.Description("Document UUID, or a slug path like 'architecture/adr/adr-004' (a path also needs project_id).")),
		mcpsdk.WithString("project_id", mcpsdk.Description("Project UUID. Required only when doc is a slug path.")),
		mcpsdk.WithString("section", mcpsdk.Description("Return only this section: a heading's text, or its anchor from the outline.")),
		mcpsdk.WithBoolean("body", mcpsdk.Description("Return the full markdown body. Prefer section= when you need one part."), mcpsdk.DefaultBool(false)),
		mcpsdk.WithBoolean("version_only", mcpsdk.Description("Return just the version — the cheap 'has this changed since I read it?' check before a write."), mcpsdk.DefaultBool(false)),
		mcpsdk.WithString("outline_depth", mcpsdk.Description("Limit the outline to headings at this level or shallower (e.g. '2' for chapters, not every subsection). Default: all levels.")),
	), s.tracked("get_doc", s.handleGetDoc))

	s.mcpServer.AddTool(mcpsdk.NewTool("search_docs",
		mcpsdk.WithDescription("Full-text search a project's documents by title and body. Returns matching documents with a snippet and a path usable directly with get_doc — this is how you find a document when you don't already know its path; list_docs is the map, this is the index. SCOPE IS PER-PROJECT ONLY: results never cross project_id, and this is not a substitute for recall (which searches memory, not documents) or for a cross-project doc search (none exists yet). A query that matches nothing returns an empty items list, not an error. Documents saved before full-text search shipped (2026-08-20) are matched by title only until their next edit."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project UUID. Search is scoped to this one project — call it once per project you need to check.")),
		mcpsdk.WithString("query", mcpsdk.Required(), mcpsdk.Description("Search text. Matched against title and body.")),
		mcpsdk.WithNumber("limit", mcpsdk.Description("Max results (default 20, server max 50).")),
	), s.tracked("search_docs", s.handleSearchDocs))

	s.mcpServer.AddTool(mcpsdk.NewTool("create_doc",
		mcpsdk.WithDescription("Create a document in a project. Returns its metadata and version — the version is what update_doc takes as base_version, so a create followed by an edit needs no read in between. The body you sent is not echoed back."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project UUID.")),
		mcpsdk.WithString("title", mcpsdk.Required(), mcpsdk.Description("Document title.")),
		mcpsdk.WithString("body", mcpsdk.Description("Markdown body.")),
		mcpsdk.WithString("slug", mcpsdk.Description("URL slug. Derived from the title if omitted.")),
		mcpsdk.WithString("parent_id", mcpsdk.Description("Parent document UUID, to nest this one under it.")),
		mcpsdk.WithNumber("position", mcpsdk.Description("Sort position among siblings.")),
	), s.tracked("create_doc", s.handleCreateDoc))

	s.mcpServer.AddTool(mcpsdk.NewTool("update_doc",
		mcpsdk.WithDescription("Edit a document. Replacing the body REQUIRES base_version — the version you got from get_doc — and the write is refused with a 409 if anyone changed the document since, so you can never silently overwrite someone else's edit. To add to the end, pass append instead: it needs no base_version, cannot conflict, and does not make you read the document first. Prefer append for reports, decisions and logs."),
		mcpsdk.WithString("doc", mcpsdk.Required(), mcpsdk.Description("Document UUID, or a slug path like 'architecture/adr/adr-004' (a path also needs project_id).")),
		mcpsdk.WithString("project_id", mcpsdk.Description("Project UUID. Required only when doc is a slug path.")),
		mcpsdk.WithString("append", mcpsdk.Description("Text to add to the END of the document. No base_version needed. Cannot be combined with body.")),
		mcpsdk.WithString("body", mcpsdk.Description("Replacement markdown for the WHOLE document. Requires base_version.")),
		mcpsdk.WithNumber("base_version", mcpsdk.Description("The version you read from get_doc. Required for any write other than append.")),
		mcpsdk.WithString("title", mcpsdk.Description("New title.")),
		mcpsdk.WithString("parent_id", mcpsdk.Description("New parent document UUID.")),
		mcpsdk.WithNumber("position", mcpsdk.Description("New sort position among siblings.")),
	), s.tracked("update_doc", s.handleUpdateDoc))

	// --- Document comments ---
	//
	// Two tools, and neither takes a byte offset. An agent points at text by
	// quoting it and the SERVER computes the position, because an agent computing
	// it gets it wrong on Cyrillic and the mistake is silent — see doc_comments.go.
	//
	// There is no resolve, unresolve or delete here on purpose: closing a
	// discussion is a claim about what people agreed, and it is absent from the
	// surface rather than refused at runtime.
	s.mcpServer.AddTool(mcpsdk.NewTool("comment_doc",
		mcpsdk.WithDescription("Comment on a document. To comment on a specific passage, pass quote with the text exactly as the document reads it — the server finds it and anchors the comment there, so you never compute a position yourself (there is no offset parameter, and a position you calculated would silently point at the wrong sentence). Without quote the comment is on the whole document. Your comment appears in the same thread humans see in the document UI."),
		mcpsdk.WithString("doc", mcpsdk.Required(), mcpsdk.Description("Document UUID, or a slug path like 'architecture/adr/adr-004' (a path also needs project_id).")),
		mcpsdk.WithString("body", mcpsdk.Required(), mcpsdk.Description("The comment text. Markdown; @slug mentions notify that person or agent.")),
		mcpsdk.WithString("project_id", mcpsdk.Description("Project UUID. Required only when doc is a slug path.")),
		mcpsdk.WithString("quote", mcpsdk.Description("The passage being commented on, copied from the document exactly. One sentence is plenty. Omit to comment on the document as a whole.")),
		mcpsdk.WithString("quote_context", mcpsdk.Description("A longer passage containing the quote exactly once — send this when the quote occurs several times in the document and you were told it was ambiguous.")),
		mcpsdk.WithString("reply_to", mcpsdk.Description("UUID of the comment being answered. A reply inherits that thread's anchor, so it takes no quote of its own.")),
	), s.tracked("comment_doc", s.handleCommentDoc))

	s.mcpServer.AddTool(mcpsdk.NewTool("list_doc_comments",
		mcpsdk.WithDescription("Read the comments on a document as threads — each top-level comment with its replies nested under it, the quoted passage it is anchored to, and who wrote it. Resolved threads are hidden unless include_resolved=true. A comment whose quoted text no longer exists in the document is marked orphaned=true in its anchor: it is still shown, and it is not pointing anywhere."),
		mcpsdk.WithString("doc", mcpsdk.Required(), mcpsdk.Description("Document UUID, or a slug path like 'architecture/adr/adr-004' (a path also needs project_id).")),
		mcpsdk.WithString("project_id", mcpsdk.Description("Project UUID. Required only when doc is a slug path.")),
		mcpsdk.WithBoolean("include_resolved", mcpsdk.Description("Include threads somebody marked resolved."), mcpsdk.DefaultBool(false)),
	), s.tracked("list_doc_comments", s.handleListDocComments))

	// --- Projects ---
	s.mcpServer.AddTool(mcpsdk.NewTool("get_project",
		mcpsdk.WithDescription("Get project details with statuses and custom fields."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project ID.")),
	), s.tracked("get_project", s.handleGetProject))

	// --- Task operations ---
	s.mcpServer.AddTool(mcpsdk.NewTool("create_subtask",
		mcpsdk.WithDescription("Create a subtask under a parent task. Set status_slug for initial status (defaults to the project's default status, NOT the parent's status)."),
		mcpsdk.WithString("parent_task_id", mcpsdk.Required(), mcpsdk.Description("Parent task ID.")),
		mcpsdk.WithString("title", mcpsdk.Required(), mcpsdk.Description("Subtask title.")),
		mcpsdk.WithString("description", mcpsdk.Description("Subtask description.")),
		mcpsdk.WithString("priority", mcpsdk.Description("Priority: urgent, high, medium, low, none."), mcpsdk.DefaultString("medium")),
		mcpsdk.WithString("status_slug", mcpsdk.Description("Status slug (e.g. 'todo'). Uses project default if omitted.")),
		// The subtask REST endpoint accepts all of the following, and create_task
		// already exposes them. Omitting them here made the two sibling tools diverge
		// for no stated reason — and because MCP does not reject unknown arguments, a
		// caller passing assignee_id got 201 back with the value silently discarded.
		mcpsdk.WithString("assignee_id", mcpsdk.Description("Agent or user ID to assign the subtask to. Defaults to the creator if omitted.")),
		mcpsdk.WithString("assignee_type", mcpsdk.Description("Assignee type: agent, user, or unassigned.")),
		mcpsdk.WithArray("labels", mcpsdk.Description("Labels for the subtask.")),
		mcpsdk.WithObject("custom_fields", mcpsdk.Description("Custom field values, keyed by field slug.")),
		mcpsdk.WithString("due_date", mcpsdk.Description("Due date, RFC3339 (e.g. 2026-08-10T12:00:00Z).")),
		mcpsdk.WithNumber("estimated_hours", mcpsdk.Description("Estimated hours.")),
	), s.tracked("create_subtask", s.handleCreateSubtask))

	s.mcpServer.AddTool(mcpsdk.NewTool("add_dependency",
		mcpsdk.WithDescription("Add a dependency between two tasks."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID.")),
		mcpsdk.WithString("depends_on_task_id", mcpsdk.Required(), mcpsdk.Description("ID of the task this depends on.")),
		mcpsdk.WithString("dependency_type", mcpsdk.Description("Dependency type: blocks, relates_to, is_child_of."), mcpsdk.DefaultString("blocks")),
	), s.tracked("add_dependency", s.handleAddDependency))

	// --- Atomic Task Checkout ---
	s.mcpServer.AddTool(mcpsdk.NewTool("checkout_task",
		mcpsdk.WithDescription("Atomically acquire an exclusive lock on a task. Prevents other agents from checking out the same task simultaneously. The lock is TTL-based and will expire automatically after ttl_minutes (default 120). Use before starting work on a task to ensure exclusive access."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID to check out.")),
		mcpsdk.WithNumber("ttl_minutes", mcpsdk.Description("Lock TTL in minutes (default 120).")),
	), s.tracked("checkout_task", s.handleCheckoutTask))

	s.mcpServer.AddTool(mcpsdk.NewTool("release_task",
		mcpsdk.WithDescription("Release the exclusive lock on a task acquired via checkout_task. Call when done with the task or if you need to hand it off. The lock is also released automatically when it expires."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID to release.")),
	), s.tracked("release_task", s.handleReleaseTask))

	s.mcpServer.AddTool(mcpsdk.NewTool("extend_checkout",
		mcpsdk.WithDescription("Push the expiry of an existing checkout_task lock forward, for work that runs longer than the original ttl_minutes. Requires an active checkout in this session (the cached checkout_token from checkout_task) — fails if the lock was never acquired here, already released, or already expired. Server clamps ttl_minutes to [1, 240]."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID whose checkout to extend.")),
		mcpsdk.WithNumber("ttl_minutes", mcpsdk.Description("New lock TTL in minutes from now (default 120, server clamps to [1, 240]).")),
	), s.tracked("extend_checkout", s.handleExtendCheckout))

	// --- Human gate (task #4545660b) ---
	// The ONE way an agent says "this card is waiting on a human". Before this,
	// "waiting on Pavel" was re-derived in 21 places by grepping comment text, each
	// with its own marker dictionary — which is how a driver came to read its own
	// instructional boilerplate back as a raised blocker (#84ab54fd).
	s.mcpServer.AddTool(mcpsdk.NewTool("set_human_gate",
		mcpsdk.WithDescription("Arm the human gate on a task: freeze it and record WHO is waiting, WHAT was asked, and WHAT you will do if nobody answers. Use INSTEAD of writing a '❓ Blocking @pavel' comment by hand — the marker still works, but this path records the whole ask on the task, so nothing has to re-read the thread. recommended_default is REQUIRED: a gate with no stated default can only ever be resolved by finding a human. You must answer four questions (credential_exists / reversible / blocked_by_other_task / customer_visible_now), each with one line of justification. The server REFUSES the arm when your own answers say nobody needs to be asked: if you hold the credential, the action is reversible, and nothing a customer sees or pays changes right now, capture a rollback anchor and just do it. If the blocker is another card, the server tells you to use add_dependency instead."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID to gate.")),
		mcpsdk.WithString("reason", mcpsdk.Required(), mcpsdk.Description("The question itself, in your own words.")),
		mcpsdk.WithString("recommended_default", mcpsdk.Required(), mcpsdk.Description("What you will do if nobody answers. Required — an ask with no default cannot time out.")),
		mcpsdk.WithString("class", mcpsdk.Description("'hard' (default, never auto-released) or 'soft' (released by timeout — the release does NOT answer the question).")),
		mcpsdk.WithString("deadline", mcpsdk.Description("RFC3339 timestamp when recommended_default applies. Omit for no deadline.")),
		// The four-question predicate (task #5d3dc714). The audit measured that 40-45% of
		// asks to Pavel were decidable from a rule already written down — an access
		// already in keys.env, the agent's own 403 read as a human's decision, an approval
		// Pavel had already declined, or waiting on someone else's card. Each answer needs
		// one line of justification: a bare bool is unreviewable, and answering these four
		// implicitly, in your head, is exactly how they got answered wrongly.
		mcpsdk.WithBoolean("credential_exists", mcpsdk.Required(), mcpsdk.Description("Do you ALREADY hold the credential or access this needs? Check ~/.config/agents/ and the fleet credentials doc before answering false — a service account the fleet created for the fleet is yours to use.")),
		mcpsdk.WithString("credential_reason", mcpsdk.Required(), mcpsdk.Description("One line: which credential, and where you checked.")),
		mcpsdk.WithBoolean("reversible", mcpsdk.Required(), mcpsdk.Description("Is there a rollback anchor — git revert, backup, snapshot, image tag? If you can MANUFACTURE one (take a backup first), the answer is true.")),
		mcpsdk.WithString("reversible_reason", mcpsdk.Required(), mcpsdk.Description("One line: the exact rollback path, or why none exists.")),
		mcpsdk.WithBoolean("blocked_by_other_task", mcpsdk.Required(), mcpsdk.Description("Is the thing you are waiting on actually ANOTHER card? If yes, the answer is add_dependency, not a gate — a blocks edge freezes the feed without adding anything to a human's queue.")),
		mcpsdk.WithString("blocked_reason", mcpsdk.Required(), mcpsdk.Description("One line: which card, or why none.")),
		mcpsdk.WithBoolean("customer_visible_now", mcpsdk.Required(), mcpsdk.Description("Does this change what a customer SEES or PAYS right now? A disabled gateway, an inactive flag or a reversible migration is NOT customer-visible; a rate that prints on invoices people already download is.")),
		mcpsdk.WithString("customer_reason", mcpsdk.Required(), mcpsdk.Description("One line: what the customer would see, or why nothing changes for them now.")),
	), s.tracked("set_human_gate", s.handleSetHumanGate))

	s.mcpServer.AddTool(mcpsdk.NewTool("clear_human_gate",
		mcpsdk.WithDescription("Release a human gate. Server-enforced user-only: an agent key gets a 403 that names the exits an agent CAN reach — withdraw your own marker with a short negator comment if you raised it, or record the human's answer via a human-gate decision. Read human_gate_info.clearable_by_owner on get_task first."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID whose gate to clear.")),
	), s.tracked("clear_human_gate", s.handleClearHumanGate))

	// --- Comments & Artifacts ---
	s.mcpServer.AddTool(mcpsdk.NewTool("list_comments",
		mcpsdk.WithDescription("List comments on a task. Paginated: call again with a higher `page` to read a thread longer than `limit`."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID.")),
		mcpsdk.WithBoolean("include_internal", mcpsdk.Description("Include internal (agent-only) comments."), mcpsdk.DefaultBool(true)),
		mcpsdk.WithNumber("limit", mcpsdk.Description("Max comments to return (default 50).")),
		mcpsdk.WithNumber("page", mcpsdk.Description("1-based page number. Omit for the first page; use with `has_more`/`total_pages` in the response to read the rest of a thread.")),
	), s.tracked("list_comments", s.handleListComments))

	s.mcpServer.AddTool(mcpsdk.NewTool("upload_artifact",
		mcpsdk.WithDescription("Upload an artifact (file, code, log, etc.) to a task. Inline content travels through the model context, so for a binary larger than a few KB prefer the REST endpoint instead: POST /api/v1/tasks/<task_id>/artifacts as multipart/form-data with -H 'X-Agent-Key: $MESH_AGENT_KEY' -F 'name=<file>' -F 'artifact_type=image' -F 'file=@<path>;type=image/png' — the bytes then never enter the context and cannot be truncated on the way."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID.")),
		mcpsdk.WithString("name", mcpsdk.Required(), mcpsdk.Description("Artifact filename.")),
		mcpsdk.WithString("content", mcpsdk.Required(), mcpsdk.Description("Artifact content. Plain text by default; set encoding=\"base64\" to send binary.")),
		mcpsdk.WithString("encoding", mcpsdk.Description("How to interpret content: \"text\" (stored as-is) or \"base64\" (decoded before storing). Required for binary — without it a base64 string is stored literally as the file body."), mcpsdk.DefaultString("text")),
		mcpsdk.WithString("sha256", mcpsdk.Description("Optional hex sha256 of the DECODED bytes. Verified before upload; a mismatch fails the call. Recommended for binary, since it is the only check that catches a payload truncated in transit.")),
		mcpsdk.WithString("artifact_type", mcpsdk.Description("Type: file, code, log, report, link, image, data."), mcpsdk.DefaultString("file")),
		mcpsdk.WithString("mime_type", mcpsdk.Description("MIME type. Auto-detected from name if omitted. For png/jpeg/gif/pdf/zip the content is checked against the type's magic bytes and the upload is refused on a mismatch.")),
		mcpsdk.WithObject("metadata", mcpsdk.Description("Additional metadata, stored on the artifact as JSON.")),
	), s.tracked("upload_artifact", s.handleUploadArtifact))

	s.mcpServer.AddTool(mcpsdk.NewTool("list_artifacts",
		mcpsdk.WithDescription("List artifacts attached to a task."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID.")),
	), s.tracked("list_artifacts", s.handleListArtifacts))

	s.mcpServer.AddTool(mcpsdk.NewTool("get_artifact",
		mcpsdk.WithDescription("Get artifact details and optionally its content."),
		mcpsdk.WithString("artifact_id", mcpsdk.Required(), mcpsdk.Description("Artifact ID.")),
		mcpsdk.WithBoolean("include_content", mcpsdk.Description("Include content for text files under 1MB."), mcpsdk.DefaultBool(false)),
	), s.tracked("get_artifact", s.handleGetArtifact))

	// --- Event Bus ---
	s.mcpServer.AddTool(mcpsdk.NewTool("publish_summary",
		mcpsdk.WithDescription("Publish a work summary event (convenience wrapper for publish_event with type=summary). Kept for backward compatibility — prefer publish_event with event_type='summary'."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project ID.")),
		mcpsdk.WithString("task_id", mcpsdk.Description("Related task ID.")),
		mcpsdk.WithString("summary", mcpsdk.Required(), mcpsdk.Description("Summary of work done.")),
		mcpsdk.WithArray("key_decisions", mcpsdk.Description("Key decisions made."), mcpsdk.WithStringItems()),
		mcpsdk.WithArray("artifacts_created", mcpsdk.Description("Artifacts created."), mcpsdk.WithStringItems()),
		mcpsdk.WithArray("blockers", mcpsdk.Description("Current blockers."), mcpsdk.WithStringItems()),
		mcpsdk.WithArray("next_steps", mcpsdk.Description("Suggested next steps."), mcpsdk.WithStringItems()),
		mcpsdk.WithObject("metrics", mcpsdk.Description("Metrics (lines changed, tests passed, etc.).")),
	), s.tracked("publish_summary", s.handlePublishSummary))

	s.mcpServer.AddTool(mcpsdk.NewTool("subscribe_events",
		mcpsdk.WithDescription("Configure push notification delivery for task events. Optionally sets a callback URL that Mesh will POST events to. Returns SSE and long-poll endpoint URLs for alternative delivery mechanisms."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project ID.")),
		mcpsdk.WithArray("event_types", mcpsdk.Description("Event types to subscribe to."), mcpsdk.WithStringItems()),
		mcpsdk.WithString("callback_url", mcpsdk.Description("Optional URL where Mesh will POST task events (task.assigned, task.created, task.status_changed). Leave empty to only use SSE or long-polling.")),
	), s.tracked("subscribe_events", s.handleSubscribeEvents))

	// --- Agent Hierarchy ---
	s.mcpServer.AddTool(mcpsdk.NewTool("register_sub_agent",
		mcpsdk.WithDescription("Register a sub-agent under the calling agent."),
		mcpsdk.WithString("name", mcpsdk.Required(), mcpsdk.Description("Sub-agent name.")),
		mcpsdk.WithString("agent_type", mcpsdk.Required(), mcpsdk.Description("Agent type: claude_code, openclaw, cline, aider, custom.")),
		mcpsdk.WithObject("capabilities", mcpsdk.Description("Agent capabilities as key-value pairs.")),
	), s.tracked("register_sub_agent", s.handleRegisterSubAgent))

	s.mcpServer.AddTool(mcpsdk.NewTool("list_sub_agents",
		mcpsdk.WithDescription("List sub-agents of an agent."),
		mcpsdk.WithString("agent_id", mcpsdk.Description("Parent agent ID. Defaults to the calling agent.")),
		mcpsdk.WithBoolean("recursive", mcpsdk.Description("Return all descendants (up to 10 levels deep)."), mcpsdk.DefaultBool(false)),
	), s.tracked("list_sub_agents", s.handleListSubAgents))

	// --- Team & Rules ---
	s.mcpServer.AddTool(mcpsdk.NewTool("get_team_directory",
		mcpsdk.WithDescription("Get the workspace team directory listing all agents and human members with their profiles."),
	), s.tracked("get_team_directory", s.handleGetTeamDirectory))

	s.mcpServer.AddTool(mcpsdk.NewTool("get_project_rules",
		mcpsdk.WithDescription("Get all rules configured for a project (all scopes: workspace + project). Kept for backward compatibility — prefer get_my_rules for agent-scoped effective rules."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project ID.")),
	), s.tracked("get_project_rules", s.handleGetProjectRules))

	s.mcpServer.AddTool(mcpsdk.NewTool("get_assignment_rules",
		mcpsdk.WithDescription("Get effective assignment rules for a project, merged from workspace and project level with source annotations."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project ID.")),
	), s.tracked("get_assignment_rules", s.handleGetAssignmentRules))

	s.mcpServer.AddTool(mcpsdk.NewTool("get_workflow_rules",
		mcpsdk.WithDescription("Get workflow rules for a project including allowed transitions, policies, and permissions for the calling agent."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project ID.")),
	), s.tracked("get_workflow_rules", s.handleGetWorkflowRules))

	s.mcpServer.AddTool(mcpsdk.NewTool("update_agent_profile",
		mcpsdk.WithDescription("Update the calling agent's profile fields such as role, capabilities, responsibility zone, and working hours."),
		mcpsdk.WithString("role", mcpsdk.Description("Agent role (e.g. developer, reviewer, tester).")),
		mcpsdk.WithArray("capabilities", mcpsdk.Description("List of capability strings (e.g. go, react, testing)."), mcpsdk.WithStringItems()),
		mcpsdk.WithString("responsibility_zone", mcpsdk.Description("Area of responsibility (e.g. Backend, Frontend).")),
		mcpsdk.WithString("escalation_to", mcpsdk.Description("Agent ID or name to escalate issues to.")),
		mcpsdk.WithArray("accepts_from", mcpsdk.Description("Agent IDs or types this agent accepts tasks from."), mcpsdk.WithStringItems()),
		mcpsdk.WithNumber("max_concurrent_tasks", mcpsdk.Description("Maximum number of concurrent tasks this agent can handle.")),
		mcpsdk.WithString("working_hours", mcpsdk.Description("Working hours description (e.g. 24/7, 9-17 UTC).")),
		mcpsdk.WithString("description", mcpsdk.Description("Human-readable description of the agent's purpose.")),
		mcpsdk.WithString("callback_url", mcpsdk.Description("URL where Mesh will POST task events (task.assigned, task.status_changed, task.commented). Set to empty string to disable.")),
	), s.tracked("update_agent_profile", s.handleUpdateAgentProfile))

	// --- Config ---
	s.mcpServer.AddTool(mcpsdk.NewTool("import_workspace_config",
		mcpsdk.WithDescription("Import workspace configuration from YAML. Applies rules, statuses, and project templates defined in the YAML."),
		mcpsdk.WithString("yaml_content", mcpsdk.Required(), mcpsdk.Description("YAML configuration content as a string.")),
	), s.tracked("import_workspace_config", s.handleImportWorkspaceConfig))

	s.mcpServer.AddTool(mcpsdk.NewTool("export_workspace_config",
		mcpsdk.WithDescription("Export the current workspace configuration as YAML, including rules, project templates, and settings."),
	), s.tracked("export_workspace_config", s.handleExportWorkspaceConfig))

	// --- Push Notifications ---
	s.mcpServer.AddTool(mcpsdk.NewTool("poll_tasks",
		mcpsdk.WithDescription("Long-poll for new task assignments. Blocks until a task is assigned to this agent or the timeout expires. Returns current assigned tasks and whether any change occurred. Kept for backward compatibility — prefer get_my_tasks for non-blocking access."),
		mcpsdk.WithNumber("timeout", mcpsdk.Description("Maximum seconds to wait for new assignments (default 30, max 120).")),
	), s.tracked("poll_tasks", s.handlePollTasks))

	// --- Recurring Tasks ---
	s.mcpServer.AddTool(mcpsdk.NewTool("create_recurring_task",
		mcpsdk.WithDescription("Creates a recurring task schedule that automatically spawns task instances on a schedule. Each instance gets access to the previous instance's summary. Use this for regular automated work: weekly reports, daily checks, periodic audits."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Target project UUID.")),
		mcpsdk.WithString("title_template", mcpsdk.Required(), mcpsdk.Description("Task title template. Supports {{.Date}}, {{.Number}}, {{.Week}}, {{.Month}}.")),
		mcpsdk.WithString("frequency", mcpsdk.Required(), mcpsdk.Description("Recurrence frequency: daily, weekly, monthly, custom. Use 'custom' with cron_expr for fine-grained control.")),
		mcpsdk.WithString("description_template", mcpsdk.Description("Task description template. Also supports {{.PrevSummary}} for previous instance context.")),
		mcpsdk.WithString("cron_expr", mcpsdk.Description("5-field cron expression (required if frequency=custom). Example: '0 9 * * 1' = every Monday at 9am.")),
		mcpsdk.WithString("timezone", mcpsdk.Description("IANA timezone for schedule evaluation. Default: UTC.")),
		mcpsdk.WithString("assignee_id", mcpsdk.Description("Agent or user UUID to assign each instance.")),
		mcpsdk.WithString("assignee_type", mcpsdk.Description("Assignee type: user, agent, unassigned."), mcpsdk.DefaultString("unassigned")),
		mcpsdk.WithString("priority", mcpsdk.Description("Priority: urgent, high, medium, low, none."), mcpsdk.DefaultString("none")),
		mcpsdk.WithArray("labels", mcpsdk.Description("Labels to apply to each instance."), mcpsdk.WithStringItems()),
		mcpsdk.WithString("starts_at", mcpsdk.Description("When to start the schedule (RFC3339). Default: now.")),
		mcpsdk.WithString("ends_at", mcpsdk.Description("When to stop the schedule (RFC3339). Default: no end.")),
		mcpsdk.WithNumber("max_instances", mcpsdk.Description("Maximum number of instances to create. Default: unlimited.")),
	), s.tracked("create_recurring_task", s.handleCreateRecurringTask))

	s.mcpServer.AddTool(mcpsdk.NewTool("list_recurring_schedules",
		mcpsdk.WithDescription("Lists all recurring task schedules for a project."),
		mcpsdk.WithString("project_id", mcpsdk.Required(), mcpsdk.Description("Project ID.")),
		mcpsdk.WithBoolean("active_only", mcpsdk.Description("Only return active schedules."), mcpsdk.DefaultBool(true)),
	), s.tracked("list_recurring_schedules", s.handleListRecurringSchedules))

	s.mcpServer.AddTool(mcpsdk.NewTool("get_recurring_history",
		mcpsdk.WithDescription("Returns the history of all instances for a recurring task schedule. ALWAYS call this when you receive a recurring task — it gives you context on what previous instances accomplished, what issues were found, and what artifacts were produced. Use it to continue work intelligently rather than starting from scratch."),
		mcpsdk.WithString("recurring_schedule_id", mcpsdk.Required(), mcpsdk.Description("UUID of the recurring schedule. Available in task.recurring_schedule_id field.")),
		mcpsdk.WithNumber("limit", mcpsdk.Description("Number of most recent instances to return. Default: 5. Use higher value for deep historical context.")),
	), s.tracked("get_recurring_history", s.handleGetRecurringHistory))

	s.mcpServer.AddTool(mcpsdk.NewTool("trigger_recurring_now",
		mcpsdk.WithDescription("Immediately creates the next instance of a recurring schedule, without waiting for the scheduled time. Useful for testing or urgent execution."),
		mcpsdk.WithString("recurring_schedule_id", mcpsdk.Required(), mcpsdk.Description("UUID of the recurring schedule.")),
	), s.tracked("trigger_recurring_now", s.handleTriggerRecurringNow))

	s.mcpServer.AddTool(mcpsdk.NewTool("update_recurring_schedule",
		mcpsdk.WithDescription("Update an existing recurring task schedule. Change title, description, frequency, assignee, priority, or deactivate it."),
		mcpsdk.WithString("recurring_schedule_id", mcpsdk.Required(), mcpsdk.Description("UUID of the recurring schedule to update.")),
		mcpsdk.WithString("title_template", mcpsdk.Description("New title template. Supports {{.Date}}, {{.Number}}, {{.Week}}, {{.Month}}.")),
		mcpsdk.WithString("description_template", mcpsdk.Description("New description template. Supports {{.PrevSummary}}.")),
		mcpsdk.WithString("frequency", mcpsdk.Description("New frequency: daily, weekly, monthly, custom.")),
		mcpsdk.WithString("cron_expr", mcpsdk.Description("New cron expression (for custom frequency).")),
		mcpsdk.WithString("timezone", mcpsdk.Description("New IANA timezone.")),
		mcpsdk.WithString("assignee_id", mcpsdk.Description("New assignee UUID.")),
		mcpsdk.WithString("assignee_type", mcpsdk.Description("New assignee type: user, agent, unassigned.")),
		mcpsdk.WithString("priority", mcpsdk.Description("New priority: urgent, high, medium, low, none.")),
		mcpsdk.WithBoolean("is_active", mcpsdk.Description("Set to false to pause the schedule.")),
	), s.tracked("update_recurring_schedule", s.handleUpdateRecurringSchedule))

	s.mcpServer.AddTool(mcpsdk.NewTool("delete_recurring_schedule",
		mcpsdk.WithDescription("Delete a recurring task schedule. Existing task instances are not affected."),
		mcpsdk.WithString("recurring_schedule_id", mcpsdk.Required(), mcpsdk.Description("UUID of the recurring schedule to delete.")),
	), s.tracked("delete_recurring_schedule", s.handleDeleteRecurringSchedule))

	// --- Canonical knowledge layer ---
	s.mcpServer.AddTool(mcpsdk.NewTool("get_canonical",
		mcpsdk.WithDescription("Query the canonical knowledge layer: returns curated facts, decisions, and strategy docs for a topic, merged from project_memories (key canonical:*) and workspace_memories (kind:canonical). Excludes ephemeral session-checkpoints. Slug aliases are resolved automatically (e.g. mesh-dev == evc-mesh). Call before authoring any doc that might conflict with existing canonical knowledge."),
		mcpsdk.WithString("topic", mcpsdk.Required(), mcpsdk.Description("Topic or keyword to search (e.g. 'auth middleware', 'evc-spark roadmap').")),
		mcpsdk.WithString("project", mcpsdk.Description("Optional project slug to narrow results (e.g. 'evc-mesh', 'evc-spark'). Aliases resolved automatically.")),
	), s.tracked("get_canonical", s.handleGetCanonical))
}

// --- Helper functions ---

// parseUUID parses a UUID string, returning an error result if invalid.
func parseUUID(s string) (uuid.UUID, error) {
	if s == "" {
		return uuid.Nil, fmt.Errorf("UUID is required")
	}
	return uuid.Parse(s)
}

// jsonResult marshals the value to JSON and returns a text result.
func jsonResult(v any) (*mcpsdk.CallToolResult, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return mcpsdk.NewToolResultError("failed to marshal response"), nil
	}
	return mcpsdk.NewToolResultText(string(data)), nil
}

// errResult returns an error tool result with a formatted message.
func errResult(format string, args ...any) (*mcpsdk.CallToolResult, error) {
	return mcpsdk.NewToolResultError(fmt.Sprintf(format, args...)), nil
}

// hasArgument reports whether the caller supplied the given argument at all.
//
// mcpsdk's Parse* helpers collapse "absent" and "explicitly set to the default"
// into the same value, which is fine for reading a value and wrong for deciding
// whether a preset may overwrite one. Anything a profile is allowed to override
// needs this distinction.
func hasArgument(request mcpsdk.CallToolRequest, key string) bool {
	args := request.GetArguments()
	if args == nil {
		return false
	}
	v, ok := args[key]
	return ok && v != nil
}

// parseStringSlice extracts a string slice from request arguments.
func parseStringSlice(request mcpsdk.CallToolRequest, key string) []string {
	args := request.GetArguments()
	if args == nil {
		return nil
	}
	v, ok := args[key]
	if !ok || v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	result := make([]string, 0, len(arr))
	for _, item := range arr {
		if str, ok := item.(string); ok {
			result = append(result, str)
		}
	}
	return result
}

// detectMIMEType guesses MIME type from file extension.
func detectMIMEType(name string) string {
	lower := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lower, ".json"):
		return "application/json"
	case strings.HasSuffix(lower, ".yaml"), strings.HasSuffix(lower, ".yml"):
		return "application/x-yaml"
	case strings.HasSuffix(lower, ".xml"):
		return "application/xml"
	case strings.HasSuffix(lower, ".html"), strings.HasSuffix(lower, ".htm"):
		return "text/html"
	case strings.HasSuffix(lower, ".css"):
		return "text/css"
	case strings.HasSuffix(lower, ".js"):
		return "application/javascript"
	case strings.HasSuffix(lower, ".ts"):
		return "application/typescript"
	case strings.HasSuffix(lower, ".go"):
		return "text/x-go"
	case strings.HasSuffix(lower, ".py"):
		return "text/x-python"
	case strings.HasSuffix(lower, ".rs"):
		return "text/x-rust"
	case strings.HasSuffix(lower, ".md"):
		return "text/markdown"
	case strings.HasSuffix(lower, ".txt"):
		return "text/plain"
	case strings.HasSuffix(lower, ".csv"):
		return "text/csv"
	case strings.HasSuffix(lower, ".png"):
		return "image/png"
	case strings.HasSuffix(lower, ".jpg"), strings.HasSuffix(lower, ".jpeg"):
		return "image/jpeg"
	case strings.HasSuffix(lower, ".gif"):
		return "image/gif"
	case strings.HasSuffix(lower, ".svg"):
		return "image/svg+xml"
	case strings.HasSuffix(lower, ".pdf"):
		return "application/pdf"
	case strings.HasSuffix(lower, ".zip"):
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}

// truncate shortens a string to at most maxLen characters.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// resolveStatusSlug looks up a status UUID from its slug by querying the REST API.
// Returns the status ID, name, and category on success.
//
// This is the project-scoped form. Use it only when the write being served is itself
// project-gated (e.g. create_task, which posts to /projects/:proj_id/tasks). For a
// write on a task route, use resolveStatusSlugForTask — see the note there.
func (s *Server) resolveStatusSlug(ctx context.Context, projectID, slug string) (statusID, statusName, statusCategory string, err error) {
	statuses, err := s.getRESTClient(ctx).GetProjectStatuses(ctx, projectID)
	if err != nil {
		return "", "", "", fmt.Errorf("get statuses: %w", err)
	}
	return pickStatusBySlug(statuses, slug)
}

// resolveStatusSlugForTask looks up a status UUID from its slug for the project
// owning taskID, reading it through the task-scoped status route.
//
// Why this exists rather than "get the task, then call resolveStatusSlug": the
// project-scoped status route is project-gated, while the writes it serves here —
// POST /tasks/:task_id/move and POST /tasks/:task_id/subtasks — are workspace-gated.
// A read that a write cannot proceed without must never be gated more strictly than
// that write. While it was, a caller who was entitled to move a task could not
// discover which statuses it could move to, and the refusal named the project rather
// than the move. The task-scoped route carries the move's own gate.
//
// It also saves a round trip: the task no longer has to be fetched just to learn its
// project_id.
//
// The fallback fires on 404 only — that is, a server predating the task-scoped route.
// Any other failure is returned as-is: "I could not look" must not be quietly
// converted into "I looked through a different door", which would restore the old
// 403 while reporting success at this layer.
func (s *Server) resolveStatusSlugForTask(ctx context.Context, taskID, slug string) (statusID, statusName, statusCategory string, err error) {
	client := s.getRESTClient(ctx)

	statuses, err := client.GetTaskStatuses(ctx, taskID)
	if err != nil {
		var apiErr *APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound {
			return "", "", "", fmt.Errorf("get statuses: %w", err)
		}

		// Either the server has no task-scoped status route, or the task does not
		// exist. Resolving the task tells the two apart and produces the right error.
		task, taskErr := client.GetTask(ctx, taskID)
		if taskErr != nil {
			return "", "", "", fmt.Errorf("get task: %w", taskErr)
		}
		projectID, _ := task["project_id"].(string)
		if projectID == "" {
			return "", "", "", fmt.Errorf("task has no project_id")
		}
		statuses, err = client.GetProjectStatuses(ctx, projectID)
		if err != nil {
			return "", "", "", fmt.Errorf("get statuses: %w", err)
		}
	}

	return pickStatusBySlug(statuses, slug)
}

// statusSlugNotFoundError marks the ONE failure inside resolveStatusSlugForTask
// (and resolveStatusSlug) that is actually about the slug argument: the status
// list was fetched fine, but no entry in it matches the requested slug. Every
// other failure in that chain (bad/missing/forbidden task_id, no project_id,
// a transport error fetching the list) happens before a status list even
// exists to search — those are task/project resolution failures, not a bad
// slug, and callers use errors.As against this type to tell the two apart
// instead of blaming status_slug for all of them (see handleMoveTask).
type statusSlugNotFoundError struct {
	slug string
}

func (e *statusSlugNotFoundError) Error() string {
	return fmt.Sprintf("status '%s' not found in project", e.slug)
}

// pickStatusBySlug selects the status with the given slug from a status list.
func pickStatusBySlug(statuses []map[string]any, slug string) (statusID, statusName, statusCategory string, err error) {
	for _, st := range statuses {
		stSlug, _ := st["slug"].(string)
		if stSlug == slug {
			stID, _ := st["id"].(string)
			stName, _ := st["name"].(string)
			stCat, _ := st["category"].(string)
			return stID, stName, stCat, nil
		}
	}
	return "", "", "", &statusSlugNotFoundError{slug: slug}
}
