package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

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
	if err := validateListTasksArguments(request); err != nil {
		return errResult("%v", err)
	}
	projectID := mcpsdk.ParseString(request, "project_id", "")
	workspaceID := mcpsdk.ParseString(request, "workspace_id", "")

	if projectID == "" && workspaceID == "" {
		return errResult("project_id or workspace_id is required")
	}

	params := map[string]string{}
	if cat := mcpsdk.ParseString(request, "status_category", ""); cat != "" {
		params["status_category"] = cat
	}
	if assignee, err := s.listTasksAssignee(ctx, request); err != nil {
		return errResult("%v", err)
	} else if assignee != "" {
		params["assignee_id"] = assignee
	}

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
	// `order` and `page` were absent from this tool's schema entirely, not
	// merely dropped in transit — the REST layer has honoured `sort_dir`/`order`
	// and `page` all along (pagination.Params). The visible cost was not a
	// missing convenience: a project larger than `limit` answered every
	// "what changed in the last day" walk with its OLDEST tasks and an
	// otherwise well-formed envelope, so the caller read "nothing changed".
	// Worse, that envelope reports total_pages, advertising pages this tool
	// gave no way to reach.
	//
	// Deliberately NOT validated here. The API already refuses a bad direction
	// by name, and a second copy of that rule in this layer is one more thing
	// to drift out of step with it.
	if order := mcpsdk.ParseString(request, "order", ""); order != "" {
		params["order"] = order
	}
	if page := mcpsdk.ParseInt(request, "page", 0); page > 0 {
		params["page"] = strconv.Itoa(page)
	}

	limit := mcpsdk.ParseInt(request, "limit", 20)
	full := mcpsdk.ParseBoolean(request, "full", false)
	// Wave 2 (#dc719c63): the compact view caps the page at
	// listTasksCompactLimitCeiling items. A live probe measured limit=200
	// handing back 118k chars of lean items — the single biggest list_tasks
	// response in the fleet — while no routing decision needs 200 cards at
	// once. full=true keeps the API's own 200 ceiling, and a clamped call
	// says so in the response (limit_clamped_to) so paging math stays honest.
	limitClamped := false
	if !full && limit > listTasksCompactLimitCeiling {
		limit = listTasksCompactLimitCeiling
		limitClamped = true
	}
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
		page, err := s.compactListTasksPage(ctx, result, full, "")
		if err != nil {
			return errResult("%v", err)
		}
		if limitClamped {
			noteLimitClamp(page)
		}
		if !full {
			capListTasksPage(page, limit, mcpsdk.ParseInt(request, "page", 1))
		}
		return jsonResult(page)
	}

	result, err := s.getRESTClient(ctx).ListTasks(ctx, projectID, params)
	if err != nil {
		return errResult("failed to list tasks: %v", err)
	}

	page, err := s.compactListTasksPage(ctx, result, full, projectID)
	if err != nil {
		return errResult("%v", err)
	}
	if limitClamped {
		noteLimitClamp(page)
	}
	if !full {
		capListTasksPage(page, limit, mcpsdk.ParseInt(request, "page", 1))
	}
	return jsonResult(page)
}

// listTasksCompactLimitCeiling is the per-page item ceiling in list_tasks'
// default (compact) view. full=true restores the API's own 200-item page.
const listTasksCompactLimitCeiling = 50

// noteLimitClamp stamps the clamp onto a response page so a caller paging
// with a larger limit sees why their pages shrank instead of silently
// overlapping.
func noteLimitClamp(page map[string]any) {
	page["limit_clamped_to"] = listTasksCompactLimitCeiling
	page["limit_note"] = fmt.Sprintf(
		"compact view caps limit at %d; pass full=true for up to 200 per page",
		listTasksCompactLimitCeiling)
}

// ============================================================================
// 4. get_task
// ============================================================================

func (s *Server) handleGetTask(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}

	var since time.Time
	sinceSet := false
	if sinceStr := mcpsdk.ParseString(request, "since", ""); sinceStr != "" {
		t, err := time.Parse(time.RFC3339, sinceStr)
		if err != nil {
			return errResult("invalid since format, expected RFC3339 (e.g. 2026-09-30T12:00:00Z): %v", err)
		}
		since = t
		sinceSet = true
	}

	task, err := s.getRESTClient(ctx).GetTask(ctx, taskID)
	if err != nil {
		return errResult("failed to get task: %v", err)
	}

	// Child reads below must go through the canonical UUID from the resolved
	// task object, not the caller's raw task_id. GetTask maps a 6-12 hex
	// short-ID prefix onto the task via /tasks/by-short-id/:prefix, but the
	// sub-resource routes are not uniform in what they accept: /dependencies
	// parses the path id as a strict UUID and answers 400 "invalid task_id"
	// for the short form (live red 02.10: get_task('06d0c6d8',
	// include_dependencies=true) on a task whose UUID form works). Using the
	// resolved id everywhere makes every include_* uniform no matter what
	// each backend route tolerates on its own.
	resolvedID, _ := task["id"].(string)
	if resolvedID == "" {
		// Malformed backend response (no id on the task object): keep the
		// historical behaviour for full-UUID callers rather than fail closed.
		resolvedID = taskID
	}

	// Default view drops envelope fields; descriptions are always complete.
	// full=true keeps the complete REST object. Applied before the
	// since-branching so both the plain and the changed-since paths return
	// the same shape.
	full := mcpsdk.ParseBoolean(request, "full", false)
	if !full {
		s.compactTaskStatus(ctx, task, resolvedID)
		leanGetTaskView(task)
	}

	resp := map[string]any{}

	// Without `since`, behave exactly as before: the full task, every call.
	// With it, the caller already paid for the static fields (description
	// and the rest) in an earlier call this session — only repay that cost
	// when the task's own updated_at proves something on it actually
	// changed; otherwise hand back task_changed=false and a trimmed stub
	// instead of the full object. 1004 of 4940 get_task calls 23-26.09 were
	// a repeat of the same task in the same session, paying full price for
	// zero new information.
	if !sinceSet {
		resp["task"] = task
	} else if taskChangedSince(task, since) {
		resp["task"] = task
		resp["task_changed"] = true
	} else {
		resp["task"] = taskDeltaStub(task)
		resp["task_changed"] = false
	}

	if mcpsdk.ParseBoolean(request, "include_comments", false) {
		// The default tail length is small on purpose: self-reports of 20
		// lanes (03.10) named re-reading whole threads as the top context cost
		// — one get_task(include_comments=true) on a busy card handed back the
		// newest 50 comments whole (~100KB on the worst cards measured). The
		// last few comments carry the state an agent needs before acting; the
		// full thread stays one explicit comments_limit away, and
		// comments_total_count/comments_has_more always say how much is hidden.
		commentsLimit := mcpsdk.ParseInt(request, "comments_limit", defaultTaskCommentsLimit)
		if commentsLimit < 1 || commentsLimit > maxTaskCommentsLimit {
			return errResult("comments_limit must be between 1 and %d, got %d", maxTaskCommentsLimit, commentsLimit)
		}
		page, err := s.getRESTClient(ctx).GetTaskComments(ctx, resolvedID, commentsLimit)
		if err != nil {
			return errResult("failed to list comments: %v", err)
		}
		var itemCount int
		if items, ok := page["items"]; ok {
			arr, _ := items.([]any)
			if sinceSet {
				arr = commentsSince(arr, since)
			}
			// Inline comments share the list_comments compact shape in the
			// default view (wave 2, #dc719c63): id/author_name/created_at/
			// complete body. full=true returns complete REST objects.
			if !full {
				arr = compactCommentItems(arr, mcpsdk.ParseBoolean(request, "include_auto", false))
			}
			resp["comments"] = arr
			itemCount = len(arr)
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
				"showing the last %d of %d comments; raise comments_limit (max %d) for more of the tail, or read the rest by paging list_comments(task_id, order=desc, page=N) — a thread longer than %d cannot be fetched in one call",
				itemCount, int(totalCount), maxTaskCommentsLimit, maxTaskCommentsLimit)
		}
	}

	if mcpsdk.ParseBoolean(request, "include_artifacts", false) {
		page, err := s.getRESTClient(ctx).GetTaskArtifacts(ctx, resolvedID)
		if err != nil {
			return errResult("failed to list artifacts: %v", err)
		}
		var itemCount int
		if items, ok := page["items"]; ok {
			resp["artifacts"] = shapeArtifactList(items)
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
		deps, err := s.getRESTClient(ctx).GetTaskDependencies(ctx, resolvedID)
		if err != nil {
			return errResult("failed to list dependencies: %v", err)
		}
		// dependencies = this task's own blockers (outgoing), matching the
		// semantics callers historically got from the bare-array response.
		resp["dependencies"] = deps.Outgoing
		resp["dependencies_incoming"] = deps.Incoming
		if !full {
			resp["dependencies"] = compactDependencies(deps.Outgoing, false)
			resp["dependencies_incoming"] = compactDependencies(deps.Incoming, true)
		}
	}

	if mcpsdk.ParseBoolean(request, "include_vcs_links", false) {
		page, err := s.getRESTClient(ctx).GetTaskVCSLinks(ctx, resolvedID)
		if err != nil {
			return errResult("failed to list vcs links: %v", err)
		}
		// REST returns {"vcs_links": [...], "count": N} — no pagination
		// envelope to strip (unlike comments/artifacts), the endpoint
		// always returns the full set.
		if links, ok := page["vcs_links"]; ok {
			resp["vcs_links"] = links
		} else {
			resp["vcs_links"] = []any{}
		}
	}

	return jsonResult(resp)
}

