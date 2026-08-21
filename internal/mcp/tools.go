package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// readFiddlerContext reads the per-session side-channel file written by fiddler.py
// on each task feed. The path comes from the FIDDLER_STATE_FILE env var set when
// the tmux session is (re)launched. Returns empty strings on any error.
func readFiddlerContext() (taskID, threadID string) {
	path := os.Getenv("FIDDLER_STATE_FILE")
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var ctx struct {
		TaskID   string `json:"task_id"`
		ThreadID string `json:"thread_id"`
	}
	if json.Unmarshal(data, &ctx) != nil {
		return
	}
	return ctx.TaskID, ctx.ThreadID
}

// ============================================================================
// 1. list_projects
// ============================================================================

func (s *Server) handleListProjects(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	wsIDStr := mcpsdk.ParseString(request, "workspace_id", "")
	includeArchived := mcpsdk.ParseBoolean(request, "include_archived", false)

	wsID := session.WorkspaceID.String()
	if wsIDStr != "" {
		wsID = wsIDStr
	}

	result, err := s.getRESTClient(ctx).ListProjects(ctx, wsID, includeArchived)
	if err != nil {
		return errResult("failed to list projects: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 2. get_project
// ============================================================================

func (s *Server) handleGetProject(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}

	project, err := s.getRESTClient(ctx).GetProject(ctx, projectID)
	if err != nil {
		return errResult("failed to get project: %v", err)
	}

	statuses, err := s.getRESTClient(ctx).GetProjectStatuses(ctx, projectID)
	if err != nil {
		return errResult("failed to list statuses: %v", err)
	}

	fields, err := s.getRESTClient(ctx).GetProjectCustomFields(ctx, projectID)
	if err != nil {
		return errResult("failed to list custom fields: %v", err)
	}

	return jsonResult(map[string]any{
		"project":       project,
		"statuses":      statuses,
		"custom_fields": fields,
	})
}

// ============================================================================
// 3. list_tasks
// ============================================================================

func (s *Server) handleListTasks(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	projectID := mcpsdk.ParseString(request, "project_id", "")
	workspaceID := mcpsdk.ParseString(request, "workspace_id", "")

	if projectID == "" && workspaceID == "" {
		return errResult("project_id or workspace_id is required")
	}

	params := map[string]string{}

	if search := mcpsdk.ParseString(request, "search", ""); search != "" {
		params["search"] = search
	}
	if at := mcpsdk.ParseString(request, "assignee_type", ""); at != "" {
		params["assignee_type"] = at
	}
	if p := mcpsdk.ParseString(request, "priority", ""); p != "" {
		params["priority"] = p
	}
	if labels := parseStringSlice(request, "labels"); len(labels) > 0 {
		params["labels"] = labels[0] // API supports single label filter
	}
	if sort := mcpsdk.ParseString(request, "sort", ""); sort != "" {
		params["sort_by"] = sort
	}

	limit := mcpsdk.ParseInt(request, "limit", 50)
	if limit > 0 {
		params["page_size"] = strconv.Itoa(limit)
	}
	if listRevision := mcpsdk.ParseInt64(request, "list_revision", 0); listRevision != 0 {
		params["list_revision"] = strconv.FormatInt(listRevision, 10)
	}

	// workspace_id path: global search across all projects.
	if workspaceID != "" {
		result, err := s.getRESTClient(ctx).SearchTasks(ctx, workspaceID, params)
		if err != nil {
			return errResult("failed to search tasks: %v", err)
		}
		return jsonResult(result)
	}

	// status_category: resolve to all matching status IDs via the API.
	// The REST API accepts status= as comma-separated UUIDs.
	if cat := mcpsdk.ParseString(request, "status_category", ""); cat != "" {
		statuses, err := s.getRESTClient(ctx).GetProjectStatuses(ctx, projectID)
		if err != nil {
			return errResult("failed to resolve status category: %v", err)
		}
		var matchedIDs []string
		for _, st := range statuses {
			stCat, _ := st["category"].(string)
			if stCat == cat {
				if stID, _ := st["id"].(string); stID != "" {
					matchedIDs = append(matchedIDs, stID)
				}
			}
		}
		if len(matchedIDs) > 0 {
			params["status"] = strings.Join(matchedIDs, ",")
		}
	}

	result, err := s.getRESTClient(ctx).ListTasks(ctx, projectID, params)
	if err != nil {
		return errResult("failed to list tasks: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 4. get_task
// ============================================================================

func (s *Server) handleGetTask(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}

	task, err := s.getRESTClient(ctx).GetTask(ctx, taskID)
	if err != nil {
		return errResult("failed to get task: %v", err)
	}

	resp := map[string]any{
		"task": task,
	}

	if mcpsdk.ParseBoolean(request, "include_comments", false) {
		page, err := s.getRESTClient(ctx).GetTaskComments(ctx, taskID)
		if err != nil {
			return errResult("failed to list comments: %v", err)
		}
		var itemCount int
		if items, ok := page["items"]; ok {
			resp["comments"] = items
			if arr, ok := items.([]any); ok {
				itemCount = len(arr)
			}
		} else {
			resp["comments"] = []any{}
		}
		// Propagate the truncation envelope REST already returns (total_count,
		// has_more) — the old code discarded both, which is exactly what made
		// a truncated response indistinguishable from a complete one. Ported
		// from entire-vc/evc-mesh (task 4222c17d / D2) — see GetTaskComments
		// above for why this repo needs its own copy of the fix.
		totalCount, _ := page["total_count"].(float64)
		hasMore, _ := page["has_more"].(bool)
		resp["comments_total_count"] = int(totalCount)
		resp["comments_has_more"] = hasMore
		if hasMore {
			resp["comments_truncated"] = true
			resp["comments_note"] = fmt.Sprintf(
				"showing the last %d of %d comments; call list_comments(task_id, page_size=200) or page through /comments for the rest",
				itemCount, int(totalCount))
		}
	}

	if mcpsdk.ParseBoolean(request, "include_artifacts", false) {
		page, err := s.getRESTClient(ctx).GetTaskArtifacts(ctx, taskID)
		if err != nil {
			return errResult("failed to list artifacts: %v", err)
		}
		var itemCount int
		if items, ok := page["items"]; ok {
			resp["artifacts"] = items
			if arr, ok := items.([]any); ok {
				itemCount = len(arr)
			}
		} else {
			resp["artifacts"] = []any{}
		}
		// Same envelope-stripping pattern as comments: artifacts already list
		// newest-first by default, so the ordering half of the comments bug
		// doesn't apply here, but a task with more artifacts than
		// DefaultPageSize still silently lost the rest without this.
		totalCount, _ := page["total_count"].(float64)
		hasMore, _ := page["has_more"].(bool)
		resp["artifacts_total_count"] = int(totalCount)
		resp["artifacts_has_more"] = hasMore
		if hasMore {
			resp["artifacts_truncated"] = true
			resp["artifacts_note"] = fmt.Sprintf(
				"showing %d of %d artifacts; call list_artifacts(task_id, page_size=200) or page through /artifacts for the rest",
				itemCount, int(totalCount))
		}
	}

	if mcpsdk.ParseBoolean(request, "include_dependencies", false) {
		deps, err := s.getRESTClient(ctx).GetTaskDependencies(ctx, taskID)
		if err != nil {
			return errResult("failed to list dependencies: %v", err)
		}
		// dependencies = this task's own blockers (outgoing), matching the
		// semantics callers historically got from the bare-array response.
		resp["dependencies"] = deps.Outgoing
		resp["dependencies_incoming"] = deps.Incoming
	}

	return jsonResult(resp)
}

// ============================================================================
// 5. create_task
// ============================================================================

func (s *Server) handleCreateTask(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}

	title := mcpsdk.ParseString(request, "title", "")
	if title == "" {
		return errResult("title is required")
	}

	body := map[string]any{
		"title":         title,
		"assignee_type": mcpsdk.ParseString(request, "assignee_type", "unassigned"),
		"priority":      mcpsdk.ParseString(request, "priority", "medium"),
	}

	// Resolve status slug to status_id, and guard against creating in review status.
	if slug := mcpsdk.ParseString(request, "status_slug", ""); slug != "" {
		stID, _, stCat, err := s.resolveStatusSlug(ctx, projectID, slug)
		if err != nil {
			return errResult("invalid status_slug: %v", err)
		}
		if strings.EqualFold(stCat, "review") {
			return errResult("Cannot create task in review status. Use 'todo' or 'in_progress'. Review is for tasks with completed work awaiting check.")
		}
		body["status_id"] = stID
	}
	// If no status_slug provided, REST API will use project default.

	if desc := mcpsdk.ParseString(request, "description", ""); desc != "" {
		body["description"] = desc
	}
	if assigneeID := mcpsdk.ParseString(request, "assignee_id", ""); assigneeID != "" {
		body["assignee_id"] = assigneeID
	}
	if parentTaskID := mcpsdk.ParseString(request, "parent_task_id", ""); parentTaskID != "" {
		body["parent_task_id"] = parentTaskID
	}
	if dueDateStr := mcpsdk.ParseString(request, "due_date", ""); dueDateStr != "" {
		if _, err := time.Parse(time.RFC3339, dueDateStr); err != nil {
			return errResult("invalid due_date format: %v", err)
		}
		body["due_date"] = dueDateStr
	}
	if eh := mcpsdk.ParseFloat64(request, "estimated_hours", 0); eh > 0 {
		body["estimated_hours"] = eh
	}
	if labels := parseStringSlice(request, "labels"); len(labels) > 0 {
		body["labels"] = labels
	}
	if cfMap := mcpsdk.ParseStringMap(request, "custom_fields", nil); cfMap != nil {
		body["custom_fields"] = cfMap
	}
	if dl := mcpsdk.ParseString(request, "delegation_level", ""); dl != "" {
		body["delegation_level"] = dl
	}

	result, err := s.getRESTClient(ctx).CreateTask(ctx, projectID, body)
	if err != nil {
		return errResult("failed to create task: %v", err)
	}

	if warn := s.contextWarning(); warn != "" {
		result["_mesh_warning"] = warn
	}

	return jsonResult(result)
}

// ============================================================================
// 6. update_task
// ============================================================================

func (s *Server) handleUpdateTask(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}

	args := request.GetArguments()
	body := map[string]any{}

	if _, ok := args["title"]; ok {
		body["title"] = mcpsdk.ParseString(request, "title", "")
	}
	if _, ok := args["description"]; ok {
		body["description"] = mcpsdk.ParseString(request, "description", "")
	}
	if _, ok := args["priority"]; ok {
		body["priority"] = mcpsdk.ParseString(request, "priority", "")
	}
	if _, ok := args["labels"]; ok {
		body["labels"] = parseStringSlice(request, "labels")
	}
	if _, ok := args["custom_fields"]; ok {
		cfMap := mcpsdk.ParseStringMap(request, "custom_fields", nil)
		if cfMap != nil {
			body["custom_fields"] = cfMap
		}
	}
	if dueDateStr := mcpsdk.ParseString(request, "due_date", ""); dueDateStr != "" {
		if _, err := time.Parse(time.RFC3339, dueDateStr); err != nil {
			return errResult("invalid due_date format: %v", err)
		}
		body["due_date"] = dueDateStr
	}
	if _, ok := args["estimated_hours"]; ok {
		eh := mcpsdk.ParseFloat64(request, "estimated_hours", 0)
		body["estimated_hours"] = eh
	}
	// delegation_level is settable at creation but was unsettable afterwards — an
	// asymmetry with no rationale, so a task's routing could never be corrected.
	if dl := mcpsdk.ParseString(request, "delegation_level", ""); dl != "" {
		body["delegation_level"] = dl
	}
	// completion_signal is documented in the domain as "set by an agent to indicate
	// agent-side work is done" — agent-facing by design, and unreachable until now.
	if _, ok := args["completion_signal"]; ok {
		body["completion_signal"] = mcpsdk.ParseBoolean(request, "completion_signal", false)
	}

	if len(body) == 0 {
		return errResult("no fields to update")
	}

	result, err := s.getRESTClient(ctx).UpdateTask(ctx, taskID, body)
	if err != nil {
		return errResult("failed to update task: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 7. move_task
// ============================================================================

func (s *Server) handleMoveTask(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}

	statusSlug := mcpsdk.ParseString(request, "status_slug", "")
	if statusSlug == "" {
		return errResult("status_slug is required")
	}

	// Resolve slug to status ID (move_task to review is intentionally allowed).
	// Read through the task-scoped route: it carries the same workspace gate as the
	// move itself, so a caller entitled to the transition is entitled to the lookup.
	// The project-scoped route would refuse a workspace member who is not a member of
	// the task's project, with a 403 naming the project rather than the move.
	//
	// resolveStatusSlugForTask fails for two very different reasons, and the message
	// must name whichever field actually caused it (measured live 2026-08-20: an
	// invalid task_id produced "invalid status_slug: get statuses: ... invalid
	// task_id", blaming the one argument that was fine). A statusSlugNotFoundError
	// means the status list was fetched fine and the slug just isn't in it — that
	// really is a status_slug problem. Anything else failed before a status list
	// existed to search, which only happens when task_id couldn't be resolved.
	stID, stName, _, err := s.resolveStatusSlugForTask(ctx, taskID, statusSlug)
	if err != nil {
		var notFound *statusSlugNotFoundError
		if errors.As(err, &notFound) {
			return errResult("invalid status_slug: %v", err)
		}
		return errResult("invalid task_id: %v", err)
	}

	moveBody := map[string]any{
		"status_id": stID,
	}
	// Optional explicit assignee — overrides auto-reassign on review.
	if assigneeID := mcpsdk.ParseString(request, "assignee_id", ""); assigneeID != "" {
		moveBody["assignee_id"] = assigneeID
		moveBody["assignee_type"] = mcpsdk.ParseString(request, "assignee_type", "agent")
	}

	if err = s.getRESTClient(ctx).MoveTask(ctx, taskID, moveBody); err != nil {
		return errResult("failed to move task: %v", err)
	}

	// Add optional comment.
	if commentBody := mcpsdk.ParseString(request, "comment", ""); commentBody != "" {
		// Best-effort: don't fail the move if comment creation fails.
		_, _ = s.getRESTClient(ctx).AddComment(ctx, taskID, map[string]any{
			"body":        commentBody,
			"is_internal": false,
		})
	}

	// Return updated task.
	updatedTask, err := s.getRESTClient(ctx).GetTask(ctx, taskID)
	if err != nil {
		return errResult("task moved but failed to reload: %v", err)
	}

	return jsonResult(map[string]any{
		"task":       updatedTask,
		"new_status": map[string]any{"id": stID, "slug": statusSlug, "name": stName},
	})
}

// ============================================================================
// 8. create_subtask
// ============================================================================

func (s *Server) handleCreateSubtask(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	parentTaskID := mcpsdk.ParseString(request, "parent_task_id", "")
	if parentTaskID == "" {
		return errResult("parent_task_id is required")
	}

	title := mcpsdk.ParseString(request, "title", "")
	if title == "" {
		return errResult("title is required")
	}

	body := map[string]any{
		"title":    title,
		"priority": mcpsdk.ParseString(request, "priority", "medium"),
	}
	if desc := mcpsdk.ParseString(request, "description", ""); desc != "" {
		body["description"] = desc
	}
	// Forward the rest of what POST /tasks/:task_id/subtasks accepts, mirroring
	// handleCreateTask field for field. assignee_id matters most: our convention is that a
	// subtask is owned by whoever will do the work, not by whoever split the parent
	// task up — and until this was forwarded, following that convention produced the
	// exact outcome it prevents, silently, because MCP ignores undeclared arguments.
	if assigneeID := mcpsdk.ParseString(request, "assignee_id", ""); assigneeID != "" {
		body["assignee_id"] = assigneeID
		// Only send a type alongside an id; the API defaults a bare type to "agent",
		// so emitting one unconditionally would mistype an unassigned subtask.
		body["assignee_type"] = mcpsdk.ParseString(request, "assignee_type", "agent")
	}
	if dueDateStr := mcpsdk.ParseString(request, "due_date", ""); dueDateStr != "" {
		if _, err := time.Parse(time.RFC3339, dueDateStr); err != nil {
			return errResult("invalid due_date format: %v", err)
		}
		body["due_date"] = dueDateStr
	}
	if eh := mcpsdk.ParseFloat64(request, "estimated_hours", 0); eh > 0 {
		body["estimated_hours"] = eh
	}
	if labels := parseStringSlice(request, "labels"); len(labels) > 0 {
		body["labels"] = labels
	}
	if cfMap := mcpsdk.ParseStringMap(request, "custom_fields", nil); cfMap != nil {
		body["custom_fields"] = cfMap
	}

	// Resolve status slug against the parent's project. Omitted → project default.
	// Same gate reasoning as move_task: POST /tasks/:task_id/subtasks is workspace-gated,
	// so the slug lookup it depends on is read through the task-scoped route too.
	// Fixing only move_task would have left this second entry into the same dead end open.
	if slug := mcpsdk.ParseString(request, "status_slug", ""); slug != "" {
		stID, _, _, err := s.resolveStatusSlugForTask(ctx, parentTaskID, slug)
		if err != nil {
			return errResult("invalid status_slug: %v", err)
		}
		body["status_id"] = stID
	}

	result, err := s.getRESTClient(ctx).CreateSubtask(ctx, parentTaskID, body)
	if err != nil {
		return errResult("failed to create subtask: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 9. add_dependency
// ============================================================================

func (s *Server) handleAddDependency(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}

	dependsOnID := mcpsdk.ParseString(request, "depends_on_task_id", "")
	if dependsOnID == "" {
		return errResult("depends_on_task_id is required")
	}

	body := map[string]any{
		"depends_on_task_id": dependsOnID,
		"dependency_type":    mcpsdk.ParseString(request, "dependency_type", "blocks"),
	}

	result, err := s.getRESTClient(ctx).AddDependency(ctx, taskID, body)
	if err != nil {
		return errResult("failed to add dependency: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 10. assign_task
// ============================================================================

func (s *Server) handleAssignTask(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}

	body := map[string]any{}
	assignToSelf := mcpsdk.ParseBoolean(request, "assign_to_self", false)

	if assignToSelf {
		body["assignee_id"] = session.AgentID.String()
		body["assignee_type"] = "agent"
	} else {
		assigneeID := mcpsdk.ParseString(request, "assignee_id", "")
		if assigneeID != "" {
			body["assignee_id"] = assigneeID
			body["assignee_type"] = mcpsdk.ParseString(request, "assignee_type", "agent")
		} else {
			body["assignee_type"] = "unassigned"
		}
	}

	result, err := s.getRESTClient(ctx).AssignTask(ctx, taskID, body)
	if err != nil {
		return errResult("failed to assign task: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 11. add_comment
// ============================================================================

func (s *Server) handleAddComment(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}

	body := mcpsdk.ParseString(request, "body", "")
	if body == "" {
		return errResult("body is required")
	}

	reqBody := map[string]any{
		"body":        body,
		"is_internal": mcpsdk.ParseBoolean(request, "is_internal", false),
	}

	if parentID := mcpsdk.ParseString(request, "parent_comment_id", ""); parentID != "" {
		reqBody["parent_comment_id"] = parentID
	}

	if metaMap := mcpsdk.ParseStringMap(request, "metadata", nil); metaMap != nil {
		metaBytes, err := json.Marshal(metaMap)
		if err == nil {
			reqBody["metadata"] = json.RawMessage(metaBytes)
		}
	}

	result, err := s.getRESTClient(ctx).AddComment(ctx, taskID, reqBody)
	if err != nil {
		return errResult("failed to create comment: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 11a. add_vcs_link
// ============================================================================

func (s *Server) handleAddVCSLink(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}

	rawURL := mcpsdk.ParseString(request, "url", "")
	if rawURL == "" {
		return errResult("url is required")
	}

	// Everything below the URL is inferred unless the caller overrides it.
	facts := parseVCSURL(rawURL)

	provider := mcpsdk.ParseString(request, "provider", "")
	if provider == "" {
		provider = facts.Provider
	}
	if provider == "" {
		provider = "github"
	}

	linkType := mcpsdk.ParseString(request, "link_type", "")
	if linkType == "" {
		linkType = facts.LinkType
	}
	if linkType == "" {
		linkType = vcsLinkTypePR
	}
	linkType = normalizeVCSLinkType(linkType)

	externalID := mcpsdk.ParseString(request, "external_id", "")
	if externalID == "" {
		externalID = facts.ExternalID
	}
	if externalID == "" {
		return errResult(
			"could not infer external_id from url %q — pass external_id explicitly "+
				"(the PR number, commit SHA, or branch name)", rawURL)
	}

	reqBody := map[string]any{
		"provider":    strings.ToLower(provider),
		"link_type":   linkType,
		"external_id": externalID,
		"url":         rawURL,
	}

	if title := mcpsdk.ParseString(request, "title", ""); title != "" {
		reqBody["title"] = title
	}

	// status has no inferred default here — a caller who does not state it
	// gets whatever the API defaults to (open, for PR links). It exists so a
	// PR that was already merged before this call links it can be recorded
	// as such immediately: no GitHub webhook will ever arrive for a merge
	// that predates the link, so without this the row is stuck unresolvable
	// forever (#df734dd9) and the done-evidence gate blocks the task on it
	// permanently.
	if status := mcpsdk.ParseString(request, "status", ""); status != "" {
		reqBody["status"] = normalizeVCSLinkStatus(status)
	}

	result, err := s.getRESTClient(ctx).AddVCSLink(ctx, taskID, reqBody)
	if err != nil {
		return errResult("failed to add VCS link: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 12. list_comments
// ============================================================================

func (s *Server) handleListComments(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}

	params := map[string]string{}
	includeInternal := mcpsdk.ParseBoolean(request, "include_internal", true)
	if includeInternal {
		params["include_internal"] = "true"
	}

	limit := mcpsdk.ParseInt(request, "limit", 50)
	if limit > 0 {
		params["page_size"] = strconv.Itoa(limit)
	}

	result, err := s.getRESTClient(ctx).ListComments(ctx, taskID, params)
	if err != nil {
		return errResult("failed to list comments: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 13. upload_artifact
// ============================================================================

func (s *Server) handleUploadArtifact(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}

	name := mcpsdk.ParseString(request, "name", "")
	if name == "" {
		return errResult("name is required")
	}

	content := mcpsdk.ParseString(request, "content", "")
	if content == "" {
		return errResult("content is required")
	}

	mimeType := mcpsdk.ParseString(request, "mime_type", "")
	if mimeType == "" {
		mimeType = detectMIMEType(name)
	}

	artifactType := mcpsdk.ParseString(request, "artifact_type", "file")

	// metadata was declared on this tool and never read — the schema promised a field
	// the handler dropped. The API stores it verbatim from the "metadata" form field.
	metadataJSON := ""
	if md := mcpsdk.ParseStringMap(request, "metadata", nil); md != nil {
		raw, err := json.Marshal(md)
		if err != nil {
			return errResult("invalid metadata: %v", err)
		}
		metadataJSON = string(raw)
	}

	// Decode before anything else looks at the bytes: everything below reasons
	// about file content, not about the wire encoding it arrived in.
	data, err := decodeArtifactContent(content, mcpsdk.ParseString(request, "encoding", encodingText))
	if err != nil {
		return errResult("%v", err)
	}

	if err = verifyArtifactChecksum(mcpsdk.ParseString(request, "sha256", ""), data); err != nil {
		return errResult("%v", err)
	}

	// Refuse rather than store content that contradicts its declared type. The
	// bug this guards was invisible precisely because the upload succeeded.
	if err = validateArtifactMagic(mimeType, data); err != nil {
		return errResult("%v", err)
	}

	result, err := s.getRESTClient(ctx).UploadArtifact(ctx, taskID, name, artifactType, mimeType, metadataJSON, data)
	if err != nil {
		return errResult("failed to upload artifact: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 14. list_artifacts
// ============================================================================

func (s *Server) handleListArtifacts(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}

	result, err := s.getRESTClient(ctx).ListArtifacts(ctx, taskID)
	if err != nil {
		return errResult("failed to list artifacts: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 15. get_artifact
// ============================================================================

func (s *Server) handleGetArtifact(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	artifactID := mcpsdk.ParseString(request, "artifact_id", "")
	if artifactID == "" {
		return errResult("artifact_id is required")
	}

	artifact, err := s.getRESTClient(ctx).GetArtifact(ctx, artifactID)
	if err != nil {
		return errResult("failed to get artifact: %v", err)
	}

	resp := map[string]any{
		"artifact": artifact,
	}

	if mcpsdk.ParseBoolean(request, "include_content", false) {
		downloadURL, err := s.getRESTClient(ctx).GetArtifactDownloadURL(ctx, artifactID)
		if err != nil {
			resp["content_error"] = fmt.Sprintf("failed to get download URL: %v", err)
		} else {
			resp["download_url"] = downloadURL
		}
	}

	return jsonResult(resp)
}

// ============================================================================
// 16. publish_event
// ============================================================================

func (s *Server) handlePublishEvent(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}

	eventType := mcpsdk.ParseString(request, "event_type", "")
	if eventType == "" {
		return errResult("event_type is required")
	}

	subject := mcpsdk.ParseString(request, "subject", "")
	if subject == "" {
		return errResult("subject is required")
	}

	payload := mcpsdk.ParseStringMap(request, "payload", nil)
	if payload == nil {
		payload = map[string]any{}
	}

	ttlHours := mcpsdk.ParseInt(request, "ttl_hours", 24)

	body := map[string]any{
		"event_type":  eventType,
		"subject":     subject,
		"payload":     payload,
		"ttl_seconds": ttlHours * 3600,
	}

	if taskID := mcpsdk.ParseString(request, "task_id", ""); taskID != "" {
		body["task_id"] = taskID
	}
	if tags := parseStringSlice(request, "tags"); len(tags) > 0 {
		body["tags"] = tags
	}

	// Parse optional memory hint — passed through to the API for persistence.
	if memoryHint := request.GetArguments()["memory"]; memoryHint != nil {
		body["memory_hint"] = memoryHint
	}

	result, err := s.getRESTClient(ctx).PublishEvent(ctx, projectID, body)
	if err != nil {
		return errResult("failed to publish event: %v", err)
	}

	if warn := s.contextWarning(); warn != "" {
		result["_mesh_warning"] = warn
	}

	return jsonResult(result)
}

// ============================================================================
// 17. publish_summary
// ============================================================================

func (s *Server) handlePublishSummary(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}

	summary := mcpsdk.ParseString(request, "summary", "")
	if summary == "" {
		return errResult("summary is required")
	}

	payload := map[string]any{
		"summary":    summary,
		"agent_name": session.AgentName,
		"agent_type": session.AgentType,
	}

	if kd := parseStringSlice(request, "key_decisions"); len(kd) > 0 {
		payload["key_decisions"] = kd
	}
	if ac := parseStringSlice(request, "artifacts_created"); len(ac) > 0 {
		payload["artifacts_created"] = ac
	}
	if bl := parseStringSlice(request, "blockers"); len(bl) > 0 {
		payload["blockers"] = bl
	}
	if ns := parseStringSlice(request, "next_steps"); len(ns) > 0 {
		payload["next_steps"] = ns
	}
	if metrics := mcpsdk.ParseStringMap(request, "metrics", nil); metrics != nil {
		payload["metrics"] = metrics
	}

	body := map[string]any{
		"event_type":  "summary",
		"subject":     fmt.Sprintf("Work summary from %s", session.AgentName),
		"payload":     payload,
		"tags":        []string{"summary", session.AgentName},
		"ttl_seconds": 24 * 3600,
	}

	if taskID := mcpsdk.ParseString(request, "task_id", ""); taskID != "" {
		body["task_id"] = taskID
	}

	result, err := s.getRESTClient(ctx).PublishEvent(ctx, projectID, body)
	if err != nil {
		return errResult("failed to publish summary: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 18. get_context
// ============================================================================

func (s *Server) handleGetContext(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}

	params := map[string]string{}

	limit := mcpsdk.ParseInt(request, "limit", 50)
	if limit > 0 {
		params["page_size"] = strconv.Itoa(limit)
	}

	// The schema advertises `since` as an RFC3339 lower bound and the handler used to
	// drop it, so a caller narrowing the window silently got the default one instead.
	// The API spells the same filter `date_from` (listEventsQuery.DateFrom).
	if since := mcpsdk.ParseString(request, "since", ""); since != "" {
		if _, err := time.Parse(time.RFC3339, since); err != nil {
			return errResult("invalid since format, expected RFC3339: %v", err)
		}
		params["date_from"] = since
	}

	if eventTypes := parseStringSlice(request, "event_types"); len(eventTypes) > 0 {
		params["event_type"] = eventTypes[0]
	}

	if tags := parseStringSlice(request, "tags"); len(tags) > 0 {
		params["tags"] = tags[0]
	}

	result, err := s.getRESTClient(ctx).GetContext(ctx, projectID, params)
	if err != nil {
		return errResult("failed to get context: %v", err)
	}

	// Normalize to match expected format with events + count.
	resp := map[string]any{}
	if items, ok := result["items"]; ok {
		count := 0
		if arr, ok := items.([]any); ok {
			count = len(arr)
		}
		resp["events"] = items
		resp["count"] = count
	} else {
		// Pass through as-is if the response is already in the expected shape.
		for k, v := range result {
			resp[k] = v
		}
	}

	// Also fetch project knowledge and merge it (non-fatal if it fails).
	knowledge, knowledgeErr := s.getRESTClient(ctx).GetProjectKnowledge(ctx, projectID, 100, 0, 0, "")
	if knowledgeErr == nil {
		// Prefer the "items" slice if present, otherwise embed the full response.
		if items, ok := knowledge["items"]; ok {
			resp["project_knowledge"] = items
		} else {
			resp["project_knowledge"] = knowledge
		}
	}
	// knowledgeErr is intentionally ignored — context events are still useful without it.

	s.recordMemoryRead(ctx, "get_context")
	return jsonResult(resp)
}

// ============================================================================
// 19. get_task_context
// ============================================================================

func (s *Server) handleGetTaskContext(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}

	result, err := s.getRESTClient(ctx).GetTaskContext(ctx, taskID)
	if err != nil {
		return errResult("failed to get task context: %v", err)
	}

	// If the task is part of a recurring series, enrich the response with
	// schedule info and previous instance summary from the history endpoint.
	task, _ := result["task"].(map[string]any)
	if task != nil {
		if scheduleID, ok := task["recurring_schedule_id"].(string); ok && scheduleID != "" {
			// Fetch the most recent instances (page_size=2: current + previous).
			history, histErr := s.getRESTClient(ctx).GetRecurringHistory(ctx, scheduleID, map[string]string{
				"page_size": "2",
			})
			if histErr == nil {
				instanceNumber, _ := task["recurring_instance_number"].(float64)
				recurringBlock := map[string]any{
					"schedule_id":     scheduleID,
					"instance_number": int(instanceNumber),
					"history_url":     fmt.Sprintf("/api/v1/recurring/%s/history", scheduleID),
				}

				// Extract previous_instance from history items (skip current instance).
				if items, ok := history["items"].([]any); ok {
					for _, item := range items {
						inst, ok := item.(map[string]any)
						if !ok {
							continue
						}
						instNum, _ := inst["instance_number"].(float64)
						if int(instNum) < int(instanceNumber) {
							recurringBlock["previous_instance"] = inst
							break
						}
					}
				}

				result["recurring"] = recurringBlock
			}
		}
	}

	s.recordMemoryRead(ctx, "get_task_context")
	return jsonResult(result)
}

// ============================================================================
// 20. subscribe_events
// ============================================================================

func (s *Server) handleSubscribeEvents(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	projectID := mcpsdk.ParseString(request, "project_id", "")
	eventTypes := parseStringSlice(request, "event_types")
	callbackURL := mcpsdk.ParseString(request, "callback_url", "")

	// If callback_url is provided, persist it on the agent via PATCH /agents/me (self-service, no admin RBAC).
	if callbackURL != "" {
		client := s.getRESTClient(ctx)
		_, err := client.UpdateMe(ctx, map[string]any{
			"callback_url": callbackURL,
		})
		if err != nil {
			return errResult("failed to set callback_url: %v", err)
		}
	}

	baseURL := s.getRESTClient(ctx).BaseURL()

	return jsonResult(map[string]any{
		"status":       "configured",
		"agent_id":     session.AgentID.String(),
		"project_id":   projectID,
		"event_types":  eventTypes,
		"callback_url": callbackURL,
		"push_endpoints": map[string]any{
			"sse":       baseURL + "/api/v1/agents/me/events/stream",
			"long_poll": baseURL + "/api/v1/agents/me/tasks/poll?timeout=30",
		},
		"message": "Push notifications configured. Available mechanisms: (1) callback_url — Mesh POSTs events to your URL, (2) SSE — connect to events/stream for real-time, (3) long-poll — call tasks/poll or use the poll_tasks MCP tool to block until new assignment.",
	})
}

// ============================================================================
// 21. heartbeat
// ============================================================================

func (s *Server) handleHeartbeat(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	// Build heartbeat body from tool params.
	body := map[string]any{}
	if status := mcpsdk.ParseString(request, "status", ""); status != "" {
		body["status"] = status
	}
	if message := mcpsdk.ParseString(request, "message", ""); message != "" {
		body["message"] = message
	}
	if currentTaskID := mcpsdk.ParseString(request, "current_task_id", ""); currentTaskID != "" {
		body["current_task_id"] = currentTaskID
	}
	if args := request.GetArguments(); args != nil {
		if md, ok := args["metadata"]; ok && md != nil {
			body["metadata"] = md
		}
	}

	_, err := s.getRESTClient(ctx).Heartbeat(ctx, body)
	if err != nil {
		return errResult("heartbeat failed: %v", err)
	}

	return jsonResult(map[string]any{
		"status":       "ok",
		"agent_id":     session.AgentID.String(),
		"timestamp":    time.Now().UTC().Format(time.RFC3339),
		"mesh_version": BuildSHA,
	})
}

// ============================================================================
// 22. get_my_tasks
// ============================================================================

func (s *Server) handleGetMyTasks(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	params := map[string]string{}

	if projID := mcpsdk.ParseString(request, "project_id", ""); projID != "" {
		params["project_id"] = projID
	}
	if cat := mcpsdk.ParseString(request, "status_category", ""); cat != "" {
		params["status_category"] = cat
	}

	limit := mcpsdk.ParseInt(request, "limit", 50)
	if limit > 0 {
		params["page_size"] = strconv.Itoa(limit)
	}

	result, err := s.getRESTClient(ctx).GetAgentTasks(ctx, params)
	if err != nil {
		return errResult("failed to get tasks: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 23. report_error
// ============================================================================

func (s *Server) handleReportError(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	errorMessage := mcpsdk.ParseString(request, "error_message", "")
	if errorMessage == "" {
		return errResult("error_message is required")
	}

	severity := mcpsdk.ParseString(request, "severity", "medium")
	recoverable := mcpsdk.ParseBoolean(request, "recoverable", true)

	payload := map[string]any{
		"error_message": errorMessage,
		"severity":      severity,
		"recoverable":   recoverable,
		"agent_name":    session.AgentName,
		"agent_type":    session.AgentType,
	}

	if stackTrace := mcpsdk.ParseString(request, "stack_trace", ""); stackTrace != "" {
		payload["stack_trace"] = stackTrace
	}

	taskID := mcpsdk.ParseString(request, "task_id", "")

	// If we have a task_id, look up its project_id and publish an error event.
	var eventID string
	if taskID != "" {
		task, err := s.getRESTClient(ctx).GetTask(ctx, taskID)
		if err == nil {
			projectID, _ := task["project_id"].(string)
			if projectID != "" {
				eventBody := map[string]any{
					"event_type":  "error",
					"subject":     fmt.Sprintf("Error from %s: %s", session.AgentName, truncate(errorMessage, 100)),
					"payload":     payload,
					"tags":        []string{"error", severity},
					"ttl_seconds": 72 * 3600,
					"task_id":     taskID,
				}
				eventResult, pubErr := s.getRESTClient(ctx).PublishEvent(ctx, projectID, eventBody)
				if pubErr == nil {
					if id, ok := eventResult["id"].(string); ok {
						eventID = id
					}
				}
			}
		}
	}

	// Best-effort: update agent error status.
	if !recoverable {
		_, _ = s.getRESTClient(ctx).UpdateAgent(ctx, session.AgentID.String(), map[string]any{
			"status": "error",
		})
	}

	resp := map[string]any{
		"status":   "reported",
		"severity": severity,
	}
	if eventID != "" {
		resp["event_id"] = eventID
	}

	return jsonResult(resp)
}

// ============================================================================
// 24. register_sub_agent
// ============================================================================

func (s *Server) handleRegisterSubAgent(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	name := mcpsdk.ParseString(request, "name", "")
	if name == "" {
		return errResult("name is required")
	}

	agentType := mcpsdk.ParseString(request, "agent_type", "")
	if agentType == "" {
		return errResult("agent_type is required")
	}

	capabilities := mcpsdk.ParseStringMap(request, "capabilities", nil)

	result, err := s.getRESTClient(ctx).RegisterSubAgent(
		ctx,
		session.WorkspaceID.String(),
		session.AgentID.String(),
		name,
		agentType,
		capabilities,
	)
	if err != nil {
		return errResult("failed to register sub-agent: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 26. get_my_rules
// ============================================================================

func (s *Server) handleGetMyRules(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	wsID := session.WorkspaceID.String()
	projectID := mcpsdk.ParseString(request, "project_id", "")

	var path string
	if projectID != "" {
		path = fmt.Sprintf("/api/v1/projects/%s/rules/effective", projectID)
	} else {
		path = fmt.Sprintf("/api/v1/workspaces/%s/rules/effective", wsID)
	}

	result, err := s.getRESTClient(ctx).GetEffectiveRules(ctx, path)
	if err != nil {
		return errResult("failed to get rules: %v", err)
	}

	rules, _ := result["items"].([]interface{})
	summary := buildRulesSummary(rules)

	return jsonResult(map[string]any{
		"rules":   rules,
		"summary": summary,
		"count":   len(rules),
	})
}

// ============================================================================
// 27. get_project_rules
// ============================================================================

func (s *Server) handleGetProjectRules(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}

	result, err := s.getRESTClient(ctx).GetEffectiveRules(ctx, fmt.Sprintf("/api/v1/projects/%s/rules", projectID))
	if err != nil {
		return errResult("failed to get project rules: %v", err)
	}

	return jsonResult(result)
}

// buildRulesSummary generates a plain-English summary of effective rules for LLMs.
func buildRulesSummary(rules []interface{}) string {
	if len(rules) == 0 {
		return "No governance rules apply to you in this context."
	}

	summary := fmt.Sprintf("%d rule(s) apply: ", len(rules))
	for i, r := range rules {
		rMap, ok := r.(map[string]interface{})
		if !ok {
			continue
		}
		name, _ := rMap["name"].(string)
		enforcement, _ := rMap["enforcement"].(string)
		if i > 0 {
			summary += "; "
		}
		summary += fmt.Sprintf("%s (%s)", name, enforcement)
	}
	return summary
}

// ============================================================================
// 25. list_sub_agents
// ============================================================================

func (s *Server) handleListSubAgents(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	// agent_id defaults to the calling agent.
	agentID := mcpsdk.ParseString(request, "agent_id", "")
	if agentID == "" {
		agentID = session.AgentID.String()
	}

	recursive := mcpsdk.ParseBoolean(request, "recursive", false)

	agents, err := s.getRESTClient(ctx).ListSubAgents(ctx, agentID, recursive)
	if err != nil {
		return errResult("failed to list sub-agents: %v", err)
	}

	return jsonResult(map[string]any{
		"agents": agents,
		"count":  len(agents),
	})
}

// ============================================================================
// 28. get_team_directory
// ============================================================================

func (s *Server) handleGetTeamDirectory(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	result, err := s.getRESTClient(ctx).GetTeamDirectory(ctx, session.WorkspaceID.String())
	if err != nil {
		return errResult("failed to get team directory: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 29. get_assignment_rules
// ============================================================================

func (s *Server) handleGetAssignmentRules(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}

	result, err := s.getRESTClient(ctx).GetAssignmentRules(ctx, projectID)
	if err != nil {
		return errResult("failed to get assignment rules: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 30. get_workflow_rules
// ============================================================================

func (s *Server) handleGetWorkflowRules(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}

	result, err := s.getRESTClient(ctx).GetWorkflowRules(ctx, projectID)
	if err != nil {
		return errResult("failed to get workflow rules: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 31. update_agent_profile
// ============================================================================

func (s *Server) handleUpdateAgentProfile(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	args := request.GetArguments()
	body := map[string]any{}

	if _, ok := args["role"]; ok {
		body["role"] = mcpsdk.ParseString(request, "role", "")
	}
	if caps := parseStringSlice(request, "capabilities"); len(caps) > 0 {
		body["capabilities"] = caps
	}
	if _, ok := args["responsibility_zone"]; ok {
		body["responsibility_zone"] = mcpsdk.ParseString(request, "responsibility_zone", "")
	}
	if _, ok := args["escalation_to"]; ok {
		body["escalation_to"] = mcpsdk.ParseString(request, "escalation_to", "")
	}
	if accepts := parseStringSlice(request, "accepts_from"); len(accepts) > 0 {
		body["accepts_from"] = accepts
	}
	if _, ok := args["max_concurrent_tasks"]; ok {
		body["max_concurrent_tasks"] = mcpsdk.ParseInt(request, "max_concurrent_tasks", 0)
	}
	if _, ok := args["working_hours"]; ok {
		body["working_hours"] = mcpsdk.ParseString(request, "working_hours", "")
	}
	if _, ok := args["description"]; ok {
		body["description"] = mcpsdk.ParseString(request, "description", "")
	}

	// callback_url goes to PATCH /agents/me (self-service), not PUT /agents/:id/profile.
	var callbackURLUpdate bool
	if _, ok := args["callback_url"]; ok {
		callbackURLUpdate = true
	}

	if len(body) == 0 && !callbackURLUpdate {
		return errResult("no profile fields to update")
	}

	var profileResult map[string]any
	if len(body) > 0 {
		var err error
		profileResult, err = s.getRESTClient(ctx).UpdateAgentProfile(ctx, session.AgentID.String(), body)
		if err != nil {
			return errResult("failed to update agent profile: %v", err)
		}
	}

	// Persist callback_url via PATCH /agents/me.
	if callbackURLUpdate {
		cbURL := mcpsdk.ParseString(request, "callback_url", "")
		if _, err := s.getRESTClient(ctx).UpdateMe(ctx, map[string]any{"callback_url": cbURL}); err != nil {
			return errResult("failed to update callback_url: %v", err)
		}
		if profileResult == nil {
			profileResult = map[string]any{}
		}
		profileResult["callback_url"] = cbURL
	}

	return jsonResult(profileResult)
}

// ============================================================================
// 32. import_workspace_config
// ============================================================================

func (s *Server) handleImportWorkspaceConfig(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	yamlContent := mcpsdk.ParseString(request, "yaml_content", "")
	if yamlContent == "" {
		return errResult("yaml_content is required")
	}

	result, err := s.getRESTClient(ctx).ImportWorkspaceConfig(ctx, session.WorkspaceID.String(), yamlContent)
	if err != nil {
		return errResult("failed to import workspace config: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 33. export_workspace_config
// ============================================================================

func (s *Server) handleExportWorkspaceConfig(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	yamlContent, err := s.getRESTClient(ctx).ExportWorkspaceConfig(ctx, session.WorkspaceID.String())
	if err != nil {
		return errResult("failed to export workspace config: %v", err)
	}

	return jsonResult(map[string]any{
		"yaml_content": yamlContent,
	})
}

// ============================================================================
// 34. poll_tasks
// ============================================================================

func (s *Server) handlePollTasks(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	timeout := mcpsdk.ParseInt(request, "timeout", 30)
	if timeout < 1 {
		timeout = 1
	}
	if timeout > 120 {
		timeout = 120
	}

	result, err := s.getRESTClient(ctx).PollTasks(ctx, timeout)
	if err != nil {
		return errResult("poll_tasks failed: %v", err)
	}
	return jsonResult(result)
}

// ============================================================================
// 35. create_recurring_task
// ============================================================================

func (s *Server) handleCreateRecurringTask(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}

	titleTemplate := mcpsdk.ParseString(request, "title_template", "")
	if titleTemplate == "" {
		return errResult("title_template is required")
	}

	frequency := mcpsdk.ParseString(request, "frequency", "")
	if frequency == "" {
		return errResult("frequency is required")
	}

	body := map[string]any{
		"title_template": titleTemplate,
		"frequency":      frequency,
	}

	if desc := mcpsdk.ParseString(request, "description_template", ""); desc != "" {
		body["description_template"] = desc
	}
	if cronExpr := mcpsdk.ParseString(request, "cron_expr", ""); cronExpr != "" {
		body["cron_expr"] = cronExpr
	}
	if tz := mcpsdk.ParseString(request, "timezone", ""); tz != "" {
		body["timezone"] = tz
	}
	if assigneeID := mcpsdk.ParseString(request, "assignee_id", ""); assigneeID != "" {
		body["assignee_id"] = assigneeID
	}
	if assigneeType := mcpsdk.ParseString(request, "assignee_type", ""); assigneeType != "" {
		body["assignee_type"] = assigneeType
	}
	if priority := mcpsdk.ParseString(request, "priority", ""); priority != "" {
		body["priority"] = priority
	}
	if labels := parseStringSlice(request, "labels"); len(labels) > 0 {
		body["labels"] = labels
	}
	if startsAt := mcpsdk.ParseString(request, "starts_at", ""); startsAt != "" {
		body["starts_at"] = startsAt
	}
	if endsAt := mcpsdk.ParseString(request, "ends_at", ""); endsAt != "" {
		body["ends_at"] = endsAt
	}

	args := request.GetArguments()
	if _, ok := args["max_instances"]; ok {
		maxInstances := mcpsdk.ParseInt(request, "max_instances", 0)
		if maxInstances > 0 {
			body["max_instances"] = maxInstances
		}
	}

	result, err := s.getRESTClient(ctx).CreateRecurringSchedule(ctx, projectID, body)
	if err != nil {
		return errResult("failed to create recurring schedule: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 36. list_recurring_schedules
// ============================================================================

func (s *Server) handleListRecurringSchedules(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}

	activeOnly := mcpsdk.ParseBoolean(request, "active_only", true)

	params := map[string]string{}
	if activeOnly {
		params["is_active"] = "true"
	}

	result, err := s.getRESTClient(ctx).ListRecurringSchedules(ctx, projectID, params)
	if err != nil {
		return errResult("failed to list recurring schedules: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 37. get_recurring_history
// ============================================================================

func (s *Server) handleGetRecurringHistory(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	scheduleID := mcpsdk.ParseString(request, "recurring_schedule_id", "")
	if scheduleID == "" {
		return errResult("recurring_schedule_id is required")
	}

	limit := mcpsdk.ParseInt(request, "limit", 5)
	if limit < 1 {
		limit = 5
	}

	params := map[string]string{
		"page_size": strconv.Itoa(limit),
	}

	result, err := s.getRESTClient(ctx).GetRecurringHistory(ctx, scheduleID, params)
	if err != nil {
		return errResult("failed to get recurring history: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// 38. trigger_recurring_now
// ============================================================================

func (s *Server) handleTriggerRecurringNow(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	scheduleID := mcpsdk.ParseString(request, "recurring_schedule_id", "")
	if scheduleID == "" {
		return errResult("recurring_schedule_id is required")
	}

	result, err := s.getRESTClient(ctx).TriggerRecurringNow(ctx, scheduleID)
	if err != nil {
		return errResult("failed to trigger recurring schedule: %v", err)
	}

	return jsonResult(result)
}

func (s *Server) handleUpdateRecurringSchedule(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	scheduleID := mcpsdk.ParseString(request, "recurring_schedule_id", "")
	if scheduleID == "" {
		return errResult("recurring_schedule_id is required")
	}

	body := map[string]any{}
	if v := mcpsdk.ParseString(request, "title_template", ""); v != "" {
		body["title_template"] = v
	}
	if v := mcpsdk.ParseString(request, "description_template", ""); v != "" {
		body["description_template"] = v
	}
	if v := mcpsdk.ParseString(request, "frequency", ""); v != "" {
		body["frequency"] = v
	}
	if v := mcpsdk.ParseString(request, "cron_expr", ""); v != "" {
		body["cron_expr"] = v
	}
	if v := mcpsdk.ParseString(request, "timezone", ""); v != "" {
		body["timezone"] = v
	}
	if v := mcpsdk.ParseString(request, "assignee_id", ""); v != "" {
		body["assignee_id"] = v
	}
	if v := mcpsdk.ParseString(request, "assignee_type", ""); v != "" {
		body["assignee_type"] = v
	}
	if v := mcpsdk.ParseString(request, "priority", ""); v != "" {
		body["priority"] = v
	}
	if args := request.GetArguments(); args != nil {
		if v, ok := args["is_active"]; ok {
			body["is_active"] = v
		}
	}

	if len(body) == 0 {
		return errResult("at least one field to update is required")
	}

	result, err := s.getRESTClient(ctx).UpdateRecurringSchedule(ctx, scheduleID, body)
	if err != nil {
		return errResult("failed to update recurring schedule: %v", err)
	}

	return jsonResult(result)
}

func (s *Server) handleDeleteRecurringSchedule(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	scheduleID := mcpsdk.ParseString(request, "recurring_schedule_id", "")
	if scheduleID == "" {
		return errResult("recurring_schedule_id is required")
	}

	if err := s.getRESTClient(ctx).DeleteRecurringSchedule(ctx, scheduleID); err != nil {
		return errResult("failed to delete recurring schedule: %v", err)
	}

	return jsonResult(map[string]any{"deleted": true})
}

// ============================================================================
// Memory tools
// ============================================================================

func (s *Server) handleRecall(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	query := mcpsdk.ParseString(request, "query", "")
	if query == "" {
		return errResult("query is required")
	}

	scope := mcpsdk.ParseString(request, "scope", "")
	projectID := mcpsdk.ParseString(request, "project_id", "")
	tags := parseStringSlice(request, "tags")
	tagsAny := parseStringSlice(request, "tags_any")
	limit := mcpsdk.ParseInt(request, "limit", 10)
	offset := mcpsdk.ParseInt(request, "offset", 0)
	createdBy := mcpsdk.ParseString(request, "created_by", "")
	since := mcpsdk.ParseString(request, "since", "")
	until := mcpsdk.ParseString(request, "until", "")
	relevanceMin := mcpsdk.ParseFloat64(request, "relevance_min", 0)
	minImportance := mcpsdk.ParseFloat64(request, "min_importance", 0.4)
	applyDecay := mcpsdk.ParseBoolean(request, "apply_recency_decay", false)
	orderBy := mcpsdk.ParseString(request, "order_by", "")
	includeExpired := mcpsdk.ParseBoolean(request, "include_expired", false)
	includeArchived := mcpsdk.ParseBoolean(request, "include_archived", false)

	// Classify the query and apply profile-specific parameter presets.
	// An explicit recall_profile param overrides the auto-classifier.
	profile := ClassifyQuery(query)
	if explicit := mcpsdk.ParseString(request, "recall_profile", ""); explicit != "" {
		profile = RecallProfile(explicit)
	}
	pp := GetProfileParams(profile)
	if pp.ApplyDecay {
		applyDecay = true
	}
	if pp.MinImportance > 0 {
		minImportance = pp.MinImportance
	}
	// Same rule as the limit below, and for the same reason: a preset fills in
	// what the caller left unsaid, it does not overrule what the caller said.
	// This one was still an unconditional override — `factual` sets
	// "relevance:desc", so a caller who explicitly asked for
	// "decayed_relevance:desc" had it silently rewritten whenever the query
	// happened to be short and contain a UUID, a path or an env-var name.
	//
	// Harmless until 2026-08-09: that order_by armed nothing on the recall path.
	// evc-mesh#540 made "decayed_relevance:desc" arm time decay by itself, so
	// from that day the rewrite silently drops decay the caller asked for.
	orderBy = resolveProfileOrderBy(pp.OrderBy, orderBy)
	// A profile may widen the page only when the caller did not ask for a size.
	// Overriding an explicit limit made the parameter unpredictable: the same
	// recall(limit=6) returned 6 or 20 rows depending on whether the query text
	// happened to trip a keyword in the multi-session classifier, and nothing in
	// the response explained the difference.
	if pp.Limit > 0 && !hasArgument(request, "limit") {
		limit = pp.Limit
	}

	rp := RecallMemoriesParams{
		Query:             query,
		WorkspaceID:       session.WorkspaceID.String(),
		ProjectID:         projectID,
		Scope:             scope,
		Tags:              tags,
		TagsAny:           tagsAny,
		CreatedBy:         createdBy,
		Since:             since,
		Until:             until,
		RelevanceMin:      relevanceMin,
		ImportanceMin:     minImportance,
		ApplyRecencyDecay: applyDecay,
		HalfLifeDays:      pp.HalfLifeDays,
		OrderBy:           orderBy,
		IncludeExpired:    includeExpired,
		IncludeArchived:   includeArchived,
		Limit:             limit,
		Offset:            offset,
	}
	if pp.IncludeSuperseded {
		falseVal := false
		rp.ExcludeSuperseded = &falseVal
	}

	result, err := s.getRESTClient(ctx).RecallMemories(ctx, rp)
	if err != nil {
		return errResult("recall failed: %v", err)
	}

	// When RECALL_GRAPH_ENABLED=true, fire a secondary KG-expanded recall and
	// append any hop>0 items not already present in the base results.
	if os.Getenv("RECALL_GRAPH_ENABLED") == "true" {
		graphResult, graphErr := s.getRESTClient(ctx).RecallWithGraph(ctx, RecallWithGraphParams{
			Query:       query,
			WorkspaceID: session.WorkspaceID.String(),
			ProjectID:   projectID,
			// Reuse rp's already-parsed Scope/Tags/TagsAny rather than re-reading
			// them from the request — a second parse is a second place for the
			// two to silently disagree (task #37e9344c).
			Scope:           rp.Scope,
			Tags:            rp.Tags,
			TagsAny:         rp.TagsAny,
			Hops:            2,
			WeightThreshold: 0.1, // wide traversal: at 0.3 hop>0 items don't survive importance filter
			Limit:           50,  // request wider set so hop>0 neighbors are included
		})
		if graphErr == nil {
			result = mergeGraphResults(result, graphResult, limit)
		}
	}

	s.recordMemoryRead(ctx, "recall")
	return jsonResult(result)
}

// graphBoostReserve returns how many of the caller's `limit` slots may be spent on
// graph-expanded neighbours.
//
// Why a reserve rather than ranking the two sets together: base items carry `score`
// (RRF over the two retrieval arms) and graph neighbours carry `composite_score`
// from a separate traversal. They are different fields on different scales, and in
// practice every observed neighbour scores below every base hit — so sorting the
// union on a common key does not balance the two, it silently drops graph boost
// entirely. A reserve makes the trade explicit and tunable instead of an accident
// of score distributions.
//
// The reserve is a ceiling, not a quota: unused slots stay with the base results.
func graphBoostReserve(limit int) int {
	if limit < 2 {
		return 0 // never spend the caller's only slot on a neighbour
	}
	reserve := limit / 4
	if reserve < 1 {
		reserve = 1
	}
	return reserve
}

// mergeGraphResults folds hop>0 graph-expanded items into the base result, marking
// them with graph_boost=true.
//
// The merged result NEVER exceeds limit. Graph neighbours take the tail slots of
// the page, displacing the weakest base hits; they are not appended on top of a
// full page. Appending (as this did until #4c65d3e2) meant recall(limit=10) handed
// back 20 rows while the response still echoed "limit": 10 — the caller could not
// see that it had been overserved, and half of what arrived was not what it asked
// for. Every agent's mandatory wake-up recall paid that cost on every spawn.
func mergeGraphResults(base, graph map[string]any, limit int) map[string]any {
	baseItems, _ := base["items"].([]any)

	// Collect IDs already present in base result.
	seenIDs := make(map[string]bool, len(baseItems))
	for _, item := range baseItems {
		if m, ok := item.(map[string]any); ok {
			if id, ok2 := m["id"].(string); ok2 {
				seenIDs[id] = true
			}
		}
	}

	// Select eligible neighbours first, so the base page is only shortened by the
	// number of neighbours actually available.
	maxBoost := graphBoostReserve(limit)
	graphItems, _ := graph["items"].([]any)
	picked := make([]any, 0, maxBoost)
	for _, item := range graphItems {
		if len(picked) >= maxBoost {
			break // reserve exhausted — discard remaining graph-only items
		}
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		hopRaw := m["hop_distance"]
		hop, _ := hopRaw.(float64) // JSON numbers unmarshal as float64
		if hop <= 0 {
			continue // rrf-seeded hit already in base
		}
		id, _ := m["id"].(string)
		if seenIDs[id] {
			continue
		}
		m["graph_boost"] = true
		picked = append(picked, m)
		seenIDs[id] = true
	}

	if len(picked) == 0 {
		return base
	}

	// Make room for the neighbours rather than growing the page past the limit.
	if keep := limit - len(picked); len(baseItems) > keep {
		if keep < 0 {
			keep = 0
		}
		baseItems = baseItems[:keep]
	}
	merged := append(baseItems, picked...)

	// Belt and braces: whatever the arithmetic above, the caller's bound holds.
	if len(merged) > limit {
		merged = merged[:limit]
	}

	out := make(map[string]any, len(base))
	for k, v := range base {
		out[k] = v
	}
	out["items"] = merged
	out["total"] = len(merged)
	out["graph_boost_count"] = len(picked)
	return out
}

func (s *Server) handleRecallWithGraph(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}
	query := mcpsdk.ParseString(request, "q", "")
	if query == "" {
		return errResult("q (query) is required")
	}
	projectID := mcpsdk.ParseString(request, "project_id", "")
	taskID := mcpsdk.ParseString(request, "task_id", "")
	hops := int(mcpsdk.ParseFloat64(request, "hops", 2))
	weightThreshold := mcpsdk.ParseFloat64(request, "weight_threshold", 0.3)

	result, err := s.getRESTClient(ctx).RecallWithGraph(ctx, RecallWithGraphParams{
		Query:           query,
		WorkspaceID:     session.WorkspaceID.String(),
		ProjectID:       projectID,
		TaskID:          taskID,
		Hops:            hops,
		WeightThreshold: weightThreshold,
	})
	if err != nil {
		return errResult("recall_with_graph failed: %v", err)
	}
	return jsonResult(result)
}

func (s *Server) handleRemember(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	key := mcpsdk.ParseString(request, "key", "")
	content := mcpsdk.ParseString(request, "content", "")
	if key == "" || content == "" {
		return errResult("key and content are required")
	}

	scope := mcpsdk.ParseString(request, "scope", "project")
	projectID := mcpsdk.ParseString(request, "project_id", "")
	tags := parseStringSlice(request, "tags")
	relevance := mcpsdk.ParseFloat64(request, "relevance", 0)
	expiresAt := mcpsdk.ParseString(request, "expires_at", "")
	sourceURL := mcpsdk.ParseString(request, "source_url", "")
	sourceTaskID := mcpsdk.ParseString(request, "source_task_id", "")
	threadID := mcpsdk.ParseString(request, "thread_id", "")
	attachContext := mcpsdk.ParseBoolean(request, "attach_context", true)
	reason := mcpsdk.ParseString(request, "reason", "")
	// 0 means "not supplied": versions start at 1, so no real expectation can
	// be zero, and treating 0 as an expectation would turn an omitted argument
	// into a conditional write that always conflicts.
	expectedVersion := int(mcpsdk.ParseFloat64(request, "expected_version", 0))

	// Auto-populate project_id from the most recently checked-out task when the
	// agent omits it. This fixes the Memory Eval E·P2 issue where 99% of episodic
	// entries had project_id=NULL because agents didn't pass it explicitly.
	//
	// Gated to scope=="project" only (task #2c0154db/F3): identity now follows
	// declared scope (workspace -> (ws,key), project -> (ws,project,key)) since
	// evc-mesh#444/memory_service.go:488 narrowed the server-side twin of this
	// same auto-stamp the same way. Without this gate, a workspace-scope
	// remember() from inside a checked-out task silently gets a project_id it
	// never asked for, which is exactly the drift #4edf3fb5's collapse had to
	// clean up once (582 rows) and started regressing again within 2h of that
	// cleanup (2 rows) because only the server side had been fixed.
	if projectID == "" && scope == "project" {
		if stored, ok := s.activeProjects.Load(session.AgentID); ok {
			if pid, ok2 := stored.(string); ok2 {
				projectID = pid
			}
		}
	}
	// Auto-populate source_task_id + thread_id when attach_context=true (default).
	// Priority: fiddler side-channel file (survives MCP restart) → sync.Map fallback.
	// Pass attach_context=false for cross-cutting records not tied to the active task.
	if attachContext {
		fTaskID, fThreadID := readFiddlerContext()
		if sourceTaskID == "" {
			if fTaskID != "" {
				sourceTaskID = fTaskID
			} else if stored, ok := s.activeTaskIDs.Load(session.AgentID); ok {
				if tid, ok2 := stored.(string); ok2 {
					sourceTaskID = tid
				}
			}
		}
		if threadID == "" {
			threadID = fThreadID
		}
	}

	body := map[string]any{
		"workspace_id": session.WorkspaceID.String(),
		"key":          key,
		"content":      content,
		"scope":        scope,
		"tags":         tags,
		"source_type":  "agent",
	}
	if projectID != "" {
		body["project_id"] = projectID
	}
	if relevance > 0 {
		body["relevance"] = relevance
	}
	if expiresAt != "" {
		body["expires_at"] = expiresAt
	}
	if sourceURL != "" {
		body["source_url"] = sourceURL
	}
	if sourceTaskID != "" {
		body["source_task_id"] = sourceTaskID
	}
	if threadID != "" {
		body["thread_id"] = threadID
	}
	if reason != "" {
		body["reason"] = reason
	}
	if expectedVersion > 0 {
		body["expected_version"] = expectedVersion
	}

	result, err := s.getRESTClient(ctx).Remember(ctx, body)
	if err != nil {
		return errResult("remember failed: %v", err)
	}

	return jsonResult(result)
}

func (s *Server) handleSetProjectKnowledge(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}
	key := mcpsdk.ParseString(request, "key", "")
	if key == "" {
		return errResult("key is required")
	}
	value := mcpsdk.ParseString(request, "value", "")
	if value == "" {
		return errResult("value is required")
	}

	category := mcpsdk.ParseString(request, "category", "")
	tags := parseStringSlice(request, "tags")
	sourceURL := mcpsdk.ParseString(request, "source_url", "")
	sourceTaskID := mcpsdk.ParseString(request, "source_task_id", "")
	threadID := mcpsdk.ParseString(request, "thread_id", "")
	attachContext := mcpsdk.ParseBoolean(request, "attach_context", true)

	if attachContext {
		fTaskID, fThreadID := readFiddlerContext()
		if sourceTaskID == "" {
			if fTaskID != "" {
				sourceTaskID = fTaskID
			} else if stored, ok := s.activeTaskIDs.Load(session.AgentID); ok {
				if tid, ok2 := stored.(string); ok2 {
					sourceTaskID = tid
				}
			}
		}
		if threadID == "" {
			threadID = fThreadID
		}
	}

	body := map[string]any{
		"key":         key,
		"value":       value,
		"source_type": "agent",
	}
	if category != "" {
		body["category"] = category
	}
	if len(tags) > 0 {
		body["tags"] = tags
	}
	if sourceURL != "" {
		body["source_url"] = sourceURL
	}
	if sourceTaskID != "" {
		body["source_task_id"] = sourceTaskID
	}
	if threadID != "" {
		body["thread_id"] = threadID
	}

	result, err := s.getRESTClient(ctx).SetProjectKnowledge(ctx, projectID, body)
	if err != nil {
		return errResult("set_project_knowledge failed: %v", err)
	}

	return jsonResult(result)
}

func (s *Server) handleGetProjectKnowledge(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}

	limit := mcpsdk.ParseInt(request, "limit", 100)
	offset := mcpsdk.ParseInt(request, "offset", 0)
	minImportance := mcpsdk.ParseFloat64(request, "min_importance", 0)
	tagsAny := mcpsdk.ParseString(request, "tags_any", "")

	result, err := s.getRESTClient(ctx).GetProjectKnowledge(ctx, projectID, limit, offset, minImportance, tagsAny)
	if err != nil {
		return errResult("get_project_knowledge failed: %v", err)
	}

	s.recordMemoryRead(ctx, "get_project_knowledge")
	return jsonResult(result)
}

func (s *Server) handleForget(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	memoryID := mcpsdk.ParseString(request, "memory_id", "")
	if memoryID == "" {
		return errResult("memory_id is required")
	}

	if err := s.getRESTClient(ctx).ForgetMemory(ctx, memoryID); err != nil {
		return errResult("forget failed: %v", err)
	}

	return jsonResult(map[string]any{"deleted": true})
}

// ============================================================================
// checkout_task / release_task
// ============================================================================

func (s *Server) handleCheckoutTask(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}
	ttlMinutes := mcpsdk.ParseInt(request, "ttl_minutes", 120)
	if ttlMinutes <= 0 {
		ttlMinutes = 120
	}

	result, err := s.getRESTClient(ctx).CheckoutTask(ctx, taskID, ttlMinutes)
	if err != nil {
		return errResult("checkout_task failed: %v", err)
	}

	// Cache the token so release_task can forward it without requiring the
	// agent to track it manually (Option B — schema stays task_id-only).
	if token, ok := result["checkout_token"].(string); ok && token != "" {
		s.checkouts.Store(taskID, token)
	}

	// Track the checked-out task's project so handleRemember can auto-populate
	// project_id when the agent omits it (Memory Eval E·P2 fix).
	if session := s.getSession(ctx); session != nil {
		if projID, ok := result["project_id"].(string); ok && projID != "" {
			s.activeProjects.Store(session.AgentID, projID)
		}
		// Also store the task_id so handleRemember can auto-populate source_task_id,
		// which activates Amendment 2/3 KG edge hooks on subsequent remember() calls.
		s.activeTaskIDs.Store(session.AgentID, taskID)
	}

	return jsonResult(result)
}

func (s *Server) handleReleaseTask(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}

	token, ok := s.checkouts.Load(taskID)
	if !ok {
		return errResult("release_task: no checkout_token found for task %s — checkout may have been acquired in a different session or already released", taskID)
	}
	checkoutToken, _ := token.(string)

	if err := s.getRESTClient(ctx).ReleaseTask(ctx, taskID, checkoutToken); err != nil {
		return errResult("release_task failed: %v", err)
	}
	s.checkouts.Delete(taskID)

	return jsonResult(map[string]any{"released": true, "task_id": taskID})
}

func (s *Server) handleExtendCheckout(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}
	ttlMinutes := mcpsdk.ParseInt(request, "ttl_minutes", 120)
	if ttlMinutes <= 0 {
		ttlMinutes = 120
	}

	token, ok := s.checkouts.Load(taskID)
	if !ok {
		return errResult("extend_checkout: no checkout_token found for task %s — checkout may have been acquired in a different session, already released, or already expired", taskID)
	}
	checkoutToken, _ := token.(string)

	result, err := s.getRESTClient(ctx).ExtendCheckout(ctx, taskID, checkoutToken, ttlMinutes)
	if err != nil {
		return errResult("extend_checkout failed: %v", err)
	}

	return jsonResult(result)
}

// ============================================================================
// session_report
// ============================================================================

func (s *Server) handleSessionReport(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	model := mcpsdk.ParseString(request, "model", "")
	tokensIn := int64(mcpsdk.ParseFloat64(request, "tokens_in", 0))
	tokensOut := int64(mcpsdk.ParseFloat64(request, "tokens_out", 0))
	cost := mcpsdk.ParseFloat64(request, "estimated_cost", 0)

	stats := s.tracker.Stats()
	if model != "" {
		stats["model_used"] = model
	}
	if tokensIn > 0 {
		stats["tokens_in"] = tokensIn
	}
	if tokensOut > 0 {
		stats["tokens_out"] = tokensOut
	}
	if cost > 0 {
		stats["estimated_cost"] = cost
	}

	score, detail := s.tracker.ComplianceScore()
	stats["compliance_score"] = score
	stats["compliance_detail"] = detail

	// Persist usage onto the agent's active session in agent_sessions.
	// Errors are non-fatal: include them in the response so the caller can inspect,
	// but don't prevent the stats from being returned.
	if rc := s.getRESTClient(ctx); rc != nil {
		persisted, err := rc.ReportSession(ctx, tokensIn, tokensOut, model, cost)
		if err != nil {
			stats["persist_error"] = err.Error()
		} else {
			stats["persisted"] = true
			if totals, ok := persisted["totals"]; ok {
				stats["session_totals"] = totals
			}
			if sessionID, ok := persisted["session_id"]; ok {
				stats["session_id"] = sessionID
			}
		}
	}

	return jsonResult(stats)
}

// ============================================================================
// pavel_decision / get_canonical_updates
// ============================================================================

// secretPattern matches text that should be auto-flagged privacy:private.
// Matches: password/token/secret/bearer/api-key keywords OR continuous hex ≥40 chars.
var secretPattern = regexp.MustCompile(`(?i)\b(password|api[_-]?key|secret|bearer|private[_-]?key)\b|agk_\w+|[0-9a-fA-F]{40,}`)

// containsSecret reports whether text matches a secret/credential pattern.
func containsSecret(text string) bool {
	return secretPattern.MatchString(text)
}

// slugify converts a summary string to a URL-safe slug (max 50 chars).
func slugify(s string) string {
	s = strings.ToLower(s)
	s = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			return r
		}
		return '-'
	}, s)
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if len(s) > 50 {
		s = s[:50]
	}
	return s
}

func (s *Server) handlePavelDecision(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	text := mcpsdk.ParseString(request, "text", "")
	summary := mcpsdk.ParseString(request, "summary", "")
	if text == "" || summary == "" {
		return errResult("text and summary are required")
	}

	projectID := mcpsdk.ParseString(request, "scope", "")
	privacy := mcpsdk.ParseString(request, "privacy", "public")
	propagateTo := parseStringSlice(request, "propagate_to")

	// Backstop: auto-flag private if text matches secret pattern.
	if privacy == "public" && containsSecret(text) {
		privacy = "private"
	}

	day := time.Now().UTC().Format("2006-01-02")
	tags := []string{
		"kind:canonical-decision",
		"owner:riker",
		"source:pavel-tg",
		"privacy:" + privacy,
	}
	for _, target := range propagateTo {
		if target != "" {
			tags = append(tags, "propagate_to:"+target)
		}
	}

	key := "canonical-decision-" + day + "-" + slugify(summary)

	bodyScope := "workspace"
	body := map[string]any{
		"workspace_id": session.WorkspaceID.String(),
		"key":          key,
		"content":      text,
		"scope":        bodyScope,
		"tags":         tags,
	}
	if projectID != "" {
		body["project_id"] = projectID
		body["scope"] = "project"
	}

	result, err := s.getRESTClient(ctx).Remember(ctx, body)
	if err != nil {
		return errResult("pavel_decision failed: %v", err)
	}

	var id, recordedAt string
	if mem, ok := result["memory"].(map[string]any); ok {
		if v, ok2 := mem["id"].(string); ok2 {
			id = v
		}
		if v, ok2 := mem["created_at"].(string); ok2 {
			recordedAt = v
		}
	}

	resp := map[string]any{
		"id":          id,
		"recorded_at": recordedAt,
		"affects":     propagateTo,
		"privacy":     privacy,
		"key":         key,
	}

	// Optional: link this decision to a gated task (docs/human-gate-decision-recorded.md
	// in evc-mesh). Best-effort — the canonical write above already succeeded, so a
	// failure here is reported alongside it rather than discarding the canon record.
	// Omitting task_id leaves behavior identical to before this field existed.
	if taskID := mcpsdk.ParseString(request, "task_id", ""); taskID != "" {
		decidedBy, pavelErr := s.resolvePavelUserID(ctx)
		if pavelErr != nil {
			resp["human_gate_decision_error"] = fmt.Sprintf("could not resolve Pavel's user id: %v", pavelErr)
		} else {
			decision, hgdErr := s.getRESTClient(ctx).CreateHumanGateDecision(ctx, taskID, map[string]any{
				"canonical_key": key,
				"decided_by":    decidedBy,
				"provenance":    "attested",
				"channel":       "telegram",
				"quote":         text,
			})
			if hgdErr != nil {
				resp["human_gate_decision_error"] = hgdErr.Error()
			} else {
				resp["human_gate_decision"] = decision
			}
		}
	}

	return jsonResult(resp)
}

// resolvePavelUserID finds Pavel's user UUID from the workspace team
// directory, for decided_by on a human_gate decision record (contract
// docs/human-gate-decision-recorded.md §3 in evc-mesh: "decided_by — человек,
// принявший решение, сегодня — user-id Pavel'я"). Prefers username=="pavel";
// falls back to role=="owner" if the username ever changes.
func (s *Server) resolvePavelUserID(ctx context.Context) (string, error) {
	session := s.getSession(ctx)
	if session == nil {
		return "", fmt.Errorf("not authenticated: no agent session")
	}
	dir, err := s.getRESTClient(ctx).GetTeamDirectory(ctx, session.WorkspaceID.String())
	if err != nil {
		return "", err
	}
	humans, _ := dir["humans"].([]any)
	var ownerID string
	for _, h := range humans {
		hm, ok := h.(map[string]any)
		if !ok {
			continue
		}
		id, _ := hm["id"].(string)
		if id == "" {
			continue
		}
		if username, _ := hm["username"].(string); username == "pavel" {
			return id, nil
		}
		if ownerID == "" {
			if role, _ := hm["role"].(string); role == "owner" {
				ownerID = id
			}
		}
	}
	if ownerID != "" {
		return ownerID, nil
	}
	return "", fmt.Errorf("no user with username=pavel or role=owner found in team directory")
}

func (s *Server) handleGetCanonicalUpdates(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	since := mcpsdk.ParseString(request, "since", "")
	agentSlug := mcpsdk.ParseString(request, "agent", "")
	scope := mcpsdk.ParseString(request, "scope", "")

	params := map[string]string{
		"since": since,
		"agent": agentSlug,
		"scope": scope,
	}

	result, err := s.getRESTClient(ctx).GetCanonicalUpdates(ctx, params)
	if err != nil {
		return errResult("get_canonical_updates failed: %v", err)
	}

	s.recordMemoryRead(ctx, "get_canonical_updates")
	return jsonResult(result)
}

// ============================================================================
// get_canonical — canonical knowledge layer read tool (Memory E3 / C2)
// ============================================================================

// canonicalEntry is a single result record returned by get_canonical.
type canonicalEntry struct {
	Source    string `json:"source"`
	Key       string `json:"key"`
	Content   string `json:"content"`
	UpdatedAt string `json:"updated_at"`
	Project   string `json:"project,omitempty"`
}

// slugVariants returns all known slug aliases for a project so that workspace
// memory queries find records fragmented across multiple slug labels.
// Source: Phase A audit — one logical project written under 2-3 slug variants.
var slugVariantTable = map[string][]string{
	"evc-mesh":       {"evc-mesh", "mesh-dev", "mesh"},
	"mesh-dev":       {"evc-mesh", "mesh-dev", "mesh"},
	"mesh":           {"evc-mesh", "mesh-dev", "mesh"},
	"evc-spark":      {"evc-spark", "spark"},
	"spark":          {"evc-spark", "spark"},
	"evc-team-relay": {"evc-team-relay", "team-relay"},
	"team-relay":     {"evc-team-relay", "team-relay"},
}

func slugVariants(slug string) []string {
	if v, ok := slugVariantTable[slug]; ok {
		return v
	}
	return []string{slug}
}

// canonicalSlug normalises known project slug aliases to a primary slug.
func canonicalSlug(slug string) string {
	aliases := map[string]string{
		"mesh-dev":   "evc-mesh",
		"mesh":       "evc-mesh",
		"spark":      "evc-spark",
		"team-relay": "evc-team-relay",
	}
	if c, ok := aliases[slug]; ok {
		return c
	}
	return slug
}

// resolveProjectID finds the UUID of the first project whose slug matches any
// of the given variants by listing projects in the workspace.
func resolveProjectID(ctx context.Context, client *RESTClient, workspaceID string, slugs []string) string {
	result, err := client.ListProjects(ctx, workspaceID, false)
	if err != nil {
		return ""
	}
	projects, _ := result["projects"].([]any)
	slugSet := make(map[string]struct{}, len(slugs))
	for _, s := range slugs {
		slugSet[s] = struct{}{}
	}
	for _, p := range projects {
		proj, ok := p.(map[string]any)
		if !ok {
			continue
		}
		for _, field := range []string{"slug", "name"} {
			if v, ok := proj[field].(string); ok {
				if _, found := slugSet[strings.ToLower(v)]; found {
					if id, ok := proj["id"].(string); ok {
						return id
					}
				}
			}
		}
	}
	return ""
}

// stringsFromAnyMap extracts a []string from a []any field on a map[string]any.
func stringsFromAnyMap(m map[string]any, field string) []string {
	raw, _ := m[field].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// projectTagFromMemory returns the first "project:<slug>" tag value in a memory map.
func projectTagFromMemory(m map[string]any) string {
	for _, t := range stringsFromAnyMap(m, "tags") {
		if strings.HasPrefix(t, "project:") {
			return strings.TrimPrefix(t, "project:")
		}
	}
	return ""
}

// memoryTimestamp returns the best available timestamp from a memory map.
func memoryTimestamp(m map[string]any) string {
	for _, f := range []string{"updated_at", "created_at", "ts"} {
		if v, ok := m[f].(string); ok && v != "" {
			return v
		}
	}
	return ""
}

func (s *Server) handleGetCanonical(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}

	topic := mcpsdk.ParseString(request, "topic", "")
	if topic == "" {
		return errResult("topic is required")
	}
	projectSlug := mcpsdk.ParseString(request, "project", "")
	client := s.getRESTClient(ctx)
	wsID := session.WorkspaceID.String()

	var results []canonicalEntry
	seen := make(map[string]struct{})
	dedupKey := func(src, key string) string { return src + "\x00" + key }

	// --- Source 1: workspace_memories tagged kind:canonical ---
	// Filtering for kind:canonical implicitly excludes session-checkpoint entries
	// (those are tagged kind:session-checkpoint, not kind:canonical).
	wsTags := []string{"kind:canonical"}
	if projectSlug != "" {
		// Expand all slug variants so fragmented entries are not silently missed.
		for _, v := range slugVariants(projectSlug) {
			wsTags = append(wsTags, "project:"+v)
		}
	}
	wsMemories, wsErr := client.RecallMemories(ctx, RecallMemoriesParams{
		Query:             topic,
		WorkspaceID:       wsID,
		TagsAny:           wsTags,
		ApplyRecencyDecay: true,
		OrderBy:           "relevance:desc",
		Limit:             25,
	})
	if wsErr == nil {
		memories, _ := wsMemories["memories"].([]any)
		for _, raw := range memories {
			m, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			// Defensive guard: never surface session-checkpoints.
			if kind, _ := m["kind"].(string); kind == "session-checkpoint" {
				continue
			}
			// When a project filter is active, tags_any may have matched on a
			// "project:<slug>" tag without kind:canonical. Keep only canonical entries.
			if projectSlug != "" {
				isCanonical := false
				if kind, _ := m["kind"].(string); kind == "canonical" {
					isCanonical = true
				}
				if !isCanonical {
					for _, t := range stringsFromAnyMap(m, "tags") {
						if t == "kind:canonical" {
							isCanonical = true
							break
						}
					}
				}
				if !isCanonical {
					continue
				}
			}
			key, _ := m["key"].(string)
			content, _ := m["content"].(string)
			dk := dedupKey("workspace_memories", key)
			if _, dup := seen[dk]; dup {
				continue
			}
			seen[dk] = struct{}{}
			results = append(results, canonicalEntry{
				Source:    "workspace_memories",
				Key:       key,
				Content:   content,
				UpdatedAt: memoryTimestamp(m),
				Project:   canonicalSlug(projectTagFromMemory(m)),
			})
		}
	}

	// --- Source 2: project_memories (key LIKE canonical:% or kind:canonical) ---
	// Requires resolving the project slug to a UUID via ListProjects.
	// Skipped gracefully if slug is absent, unresolvable, or GetProjectKnowledge fails.
	if projectSlug != "" {
		projID := resolveProjectID(ctx, client, wsID, slugVariants(projectSlug))
		if projID != "" {
			pkResult, pkErr := client.GetProjectKnowledge(ctx, projID, 100, 0, 0, "")
			if pkErr == nil {
				topicLower := strings.ToLower(topic)
				pms, _ := pkResult["project_memories"].([]any)
				for _, raw := range pms {
					pm, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					key, _ := pm["key"].(string)
					kind, _ := pm["kind"].(string)
					if !strings.HasPrefix(key, "canonical:") && kind != "canonical" {
						continue
					}
					// Basic topic relevance check against key + content.
					content, _ := pm["content"].(string)
					if content == "" {
						content, _ = pm["value"].(string)
					}
					if topicLower != "" && !strings.Contains(strings.ToLower(key+" "+content), topicLower) {
						continue
					}
					dk := dedupKey("project_memories", key)
					if _, dup := seen[dk]; dup {
						continue
					}
					seen[dk] = struct{}{}
					results = append(results, canonicalEntry{
						Source:    "project_memories",
						Key:       key,
						Content:   content,
						UpdatedAt: memoryTimestamp(pm),
						Project:   canonicalSlug(projectSlug),
					})
				}
			}
		}
	}

	merged := buildCanonicalMarkdown(results, topic)
	s.recordMemoryRead(ctx, "get_canonical")
	return jsonResult(map[string]any{
		"topic":           topic,
		"count":           len(results),
		"results":         results,
		"merged_markdown": merged,
	})
}

// buildCanonicalMarkdown renders canonical entries as a readable markdown document.
func buildCanonicalMarkdown(entries []canonicalEntry, topic string) string {
	if len(entries) == 0 {
		return fmt.Sprintf("No canonical entries found for topic: %s\n", topic)
	}
	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("# Canonical knowledge: %s\n\n", topic))
	sb.WriteString(fmt.Sprintf("*%d record(s)*\n\n", len(entries)))
	for _, e := range entries {
		sb.WriteString(fmt.Sprintf("## %s\n", e.Key))
		proj := e.Project
		if proj == "" {
			proj = "—"
		}
		sb.WriteString(fmt.Sprintf("_source: %s · project: %s · updated: %s_\n\n", e.Source, proj, e.UpdatedAt))
		sb.WriteString(e.Content)
		sb.WriteString("\n\n---\n\n")
	}
	return sb.String()
}