// taskChangedSince reports whether task's own updated_at is after since. A
// missing or unparsable updated_at fails open (returns true): never suppress
// data we can't actually judge as stale, only ever suppress data we can
// positively confirm is unchanged.
func taskChangedSince(task map[string]any, since time.Time) bool {
	raw, _ := task["updated_at"].(string)
	if raw == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return true
	}
	return t.After(since)
}

// taskDeltaStub is what get_task(since=...) returns in place of the full task
// when taskChangedSince says nothing changed: enough to confirm identity and
// re-anchor the caller's next `since`, without repaying for the static
// fields (description, custom_fields, ...) it already has from an earlier
// call this session.
func taskDeltaStub(task map[string]any) map[string]any {
	stub := map[string]any{}
	for _, f := range []string{"id", "status", "status_id", "status_lookup_error", "assignee_id", "assignee_name", "checked_out_by", "human_gate", "updated_at"} {
		if v, ok := task[f]; ok {
			stub[f] = v
		}
	}
	return stub
}

// defaultTaskCommentsLimit is how many of the newest comments
// get_task(include_comments=true) inlines by default. Self-reports of 20 lanes
// (03.10, #4094eeb1) named re-reading whole threads as the top context cost:
// the previous default (the server's 50, whole) put ~100KB into a lane's
// context on the worst cards. The tail is where an acting agent looks first;
// comments_total_count/comments_has_more keep the hidden rest visible.
const defaultTaskCommentsLimit = 5

// leanGetTaskView removes envelope fields, preserving all decision text.
func leanGetTaskView(task map[string]any) {
	keep := map[string]bool{}
	for _, k := range []string{"id", "title", "status", "status_id", "status_lookup_error", "priority", "assignee_name", "labels", "parent_task_id", "start_after", "updated_at", "description", "human_gate", "custom_fields", "dod_checks"} {
		keep[k] = true
	}
	for k := range task {
		if !keep[k] || (isEmptyJSONValue(task[k]) && k != "description" && k != "id" && k != "title") {
			delete(task, k)
		}
	}
}

// maxTaskCommentsLimit bounds the explicit "give me more of the tail" escape
// hatch to what the API's own page cap serves in one request — a thread
// longer than this is only readable end-to-end by paging list_comments,
// never by one get_task call.
const maxTaskCommentsLimit = 200

// leanMutationTaskFields is what a mutation response keeps by default: enough
// to confirm the write landed and see where the card now routes (status,
// assignee — including move-to-review's auto-reassign), nothing else. The
// description and the other static fields are the fat the caller either just
// sent (update_task) or can get with one get_task — echoing them on every
// mutation cost ~1.5k chars per call fleet-wide (Jacques, 03.10).
var leanMutationTaskFields = []string{
	"id", "status_id", "assignee_id", "assignee_type", "assignee_name", "updated_at",
}

// leanMutationTask projects a full task object onto the fields above.
func leanMutationTask(task map[string]any) map[string]any {
	lean := map[string]any{}
	for _, f := range leanMutationTaskFields {
		if v, ok := task[f]; ok {
			lean[f] = v
		}
	}
	return lean
}

// leanCommentFields is what an add_comment response keeps by default. The fat
// part of a comment echo is the body — the one thing the caller provably
// already has, having just written it. delivery/hint stay: they are the
// mention-delivery contract (did the @-mention actually reach a path its
// target consumes) and are both small and un-derivable.
var leanCommentFields = []string{
	"id", "task_id", "parent_comment_id",
	"author_id", "author_name", "author_type",
	"is_internal", "created_at", "delivery", "hint",
}

// leanCommentResult projects a created comment onto the fields above.
func leanCommentResult(comment map[string]any) map[string]any {
	lean := map[string]any{}
	for _, f := range leanCommentFields {
		if v, ok := comment[f]; ok {
			lean[f] = v
		}
	}
	return lean
}

// commentsSince keeps only comments created after since. A comment with a
// missing or unparsable created_at fails open (kept, not dropped) — the same
// asymmetry as taskChangedSince: a filter that silently drops content it
// couldn't judge is worse than one that over-returns.
func commentsSince(items []any, since time.Time) []any {
	out := make([]any, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			out = append(out, it)
			continue
		}
		raw, _ := m["created_at"].(string)
		if raw == "" {
			out = append(out, it)
			continue
		}
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			out = append(out, it)
			continue
		}
		if t.After(since) {
			out = append(out, it)
		}
	}
	return out
}

// statusArgPresent reports whether the caller passed a non-empty `status`
// argument. No task tool accepts `status` — create_task/update_task read only
// status_slug — but it's the shorter, more natural name for an LLM caller to
// type. It used to be dropped silently, so create_task(status="backlog")
// reported success while the card landed in the project's default status: the
// call looked executed and wasn't. An empty string counts as absent, matching
// how these handlers treat every other optional string; a non-string value
// (number, object) counts as present — the caller clearly meant something.
func statusArgPresent(request mcpsdk.CallToolRequest) bool {
	args := request.GetArguments()
	if args == nil {
		return false
	}
	v, ok := args["status"]
	if !ok || v == nil {
		return false
	}
	s, isString := v.(string)
	return !isString || s != ""
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

	// `status` is not a parameter of this tool — refuse it loudly instead of
	// dropping it (see statusArgPresent). Not accepted as an alias either: a
	// caller that means status_slug should send status_slug, and an explicit
	// status_slug wins over a stray `status` sent alongside it.
	if statusArgPresent(request) && mcpsdk.ParseString(request, "status_slug", "") == "" {
		return errResult(`unknown parameter "status": use status_slug`)
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
	if startAfterStr := mcpsdk.ParseString(request, "start_after", ""); startAfterStr != "" {
		if _, err := time.Parse(time.RFC3339, startAfterStr); err != nil {
			return errResult("invalid start_after format: %v", err)
		}
		body["start_after"] = startAfterStr
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

	// Same refusal as create_task (see statusArgPresent), but without
	// create_task's status_slug carve-out: update_task has no status_slug
	// parameter at all — status transitions are move_task's job — so there is
	// nothing for a stray `status` to lose to, and a caller hedging with both
	// spellings must be stopped just as hard. Checked before the field loop
	// below so the other fields cannot be patched while the status change is
	// quietly dropped.
	if statusArgPresent(request) {
		return errResult(`unknown parameter "status": use status_slug via move_task (update_task cannot change status)`)
	}

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
	// due_date/start_after use the presence-check (`args[...]; ok`), not the
	// `!= ""` pattern the other date-ish fields below still use: an update
	// that clears a date is a real, agent-reachable action (the whole reason
	// start_after exists), and `!= ""` cannot represent "caller
	// sent an explicit empty value to clear it", only "field omitted". Both
	// share that requirement, so both share the fix — leaving due_date on the
	// old pattern right next to a just-fixed start_after would leave the
	// identical bug sitting in the same function. An empty string reaches the
	// API as `"due_date":""`/`"start_after":""`, which flexTime.UnmarshalJSON
	// treats the same as JSON null: wasSet=true, Time=nil — an explicit clear,
	// not a silent no-op.
	if _, ok := args["due_date"]; ok {
		dueDateStr := mcpsdk.ParseString(request, "due_date", "")
		if dueDateStr != "" {
			if _, err := time.Parse(time.RFC3339, dueDateStr); err != nil {
				return errResult("invalid due_date format: %v", err)
			}
		}
		body["due_date"] = dueDateStr
	}
	if _, ok := args["start_after"]; ok {
		startAfterStr := mcpsdk.ParseString(request, "start_after", "")
		if startAfterStr != "" {
			if _, err := time.Parse(time.RFC3339, startAfterStr); err != nil {
				return errResult("invalid start_after format: %v", err)
			}
		}
		body["start_after"] = startAfterStr
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

	if !mcpsdk.ParseBoolean(request, "full", false) {
		return jsonResult(leanMutationTask(result))
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

	// Return updated task. The reload stays even in the lean view: the move
	// endpoint answers only {"status":"ok"}, so the post-move assignee — which
	// move-to-review's auto-reassign can change — is unknowable without it.
	updatedTask, err := s.getRESTClient(ctx).GetTask(ctx, taskID)
	if err != nil {
		return errResult("task moved but failed to reload: %v", err)
	}

	if !mcpsdk.ParseBoolean(request, "full", false) {
		updatedTask = leanMutationTask(updatedTask)
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
	if startAfterStr := mcpsdk.ParseString(request, "start_after", ""); startAfterStr != "" {
		if _, err := time.Parse(time.RFC3339, startAfterStr); err != nil {
			return errResult("invalid start_after format: %v", err)
		}
		body["start_after"] = startAfterStr
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

	if !mcpsdk.ParseBoolean(request, "full", false) {
		return jsonResult(leanMutationTask(result))
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

	if !mcpsdk.ParseBoolean(request, "full", false) {
		return jsonResult(leanCommentResult(result))
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
	// forever and the done-evidence gate blocks the task on it
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

// defaultListCommentsLimit: the newest 10 comments are what an agent acts on;
// list_comments averaged 6.6k / p95 24.9k per call at the old default of 50.
// Bodies stay whole; has_more/total_pages say how much is hidden.
const defaultListCommentsLimit = 10

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

	limit := mcpsdk.ParseInt(request, "limit", defaultListCommentsLimit)
	if limit > 0 {
		params["page_size"] = strconv.Itoa(limit)
	}

	// The server's own default (sort_dir=asc) made limit=N return the OLDEST N
	// comments — an agent asking for "the last 3" of a thread got the wrong end
	// (live red 03.10: limit=2 on a 17-comment thread returned comments 1-2),
	// so the only safe way to catch up on a thread was reading all of it.
	// Default to the newest end; an explicit order still wins, and a non-empty
	// garbage value is forwarded for the API to refuse (the list_tasks `order`
	// precedent — never silently normalize a direction the caller named).
	// An EXPLICIT order="" is refused HERE rather than forwarded: at the HTTP
	// boundary `sort_dir=` is indistinguishable from an absent sort_dir, and
	// the backend gate (evc-mesh rejectBadSortDir) deliberately reads an empty
	// direction as "caller declined" and defaults to asc — the OLDEST end
	// again, silently. hasArgument is the only place left that can tell
	// explicit-empty from absent, so the refusal lives in this layer (the
	// get_task comments_limit local-refusal precedent).
	if hasArgument(request, "order") {
		order := mcpsdk.ParseString(request, "order", "")
		if order == "" {
			return errResult("order must be \"asc\" or \"desc\"")
		}
		params["sort_dir"] = order
	} else {
		params["sort_dir"] = "desc"
	}

	// `page` was declared nowhere and read nowhere: every call landed on page 1
	// regardless of what the caller passed, silently, while total_pages/has_more
	// in the response looked like a working pager. This is the tool
	// READ-BEFORE-ACT tells every agent to page through for a truncated thread.
	if page := mcpsdk.ParseInt(request, "page", 0); page > 0 {
		params["page"] = strconv.Itoa(page)
	}

	var result map[string]any
	var err error
	if hasArgument(request, "after") {
		after := mcpsdk.ParseString(request, "after", "")
		if after == "" {
			return errResult("after must be a comment UUID or RFC3339 timestamp")
		}
		result, err = s.commentsAfter(ctx, taskID, after, params)
	} else {
		result, err = s.getRESTClient(ctx).ListComments(ctx, taskID, params)
	}
	if err != nil {
		return errResult("failed to list comments: %v", err)
	}

	// Compact view by default (wave 2, #dc719c63): the REST page carries 12
	// fields per comment — a live probe measured
	// 1.6k chars per comment, most of it author_id/task_id/url/metadata the
	// caller already knows or never uses. The compact item keeps what a
	// reader acts on; full=true returns the page untouched.
	if !mcpsdk.ParseBoolean(request, "full", false) {
		result = leanCommentsPage(result, mcpsdk.ParseBoolean(request, "include_auto", false))
	}

	return jsonResult(result)
}

// compactComment retains cursor/reply id and all ordinary text. Only known
// automation prefixes are summarized, with include_auto restoring their bodies.
func compactComment(m map[string]any, includeAuto ...bool) map[string]any {
	out := make(map[string]any, 4)
	for _, f := range []string{"id", "author_name", "created_at", "body"} {
		if v, ok := m[f]; ok {
			out[f] = v
		}
	}
	if stamp, ok := out["created_at"].(string); ok {
		if t, err := time.Parse(time.RFC3339Nano, stamp); err == nil {
			out["created_at"] = t.UTC().Format("2006-01-02T15:04Z")
		}
	}
	if len(includeAuto) == 0 || !includeAuto[0] {
		if body, ok := out["body"].(string); ok && isAutomaticComment(body) {
			out["body"] = strings.Join(strings.Fields(strings.SplitN(body, "\n", 2)[0]), " ")
			out["auto_summary"] = true
		}
	}
	return out
}

func isAutomaticComment(body string) bool {
	for _, prefix := range []string{"[fiddler] ✅ completed", "[balancer]", "🔀 **fleet-balancer сменил исполнителя:", "🤖 auto:", "INTAKE:", "[INTAKE]", "🔀 INTAKE-DECOMPOSE", "🔼 INTAKE-PROMOTE", "🏁 INTAKE-PARENT-CLOSE"} {
		if strings.HasPrefix(body, prefix) {
			return true
		}
	}
	return false
}

// compactCommentItems projects every item; non-map entries pass through.
func compactCommentItems(items []any, includeAuto ...bool) []any {
	out := make([]any, len(items))
	for i, it := range items {
		if m, ok := it.(map[string]any); ok {
			out[i] = compactComment(m, includeAuto...)
		} else {
			out[i] = it
		}
	}
	return out
}

// commentsPageKeepFields is the envelope allow-list for the compact
// list_comments view: the pager (and nothing else) — paging is how a thread
// longer than `limit` is read.
var commentsPageKeepFields = []string{
	"page", "page_size", "total_count", "total_pages", "has_more", "list_revision",
}

// leanCommentsPage applies the compact view to a REST comments page.
func leanCommentsPage(result map[string]any, includeAuto ...bool) map[string]any {
	out := make(map[string]any, len(commentsPageKeepFields)+1)
	for _, k := range commentsPageKeepFields {
		if v, ok := result[k]; ok {
			out[k] = v
		}
	}
	if items, ok := result["items"].([]any); ok {
		out["items"] = compactCommentItems(items, includeAuto...)
	} else if items, ok := result["items"]; ok {
		out["items"] = items
	} else {
		out["items"] = []any{}
	}
	return out
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

	return jsonResult(shapeArtifact(result))
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

	shapeArtifactList(result)
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
		"artifact": shapeArtifact(artifact),
	}

	if mcpsdk.ParseBoolean(request, "include_content", false) {
		downloadURL, err := s.getRESTClient(ctx).GetArtifactDownloadURL(ctx, artifactID)
		if err != nil {
			resp["content_error"] = fmt.Sprintf("failed to get download URL: %v", err)
		} else {
			resp["download_api_url"] = downloadURL
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

	// Resolve a 6–12 hex short-ID prefix onto the task first, exactly like
	// get_task: the /tasks/:id/context route parses the path id as a strict
	// UUID and answers 400 "invalid task_id" for the short form (live red
	// 02.10, #e143d391: get_task_context('06d0c6d8')). Full UUIDs skip the
	// extra hop; unknown and ambiguous prefixes surface the resolver's own
	// verdict as a normal tool error without any context read.
	resolvedID := taskID
	if len(taskID) != 36 {
		resolved, err := s.getRESTClient(ctx).GetTask(ctx, taskID)
		if err != nil {
			return errResult("failed to get task context: %v", err)
		}
		if id, _ := resolved["id"].(string); id != "" {
			resolvedID = id
		}
	}

	result, err := s.getRESTClient(ctx).GetTaskContext(ctx, resolvedID)
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

	if arts, ok := result["artifacts"]; ok {
		shapeArtifactList(arts)
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

// validAgentTypes mirrors the agent_type enum on the Mesh API. The API is the
// authority; this list only lets a bad value fail before any request is sent
// (update_agent_profile makes up to two writes, a late failure would leave the
// first one applied).
var validAgentTypes = map[string]bool{
	"claude_code": true, "openclaw": true, "cline": true, "aider": true, "custom": true,
	"hermes": true, "codex": true, "cursor": true, "copilot": true, "gemini_cli": true,
}

// agentModelMaxLen mirrors the API limit on the self-reported model string.
// Like the API it is a byte limit (model ids are ASCII); keep the two in step.
const agentModelMaxLen = 128

// parseHarnessModel reads the optional agent_type / model self-report params.
// A supplied agent_type must be a valid enum value (blank included: the
// harness cannot be reset). A blank model is passed through
// as "" (the API clears it back to "not reported").
//
// The model limit is checked AFTER TrimSpace, mirroring the API's
// normalizeAgentModel (evc-mesh agent_handler.go): the value sent is the
// trimmed one, so nothing over the limit ever leaves this client, and raw
// input that trims to size is accepted here exactly as the API accepts it
// over REST. Checking the raw length first would reject values the API
// itself stores — keep this order in sync with the API instead.
func parseHarnessModel(request mcpsdk.CallToolRequest) (fields map[string]any, err error) {
	args := request.GetArguments()
	fields = map[string]any{}
	if _, ok := args["agent_type"]; ok {
		at := strings.TrimSpace(mcpsdk.ParseString(request, "agent_type", ""))
		if !validAgentTypes[at] {
			return nil, fmt.Errorf("invalid agent_type %q", at)
		}
		fields["agent_type"] = at
	}
	if _, ok := args["model"]; ok {
		m := strings.TrimSpace(mcpsdk.ParseString(request, "model", ""))
		if len(m) > agentModelMaxLen {
			return nil, fmt.Errorf("model must be <=%d chars", agentModelMaxLen)
		}
		fields["model"] = m
	}
	return fields, nil
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
	selfReport, perr := parseHarnessModel(request)
	if perr != nil {
		return errResult("%v", perr)
	}
	for k, v := range selfReport {
		body[k] = v
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

	return s.renderMyTasks(ctx, request, params, result)
}

// myTasksDescChars caps a task's description in the get_my_tasks list view.
// 200 chars covers the summary-first-line convention card authors write;
// anything longer is body prose a list caller didn't ask to re-read.
const myTasksDescChars = 200

// trimTaskSummaries trims each task's description to its first line, at most
// myTasksDescChars runes (rune-safe via firstLineTruncated), and stamps
// description_truncated=true when text was cut. Items whose description the server already
// blanked (empty string, has_description reflecting real content — the API's
// own >200KB page trim) pass through untouched. Routing fields
// (id/title/status/priority/labels/assignee) are never modified.
func trimTaskSummaries(tasks []any) []any {
	out := make([]any, 0, len(tasks))
	for _, it := range tasks {
		m, ok := it.(map[string]any)
		if !ok {
			out = append(out, it)
			continue
		}
		desc, _ := m["description"].(string)
		if desc == "" {
			out = append(out, m)
			continue
		}
		if _, has := m["has_description"]; !has {
			m["has_description"] = true
		}
		if trimmed := firstLineTruncated(desc, myTasksDescChars); trimmed != desc {
			m["description"] = trimmed
			m["description_truncated"] = true
		}
		out = append(out, m)
	}
	return out
}

// myTasksLeanDropKeys are per-item fields the default get_my_tasks view omits.
// After the description trim (!136) a 50-task page still measured ~60k chars
// live (2026-10-03, task 82b388a0): ~1.1k chars of envelope per card, mostly
// UUID cross-references, timestamps and a url derivable from id. None of them
// is needed to pick the next card; get_task(id) or full=true returns them.
var myTasksLeanDropKeys = []string{
	"url", "parent_task_id", "assignee_id", "assignee_type", "assigned_by",
	"created_by", "created_by_name", "created_by_type", "created_at", "completed_at",
	"delegation_level", "position", "human_gate_class", "is_shipped",
}

// myTasksLeanDropWhenEmpty are dropped only when null/empty/zero/false, so a
// real value (estimate, start_after, custom fields, subtasks) is still shown.
// due_date and human_gate joined after the first live page (still 27k chars,
// 2026-10-03) carried human_gate:false on all 50 items and due_date:null on
// 49 — a set deadline or an armed gate must survive, the empty forms don't.
var myTasksLeanDropWhenEmpty = []string{
	"custom_fields", "dod_checks", "estimated_hours", "start_after", "subtask_count",
	"artifact_count", "vcs_link_count", "completion_signal", "due_date", "human_gate",
}

// leanTaskSummaries strips the envelope fields above from each item. Routing
// fields (id/title/status_id/priority/labels/assignee_name/updated_at/
// project_id) and gate/checkout context are never touched; due_date and
// human_gate are value-conditional (see myTasksLeanDropWhenEmpty).
func leanTaskSummaries(tasks []any) []any {
	for _, it := range tasks {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		for _, k := range myTasksLeanDropKeys {
			delete(m, k)
		}
		for _, k := range myTasksLeanDropWhenEmpty {
			if isEmptyJSONValue(m[k]) {
				delete(m, k)
			}
		}
	}
	return tasks
}

func isEmptyJSONValue(v any) bool {
	switch x := v.(type) {
	case nil:
		return true
	case string:
		return x == ""
	case bool:
		return !x
	case float64:
		return x == 0
	case map[string]any:
		return len(x) == 0
	case []any:
		return len(x) == 0
	}
	return false
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

	if mcpsdk.ParseBoolean(request, "full", false) {
		return jsonResult(result)
	}
	return jsonResult(compactTeamDirectory(result))
}

// teamDirectoryZoneChars caps the project column (responsibility_zone) in
// the compact directory to one line. Wave 2 (#dc719c63): a live probe put
// the zone column at 54% of the whole compact payload — 200 CHARS is up to
// 400 BYTES of UTF-8 Russian, and the fleet's own growth (41 agents and
// counting) multiplied it. 60 chars still name the zone; the full text is a
// full=true field.
const teamDirectoryZoneChars = 60

// teamDirectoryColumns are the row-array columns compactTeamDirectory emits
// for both "agents" and "humans", in this order.
var teamDirectoryColumns = []string{"id", "name", "role", "project", "status"}

// compactTeamDirectory trims the full team-directory dump (~35 fields per
// agent: heartbeat text, capabilities map, accepts_from, three timestamps,
// is_home/is_stale, parent_agent_id, working_hours, ...) to a row-array
// table of the fields routing decisions actually use: identity (id/name),
// role, project/zone, and status. Full profiles remain one full=true call
// away.
//
// "status" is computed_status, not the raw status field — get_team_directory's
// own status lies (liveness is computed_status + heartbeat age, not
// status), so surfacing the trustworthy field under a
// plain "status" column here is a deliberate correction, not an oversight.
func compactTeamDirectory(full map[string]any) map[string]any {
	out := map[string]any{
		"workspace": full["workspace"],
		"columns":   teamDirectoryColumns,
		"agents":    compactTeamDirectoryRows(full["agents"]),
		"humans":    compactTeamDirectoryRows(full["humans"]),
		"note":      "Compact rows: id/name/role/project/status (project = responsibility_zone, one line ≤60 chars; status = computed_status). full=true returns full profiles.",
	}
	return out
}

func compactTeamDirectoryRows(members any) [][]any {
	items, _ := members.([]any)
	rows := make([][]any, 0, len(items))
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		status, _ := m["computed_status"].(string)
		if status == "" {
			status, _ = m["status"].(string)
		}
		var zone any = m["responsibility_zone"]
		if z, ok := zone.(string); ok {
			zone = firstLineTruncated(z, teamDirectoryZoneChars)
		}
		rows = append(rows, []any{
			m["id"],
			m["name"],
			m["role"],
			zone,
			status,
		})
	}
	return rows
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

	// agent_type / model also go to PATCH /agents/me. Validated here, before
	// the first write, so a bad value cannot leave the profile half-applied.
	selfReport, perr := parseHarnessModel(request)
	if perr != nil {
		return errResult("%v", perr)
	}

	if len(body) == 0 && !callbackURLUpdate && len(selfReport) == 0 {
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

	// Persist callback_url / agent_type / model via PATCH /agents/me.
	meBody := map[string]any{}
	for k, v := range selfReport {
		meBody[k] = v
	}
	var cbURL string
	if callbackURLUpdate {
		cbURL = mcpsdk.ParseString(request, "callback_url", "")
		meBody["callback_url"] = cbURL
	}
	if len(meBody) > 0 {
		if _, err := s.getRESTClient(ctx).UpdateMe(ctx, meBody); err != nil {
			return errResult("failed to update agent profile (agent_type/model/callback_url): %v", err)
		}
		if profileResult == nil {
			profileResult = map[string]any{}
		}
		if callbackURLUpdate {
			profileResult["callback_url"] = cbURL
		}
		for k, v := range selfReport {
			profileResult[k] = v
		}
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
	// Same lean per-card view as get_my_tasks; full=true is the old shape.
	if !mcpsdk.ParseBoolean(request, "full", false) {
		if tasks, ok := result["tasks"].([]any); ok {
			result["tasks"] = leanTaskSummaries(trimTaskSummaries(tasks))
		}
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
	// 0.3, not 0.4: this fallback must match the server's defaultMinImportance,
	// which is itself pinned to the LOWEST score the server mints
	// (kind:session-checkpoint = 0.30). At 0.4 this line silently defeated that
	// server default for every agent — ParseFloat64 FILLS THE FIELD IN, so the
	// request always carried min_importance and the server's "caller omitted it"
	// branch could never run. Measured on prod 2026-09-06: 89 of 2527 active
	// memories sat below 0.4 and all 89 were session-checkpoints, i.e. the one
	// class written for the next session to read was invisible to a plain
	// recall().
	//
	// ⚠️ This is a SECOND copy of a default that the server also owns, which is
	// a known drift class. The structural fix is for
	// this client to send nothing when the caller said nothing and let the server
	// decide; that needs ImportanceMin to become a pointer through the shared
	// request builder, so it is filed separately rather than smuggled in here.
	minImportance := mcpsdk.ParseFloat64(request, "min_importance", recallDefaultMinImportance)
	applyDecay := mcpsdk.ParseBoolean(request, "apply_recency_decay", false)
	orderBy := mcpsdk.ParseString(request, "order_by", "")
	includeExpired := mcpsdk.ParseBoolean(request, "include_expired", false)
	includeArchived := mcpsdk.ParseBoolean(request, "include_archived", false)
	full := mcpsdk.ParseBoolean(request, "full", false)

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
	// Same rule as order_by and limit below, and it was the one param that still
	// broke it: a preset fills in what the caller left unsaid, it does not overrule
	// what the caller said. Without the hasArgument guard an explicit
	// min_importance was silently replaced whenever the classifier picked a profile
	// that carries one — `factual` sets 0.5, so a caller asking for 0.3 got 0.5 and
	// no session-checkpoints, with nothing in the response to say why.
	//
	// Measured through the real stdio path 2026-09-06 on one query: explicit
	// min_importance=0.3 returned 0 checkpoints (preset won), the same query under
	// the default profile returned 3.
	minImportance = resolveProfileMinImportance(pp.MinImportance, minImportance,
		hasArgument(request, "min_importance"))
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

	// Rerank before anything else touches result["items"]: the graph-merge step
	// below cuts the WEAKEST base items to make room for neighbours, and the
	// trim step keeps only the first recallFullCount full — both need the
	// boosted order, not the server's raw score order.
	items, _ := result["items"].([]any)
	if len(items) > 0 {
		items = rerankRecallItems(items, query, projectID, scope)
		result["items"] = items
	}

	// Gate on the server's own score, not the boost above: the boost is a
	// structural tie-breaker (this looks like what you asked for), not a
	// relevance signal on its own, and gating on the boosted order would let an
	// exact key match through even when nothing in the result is actually
	// relevant. If nothing clears the bar, say so instead of handing back the
	// weakest candidates padded to a full page.
	best, scored := recallTopScore(items)
	noisy := recallNoisyResult(result, items, query, best, scored)
	// The threshold is not an unconditional kill anymore (2026-10-03 fixture): on a
	// server that reports arm counts, only the noise gate decides — a query
	// whose word occurs in a returned entry is answerable however low its
	// fused rank. The bare threshold still gates servers without arm counts,
	// where the noise gate cannot run.
	if !scored || noisy || (best < recallRelevanceThreshold && !recallArmCounts(result)) {
		reason := fmt.Sprintf("no candidate cleared the relevance threshold (best score %.5f, threshold %.5f)",
			best, recallRelevanceThreshold)
		if !scored {
			reason = "no returned entry carried a score the gate could judge"
		} else if noisy {
			reason = fmt.Sprintf("best score %.5f is below the noise ceiling %.5f (sparse_rows=%d, dense_rows=%d): nothing ranked near the top of either arm and no query word occurs in any returned entry",
				best, recallNoiseCeiling, int(result["sparse_rows"].(float64)), int(result["dense_rows"].(float64)))
		}
		result["items"] = []any{}
		result["total"] = 0
		result["explanation"] = reason + " — returning empty instead of the weakest matches; " +
			"try a more specific query, or widen scope/project_id"
		s.recordMemoryRead(ctx, "recall")
		return jsonResult(result)
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
			// two to silently disagree.
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

	// Compact view by default: every item is cut to the fields a reader acts on
	// and ~recallContentChars of content (flagged when cut). full=true returns
	// the server's items untouched; get_memory(key) fetches one entry whole.
	if !full {
		if items, ok := result["items"].([]any); ok {
			result["items"] = compactRecallItems(items)
		}
	}

	s.recordMemoryRead(ctx, "recall")
	return jsonResult(result)
}

// get_memory scans at most getMemoryPages pages of getMemoryPageSize results.
const (
	getMemoryPageSize = 50
	getMemoryPages    = 4
)

// handleGetMemory returns the full text of ONE memory by exact key. The backend
// has no get-by-key endpoint, so this recalls on the key and picks the entry whose
// key matches exactly; the relevance gate of handleRecall is deliberately not
// applied (a known key is not a search), nor is the importance floor.
func (s *Server) handleGetMemory(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	session := s.getSession(ctx)
	if session == nil {
		return errResult("not authenticated: no agent session")
	}
	key := mcpsdk.ParseString(request, "key", "")
	if key == "" {
		return errResult("key is required")
	}
	projectID := mcpsdk.ParseString(request, "project_id", "")
	scope := mcpsdk.ParseString(request, "scope", "")

	// Page through the search until the exact key shows up: the key is a query
	// like any other, so its entry is not guaranteed to sit in the first page.
	// Bounded (getMemoryPages x getMemoryPageSize) so a typo cannot scan forever.
	includeArchived := mcpsdk.ParseBoolean(request, "include_archived", false)
	// A key is an address, not a search: a superseded entry is still the entry at
	// that key, so the server default (exclude superseded) must not hide it.
	includeSuperseded := false
	for page := 0; page < getMemoryPages; page++ {
		result, err := s.getRESTClient(ctx).RecallMemories(ctx, RecallMemoriesParams{
			Query:             key,
			WorkspaceID:       session.WorkspaceID.String(),
			ProjectID:         projectID,
			Scope:             scope,
			ImportanceMin:     0,
			IncludeArchived:   includeArchived,
			ExcludeSuperseded: &includeSuperseded,
			Limit:             getMemoryPageSize,
			Offset:            page * getMemoryPageSize,
		})
		if err != nil {
			return errResult("get_memory failed: %v", err)
		}
		items, _ := result["items"].([]any)
		for _, it := range items {
			if m, ok := it.(map[string]any); ok && m["key"] == key {
				s.recordMemoryRead(ctx, "get_memory")
				return jsonResult(m)
			}
		}
		if len(items) < getMemoryPageSize {
			break // last page
		}
	}
	return errResult("no memory with key %q found (try recall to search by words)", key)
}

// recallRelevanceThreshold gates whether recall returns anything at all: below
// it, the best candidate is noise for this query, and the honest answer is an
// empty list with an explanation rather than the 8 weakest matches padded onto
// the page.
//
// First calibration (2026-09-30, superseded) picked the p60 mark of the
// per-call top-score distribution over 2288 historical recall calls:
//
//	p10 0.01148  p30 0.01482  p50 0.01570  p60 0.01595  p70 0.01613  p90 0.01639
//
// That cut (0.0159) landed the ~50-70% empty-rate target on
// historical traffic, but historical traffic is not the right reference
// class — it mixes genuinely-unanswerable exploratory queries with queries
// that had a real answer in memory, and a single global percentile can't
// tell them apart. Review raised exactly this concern: RRF scores
// are small and tightly clustered, so an absolute cut calibrated on the
// whole population risks eating real matches, and argued for a
// relative/normalized cutoff instead (score/top1_score).
//
// Live-validated 2026-09-30: ran the p60 threshold against 10 fixture-style
// queries built from representative topics, each with a genuinely on-topic top
// hit already in memory. 0.0159 would have zeroed out at least 3 of them —
// e.g. three of them top-scored 0.01148, 0.01393 and 0.01537 on their own
// on-topic episodes — all real matches, all below the p60 bar. The
// concern was confirmed empirically, not just in theory.
//
// A pure ratio (score/top1_score) can't fix this on its own: top1/top1 is
// always 1, so a within-call relative cutoff can never empty a response,
// and the gate must be able to return an empty result when nothing is relevant.
// Instead this drops the absolute floor well below the weakest genuine hit
// observed above (0.01148, with margin), and leaves quality/ordering to
// rerankRecallItems (key/project/scope boost) rather than to the gate. The
// floor's job is narrowed to catching genuine noise (a result that only
// showed up via a weak, low-rank coincidence in one retrieval arm), not to
// hitting a target empty-rate on its own — that rate is now whatever real
// traffic produces under this lower, safer floor. Formal acceptance against
// a fixed fixture of real queries is the next checkpoint to retune this if
// needed.
//
// Measured on the re-recorded 2026-10-03 fixture snapshot (222 calls +
// 3 live garbage probes): applied unconditionally, this threshold killed real
// queries BEFORE the noise gate's overlap check could rescue them. The three
// lost cases sat at fused 0.0078/0.0088/0.0098 — below the bar — with their
// query tokens present in the returned keys ("gotcha-verify-driver-…",
// "fiddlersessiondead-…", "solution-checkout-…"), while garbage sits at
// 0.011475 (dense-arm rank 1 alone), ABOVE the bar: the unconditional cut
// only ever hit the real ones. It now applies only when the server reports no
// arm counts, where recallNoisyResult cannot run.
const recallRelevanceThreshold = 0.0100

// recallNoiseCeiling is the second gate signal (#858f3a13). The dense arm
// always returns its nearest neighbours, so on a nonsense query the fused score
// still lands at ~0.0115 — above recallRelevanceThreshold — and the gate never
// fired. A fused score below this ceiling means no row ranked near the top of
// either arm (one arm's rank-1 alone is 1/61 = 0.0164), which is what noise
// looks like. The sparse_rows COUNT is deliberately not used: live acceptance
// showed it drifting on the same garbage string (0, then 1, then 3), so any
// "sparse_rows <= N" cut only catches part of the noise. The ceiling alone
// also ate a real identifier-only query (same 0.01148, sparse_rows=0), so it
// only fires when no query word occurs in any returned entry. A missing sparse_rows
// field (older server) disables the signal; dense_rows must be > 0.
//
// 2026-10-03 measurement note: the ceiling cannot move down to separate the
// classes — on the 03.10 re-record the lost real queries (fused 0.0078–0.0098)
// sit BELOW garbage (0.011475), so any ceiling that keeps garbage out also
// keeps them out. Lexical overlap is the only discriminator below the ceiling.
const recallNoiseCeiling = 0.0120

// recallNoisyResult reports whether the response looks like dense-arm noise:
// best fused score below recallNoiseCeiling on a server that reports arm
// counts and returned dense rows, AND no word of the query appears anywhere in
// the returned entries. The fused score is rank-only, so a garbage query, a
// real identifier-only query (acceptance #858f3a13, fixture 9) and a real
// paraphrase all land in the same band; the lexical overlap is what tells the
// real ones apart — including below recallRelevanceThreshold, which is why
// that threshold no longer empties a response unconditionally (2026-10-03 fixture: it
// cut "review-verify-driver" — used key at server rank 1, fused 0.0078, query
// token in the returned key — before this check could rescue it).
//
// A third signal — the raw dense cosine (dense_score, exposed by evc-mesh
// !1065) — was measured for this gate and rejected (re-recorded 03.10
// snapshot, 222 cases + 3 live garbage probes): garbage tops out at cosine
// 0.839–0.864 while the lost real queries top at ~0.86, overlapping bands, so
// no floor value can separate them. dense_score remains a diagnostic field.
func recallNoisyResult(result map[string]any, items []any, query string, best float64, scored bool) bool {
	if !scored || best >= recallNoiseCeiling {
		return false
	}
	if !recallArmCounts(result) {
		return false
	}
	return !recallQueryOverlapsItems(query, items)
}

// recallArmCounts reports whether the server populated the arm-count envelope
// the noise gate needs (sparse_rows present, dense_rows > 0). Servers without
// it are gated by recallRelevanceThreshold alone — the pre-#858f3a13 contract.
func recallArmCounts(result map[string]any) bool {
	if _, ok := result["sparse_rows"].(float64); !ok {
		return false
	}
	dense, _ := result["dense_rows"].(float64)
	return dense > 0
}

// recallMinOverlapTokenLen: shorter words ("the", "mcp", "py") are too common
// to count as evidence that an entry answers the query.
const recallMinOverlapTokenLen = 4

// recallQueryOverlapsItems reports whether any query word of at least
// recallMinOverlapTokenLen characters appears as a whole word in the key,
// content or tags of any item. A query with no such word cannot be judged
// lexically, so it counts as overlapping (the gate stays off).
func recallQueryOverlapsItems(query string, items []any) bool {
	split := func(s string) []string {
		return strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
			return !unicode.IsLetter(r) && !unicode.IsDigit(r)
		})
	}
	var qTokens []string
	for _, t := range split(query) {
		if len([]rune(t)) >= recallMinOverlapTokenLen {
			qTokens = append(qTokens, t)
		}
	}
	if len(qTokens) == 0 {
		return true
	}
	have := map[string]struct{}{}
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		for _, f := range []string{"key", "content", "snippet"} {
			if str, ok := m[f].(string); ok {
				for _, t := range split(str) {
					have[t] = struct{}{}
				}
			}
		}
		if tags, ok := m["tags"].([]any); ok {
			for _, tg := range tags {
				if str, ok := tg.(string); ok {
					for _, t := range split(str) {
						have[t] = struct{}{}
					}
				}
			}
		}
	}
	for _, t := range qTokens {
		if _, ok := have[t]; ok {
			return true
		}
	}
	return false
}

// recallTopScore returns the highest "score" field among items, and whether any
// item carried a numeric score at all (items missing "score" are not evidence
// of irrelevance — they just cannot be judged, which is treated the same as
// "gate failed" by the caller).
func recallTopScore(items []any) (float64, bool) {
	best := 0.0
	found := false
	for _, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		score, ok := m["score"].(float64)
		if !ok {
			continue
		}
		if !found || score > best {
			best = score
			found = true
		}
	}
	return best, found
}

// rerankRecallItems moves candidates that match the query's own key, or the
// caller's project/scope, ahead of items the server scored higher on text/
// vector similarity alone. It is a stable sort on a small integer boost, so two
// items with the same boost (including boost 0, the common case) keep the
// server's original relevance order — this only ever reorders across boost
// tiers, never within one.
//
// The boost is intentionally coarse (an int, not a score blend): the "score"
// field is an RRF fusion value on its own scale, and folding a structural match
// into it invites exactly the kind of silent cross-scale arithmetic that has
// broken ranking here before (see graphBoostReserve's comment on base vs.
// composite_score). Keeping the two separate means the threshold gate above can
// keep judging actual relevance while this only judges "is this what the
// caller is clearly pointing at".
//
// Since 2026-10-03 the boost-0 tail additionally gets importance tie-bands:
// see bandUnboostedByImportance.
func rerankRecallItems(items []any, query, projectID, scope string) []any {
	if len(items) == 0 {
		return items
	}
	type ranked struct {
		item  any
		boost int
	}
	qLower := strings.ToLower(query)
	out := make([]ranked, len(items))
	for i, it := range items {
		boost := 0
		if m, ok := it.(map[string]any); ok {
			if key, _ := m["key"].(string); key != "" {
				boost += keyMatchBoost(strings.ToLower(key), qLower)
			}
			if projectID != "" {
				if pid, _ := m["project_id"].(string); pid == projectID {
					boost++
				}
			}
			if scope != "" {
				if sc, _ := m["scope"].(string); sc == scope {
					boost++
				}
			}
		}
		out[i] = ranked{item: it, boost: boost}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].boost > out[j].boost })
	result := make([]any, len(out))
	// The boost sort is stable, so every boost tier keeps server order inside
	// it; the boost-0 items therefore form one contiguous tail starting at the
	// first unboosted position. Only that tail is eligible for the importance
	// tie-bands below — a structural match (key/project/scope) is a stronger
	// statement than any score-band argument and must not be re-judged.
	tailStart := len(out)
	for i, r := range out {
		if r.boost == 0 {
			tailStart = i
			break
		}
	}
	for i, r := range out {
		result[i] = r.item
	}
	if tailStart < len(result) {
		bandUnboostedByImportance(result[tailStart:])
	}
	return result
}

// recallScoreBandEps is how close two fused server scores must be for the pair
// to count as a server tie. RRF fusion values on a page of results are small
// and tightly clustered (p10 adjacent gap 0.39%, median 2.3% of ~0.011), so an
// absolute epsilon is a usable "same band" test. 0.001 ≈ 9% of a typical fused
// score: wide enough to cover the cluster noise the tie-band is aimed at,
// narrow enough that it can never vault an item across a real relevance gap.
// The value sits in the middle of a measured plateau (0.0008 and 0.0012 give
// the same replay top-3; 0.002 and above collapse the gain entirely), so it is
// not a knife-edge tuning point.
const recallScoreBandEps = 0.001

// bandUnboostedByImportance reorders only the score-TIES among unboosted items.
//
// Why: the 2026-10-03 replay fixture (222 real logged recall calls, 188
// measurable used keys) showed per-item top-3 of 62% with the plain boost sort
// — 71 misses, and in most of them the used key sat at server rank 4-9 behind
// items the fused score put within a fraction of a percent of it. The used
// keys are overwhelmingly curated knowledge (solution-*, decision-*,
// learning-*, doc-*), while the near-tied items crowding them out are
// frequently episodic (episode-*, session-checkpoint-*), and importance_score
// is exactly the server-curated field that encodes that distinction (pinned
// 1.0 > incident 0.85 > decision 0.80 > learning 0.70 > fact 0.60 > none 0.50
// > checkpoint 0.30). So within a score band — where the server itself
// expresses no meaningful preference — the more important item goes first.
//
// Blast radius is bounded by construction: it only ever swaps items the server
// scored within recallScoreBandEps of each other, only within the boost-0
// tail, and it is a stable sort, so equal importance keeps server order. It
// cannot touch the gate (recallTopScore and the noise/overlap checks read all
// items regardless of order) and cannot change response size beyond which
// three items get full content. On the replay fixture: tune split 58/96 ->
// 63/96 (+6 gains, 1 loss), gate-loss 0, garbage queries unaffected.
func bandUnboostedByImportance(tail []any) {
	if len(tail) < 2 {
		return
	}
	scoreOf := func(it any) (float64, bool) {
		m, ok := it.(map[string]any)
		if !ok {
			return 0, false
		}
		s, ok := m["score"].(float64)
		return s, ok
	}
	// Band construction needs the tail in fused-score order so a band head is
	// the band's highest score. Server pages arrive score-descending in
	// practice, but ties and legacy rows without a score make that an
	// assumption, not a contract — one stable pre-sort removes it (equal or
	// missing scores keep server order; unscored rows sort last).
	sort.SliceStable(tail, func(i, j int) bool {
		si, okI := scoreOf(tail[i])
		sj, okJ := scoreOf(tail[j])
		switch {
		case !okI && !okJ:
			return false
		case !okI:
			return false // unscored sorts after scored
		case !okJ:
			return true
		default:
			return si > sj
		}
	})
	// A band is anchored at its HIGHEST score: every following item whose score
	// is within recallScoreBandEps of the band head joins it, and the first item
	// further away starts the next band. Head-anchoring, not adjacent-gap
	// chaining: a chain of within-eps ADJACENT gaps (0.0120, 0.0115, 0.0110,
	// 0.0105, ...) would let one band grow unboundedly and vault items across a
	// cumulative gap the server did express a preference over (measured on the
	// replay fixture: chaining drops the tune top-3 from 63 to 57).
	// Items without a score start their own band and never merge.
	start := 0
	headScore, hasHead := scoreOf(tail[0])
	flush := func(end int) {
		sort.SliceStable(tail[start:end], func(i, j int) bool {
			impI, _ := tail[start+i].(map[string]any)
			impJ, _ := tail[start+j].(map[string]any)
			// missing importance_score sorts as 0: an item the server never
			// graded loses a band tie-break, it does not win one.
			vi, vj := 0.0, 0.0
			if impI != nil {
				vi, _ = impI["importance_score"].(float64)
			}
			if impJ != nil {
				vj, _ = impJ["importance_score"].(float64)
			}
			return vi > vj
		})
	}
	for i := 1; i < len(tail); i++ {
		s, ok := scoreOf(tail[i])
		if !ok || !hasHead || headScore-s > recallScoreBandEps {
			flush(i)
			start = i
			headScore, hasHead = s, ok
		}
	}
	flush(len(tail))
}

// recallKeyTokenMinShared is how many distinct 4+ char key tokens must appear
// in the query before a key counts as "pointed at". One shared token is not a
// pointer: replayed over 222 real logged recall calls the old
// any-one-token rule demoted used memories out of the top 3 (per-item top-3
// 60% in server order -> 53% reranked) because common tokens such as "spark",
// "mesh" or "decision" lift whatever key happens to contain them.
const recallKeyTokenMinShared = 3

// keyMatchBoost scores how strongly a memory's key matches the query text:
// exact match highest, one containing the other next, then several shared
// hyphen/underscore-delimited tokens (4+ chars each, recallKeyTokenMinShared of
// them, to skip noise like "the" or "p2a"). All inputs are expected already
// lower-cased by the caller.
func keyMatchBoost(key, query string) int {
	switch {
	case key == query:
		return 3
	case strings.Contains(query, key) || strings.Contains(key, query):
		return 2
	}
	shared := 0
	seen := map[string]bool{}
	for _, tok := range strings.FieldsFunc(key, func(r rune) bool { return r == '-' || r == '_' }) {
		if len(tok) >= 4 && !seen[tok] && strings.Contains(query, tok) {
			seen[tok] = true
			shared++
		}
	}
	if shared >= recallKeyTokenMinShared {
		return 1
	}
	return 0
}

// recallContentChars caps the content of every item in the default (compact)
// recall view. Measured on the fleet: 349 recall calls/day averaging 7.9k chars
// per response, almost all of it content nobody reads past the first lines. A cut
// item says so and carries its full length; full=true or get_memory(key)
// returns it whole.
const recallContentChars = 300

// recallCompactFields is the allow-list of fields kept per item in the compact
// view. Everything else (scope, importance_score, created_at and the other
// write-path bookkeeping, dedup hash, decay scores, version...) is dropped:
// it restates the order or is only useful to the server, and a fleet probe
// (2026-10-05, #dc719c63) put the bookkeeping at ~140 bytes of a ~600-byte
// item. graph_boost/provenance stay because they change how a reader must
// interpret the hit. Timestamps and scores-of-the-write-path are full=true
// fields now.
var recallCompactFields = []string{
	"key", "tags", "score", "graph_boost", "provenance",
}

// compactRecallItems projects every item onto the compact view.
func compactRecallItems(items []any) []any {
	out := make([]any, len(items))
	for i, it := range items {
		m, ok := it.(map[string]any)
		if !ok {
			out[i] = it
			continue
		}
		out[i] = compactRecallItem(m)
	}
	return out
}

// compactRecallItem returns a copy of m limited to recallCompactFields plus
// content cut to recallContentChars (rune-safe). A cut is marked by a trailing
// ellipsis in the content itself — the content_truncated/content_chars fields
// left the compact view in wave 2 (#dc719c63); get_memory(key) or full=true
// fetches the whole entry. project_id is kept only for project-scoped entries
// (it is what get_memory needs to disambiguate the key); archived/status
// survive only while they carry a non-default value, so include_archived
// callers still see the ones that matter.
func compactRecallItem(m map[string]any) map[string]any {
	out := make(map[string]any, len(recallCompactFields)+4)
	for _, f := range recallCompactFields {
		if v, ok := m[f]; ok {
			out[f] = v
		}
	}
	if sc, _ := m["scope"].(string); sc == "project" {
		if v, ok := m["project_id"]; ok {
			out["project_id"] = v
		}
	}
	if a, ok := m["archived"].(bool); ok && a {
		out["archived"] = a
	}
	if st, _ := m["status"].(string); st != "" && st != "active" {
		out["status"] = st
	}
	content, ok := m["content"].(string)
	if !ok {
		// Entries the server already reduced to key+snippet keep the snippet.
		if sn, ok := m["snippet"]; ok {
			out["content"] = sn
		}
		return out
	}
	if r := []rune(content); len(r) > recallContentChars {
		out["content"] = string(r[:recallContentChars]) + "…"
	} else {
		out["content"] = content
	}
	return out
}

// firstLineTruncated returns the first line of s, truncated to at most maxChars
// runes.
func firstLineTruncated(s string, maxChars int) string {
	if nl := strings.IndexByte(s, '\n'); nl >= 0 {
		s = s[:nl]
	}
	r := []rune(s)
	if len(r) > maxChars {
		return string(r[:maxChars])
	}
	return s
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
// full page. Appending (as this used to) meant recall(limit=10) handed
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
	// Gated to scope=="project" only (F3): identity now follows
	// declared scope (workspace -> (ws,key), project -> (ws,project,key)) since
	// evc-mesh#444/memory_service.go:488 narrowed the server-side twin of this
	// same auto-stamp the same way. Without this gate, a workspace-scope
	// remember() from inside a checked-out task silently gets a project_id it
	// never asked for, which is exactly the drift a one-off collapse had to
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
// set_human_gate / clear_human_gate
// ============================================================================

// handleSetHumanGate is the explicit arming path. It validates the two
// fields locally BEFORE the round trip, not because the server would miss them — it
// returns 422 naming the field — but because the local message can say what to do next
// while the HTTP one can only say what was wrong.
// setHumanGateArgs is the parsed, validated shape of a set_human_gate call.
type setHumanGateArgs struct {
	TaskID             string
	Reason             string
	RecommendedDefault string
	Class              string
	Deadline           *time.Time
	// Predicate is the four-question check. Sent to the server, which
	// decides — the client does NOT pre-judge it. Deliberate: two implementations of one
	// predicate drift, and the server's answer is the one that governs the write.
	Predicate map[string]any
}

// parseSetHumanGateArgs validates locally, BEFORE the round trip. Not because the server
// would miss anything — it returns 422 naming the field — but because the local message
// can say what to WRITE next, while the HTTP one can only say what was wrong. An agent
// that learns only "recommended_default: required" retries the same call verbatim or
// concludes it may not raise a gate at all; that misreading is the failure this card is
// about. Split out of the handler so both the refusals and the accept path are testable
// without a network.
func parseSetHumanGateArgs(request mcpsdk.CallToolRequest) (*setHumanGateArgs, string) {
	taskID := strings.TrimSpace(mcpsdk.ParseString(request, "task_id", ""))
	if taskID == "" {
		return nil, "task_id is required"
	}
	reason := strings.TrimSpace(mcpsdk.ParseString(request, "reason", ""))
	if reason == "" {
		return nil, "reason is required — a gate with no stated question cannot be answered by anyone but you"
	}
	recommendedDefault := strings.TrimSpace(mcpsdk.ParseString(request, "recommended_default", ""))
	if recommendedDefault == "" {
		return nil, "recommended_default is required — without it this gate can never time out, " +
			"so it can only ever be resolved by finding a human. State what you will do if nobody " +
			"answers (e.g. \"merge; the gateway is inactive so no client can be charged\")."
	}

	class := strings.TrimSpace(mcpsdk.ParseString(request, "class", ""))
	if class != "" && class != "hard" && class != "soft" {
		return nil, fmt.Sprintf("class must be 'hard' or 'soft' (got %q)", class)
	}

	var deadline *time.Time
	if raw := strings.TrimSpace(mcpsdk.ParseString(request, "deadline", "")); raw != "" {
		parsed, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			// Refused, not silently dropped: a deadline the caller believes they set,
			// which silently became "no deadline", is the exact failure shape this
			// whole card removes.
			return nil, fmt.Sprintf("deadline %q is not RFC3339 (e.g. 2026-09-09T12:00:00Z): %v", raw, err)
		}
		deadline = &parsed
	}

	// Four-question predicate. Validated for PRESENCE here (so the caller is told what
	// to write, in words the HTTP 422 cannot carry), but never EVALUATED here — the
	// server decides. A second copy of the decision rule in the client is exactly the
	// drift this fleet keeps paying for.
	predicate := map[string]any{}
	for _, f := range []struct {
		boolKey, reasonKey string
	}{
		{"credential_exists", "credential_reason"},
		{"reversible", "reversible_reason"},
		{"blocked_by_other_task", "blocked_reason"},
		{"customer_visible_now", "customer_reason"},
	} {
		reason := strings.TrimSpace(mcpsdk.ParseString(request, f.reasonKey, ""))
		if reason == "" {
			return nil, fmt.Sprintf(
				"%s is required — one line saying why you answered %s as you did. "+
					"The audit found 40-45%% of asks to a human were decidable from a rule "+
					"already written down; a bare true/false with no reason is what made "+
					"those invisible.", f.reasonKey, f.boolKey)
		}
		predicate[f.boolKey] = mcpsdk.ParseBoolean(request, f.boolKey, false)
		predicate[f.reasonKey] = reason
	}

	// copy_tier (task 1.17a). Optional here — the server is the one place that knows
	// whether `reason` is copy-shaped and this field is therefore required; duplicating
	// that regex client-side would drift from the server's copy of it (the same
	// argument that keeps Decide() itself server-only, see setHumanGateArgs.Predicate).
	if copyTier := strings.TrimSpace(mcpsdk.ParseString(request, "copy_tier", "")); copyTier != "" {
		predicate["copy_tier"] = copyTier
	}

	return &setHumanGateArgs{
		TaskID:             taskID,
		Reason:             reason,
		RecommendedDefault: recommendedDefault,
		Class:              class,
		Deadline:           deadline,
		Predicate:          predicate,
	}, ""
}

func (s *Server) handleSetHumanGate(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	args, refusal := parseSetHumanGateArgs(request)
	if refusal != "" {
		return errResult("%s", refusal)
	}

	result, err := s.getRESTClient(ctx).SetHumanGate(ctx, args.TaskID, args.Reason,
		args.RecommendedDefault, args.Class, args.Deadline, args.Predicate)
	if err != nil {
		return errResult("set_human_gate failed: %v", err)
	}
	return jsonResult(result)
}

func (s *Server) handleClearHumanGate(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	taskID := mcpsdk.ParseString(request, "task_id", "")
	if taskID == "" {
		return errResult("task_id is required")
	}
	result, err := s.getRESTClient(ctx).ClearHumanGate(ctx, taskID)
	if err != nil {
		// The server's 403 body already names the agent-reachable exits; pass it
		// through verbatim rather than replacing it with a generic "forbidden", which
		// is what agents read as "no exit exists" and escalate.
		return errResult("clear_human_gate failed: %v", err)
	}
	return jsonResult(result)
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
// record_owner_decision / get_canonical_updates
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

func (s *Server) handleRecordOwnerDecision(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
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
		"source:owner-decision",
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
		return errResult("%s failed: %v", toolRecordOwnerDecision, err)
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
		decidedBy, deciderErr := s.resolveDeciderUserID(ctx)
		if deciderErr != nil {
			resp["human_gate_decision_error"] = fmt.Sprintf("could not resolve the deciding user's id: %v", deciderErr)
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

// resolveDeciderUserID finds the user UUID recorded as decided_by on a
// human_gate decision (contract docs/human-gate-decision-recorded.md §3 in
// evc-mesh). If MESH_MCP_DECIDER_USERNAME is set, the human with that username
// wins; otherwise — and as a fallback — the first human with role=="owner".
func (s *Server) resolveDeciderUserID(ctx context.Context) (string, error) {
	session := s.getSession(ctx)
	if session == nil {
		return "", fmt.Errorf("not authenticated: no agent session")
	}
	dir, err := s.getRESTClient(ctx).GetTeamDirectory(ctx, session.WorkspaceID.String())
	if err != nil {
		return "", err
	}
	preferred := strings.TrimSpace(os.Getenv(envDeciderUsername))
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
		if username, _ := hm["username"].(string); preferred != "" && username == preferred {
			return id, nil
		}
		if ownerID == "" {
			if role, _ := hm["role"].(string); role == "owner" {
				ownerID = id
			}
		}
	}
	if ownerID != "" {
		if preferred != "" {
			log.Printf("%s: %s=%q matches no human in the team directory; recording the workspace owner as decided_by", toolRecordOwnerDecision, envDeciderUsername, preferred)
		}
		return ownerID, nil
	}
	if preferred != "" {
		return "", fmt.Errorf("no user with username=%s or role=owner found in team directory", preferred)
	}
	return "", fmt.Errorf("no user with role=owner found in team directory")
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
// Source: an earlier data audit — one logical project written under 2-3 slug variants.
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
