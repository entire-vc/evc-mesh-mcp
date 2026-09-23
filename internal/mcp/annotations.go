package mcp

import (
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

// toolKind classifies a tool for the MCP behaviour annotations
// (readOnlyHint / destructiveHint / idempotentHint) that clients and MCP
// catalogs show next to each tool.
type toolKind int

const (
	// kindRead: reads only, never changes state.
	kindRead toolKind = iota + 1
	// kindAdditive: creates new state (a comment, a task, an event) without
	// overwriting or removing anything that already exists.
	kindAdditive
	// kindAdditiveIdempotent: like kindAdditive, but repeating the call with
	// the same arguments has no further effect (heartbeat, checkout renewal).
	kindAdditiveIdempotent
	// kindOverwrite: replaces existing values or removes data.
	kindOverwrite
	// kindOverwriteIdempotent: replaces existing values; repeating the call
	// with the same arguments leaves the same result (upserts, field updates).
	kindOverwriteIdempotent
)

// toolKinds is the single source of truth for tool annotations. Every
// registered tool MUST have an entry — TestEveryToolIsAnnotated fails the
// build otherwise, so a new tool can't ship without a deliberate choice.
var toolKinds = map[string]toolKind{
	// read-only
	"export_workspace_config":  kindRead,
	"get_artifact":             kindRead,
	"get_assignment_rules":     kindRead,
	"get_canonical":            kindRead,
	"get_canonical_updates":    kindRead,
	"get_context":              kindRead,
	"get_doc":                  kindRead,
	"get_my_rules":             kindRead,
	"get_my_tasks":             kindRead,
	"get_project":              kindRead,
	"get_project_knowledge":    kindRead,
	"get_project_rules":        kindRead,
	"get_recurring_history":    kindRead,
	"get_task":                 kindRead,
	"get_task_context":         kindRead,
	"get_team_directory":       kindRead,
	"get_workflow_rules":       kindRead,
	"list_artifacts":           kindRead,
	"list_comments":            kindRead,
	"list_doc_comments":        kindRead,
	"list_docs":                kindRead,
	"list_projects":            kindRead,
	"list_recurring_schedules": kindRead,
	"list_sub_agents":          kindRead,
	"list_tasks":               kindRead,
	"poll_tasks":               kindRead,
	"recall":                   kindRead,
	"recall_with_graph":        kindRead,
	"search_docs":              kindRead,

	// creates new state
	"add_comment":           kindAdditive,
	"add_dependency":        kindAdditive,
	"add_vcs_link":          kindAdditive,
	"comment_doc":           kindAdditive,
	"create_doc":            kindAdditive,
	"create_recurring_task": kindAdditive,
	"create_subtask":        kindAdditive,
	"create_task":           kindAdditive,
	"publish_event":         kindAdditive,
	"publish_summary":       kindAdditive,
	"register_sub_agent":    kindAdditive,
	"report_error":          kindAdditive,
	"session_report":        kindAdditive,
	"trigger_recurring_now": kindAdditive,
	"upload_artifact":       kindAdditive,
	"heartbeat":             kindAdditiveIdempotent,
	"extend_checkout":       kindAdditiveIdempotent,
	"checkout_task":         kindAdditiveIdempotent,
	"subscribe_events":      kindAdditiveIdempotent,

	// overwrites or removes existing state
	"forget":                    kindOverwrite,
	"delete_recurring_schedule": kindOverwrite,
	"import_workspace_config":   kindOverwrite,
	"pavel_decision":            kindOverwrite,
	"assign_task":               kindOverwriteIdempotent,
	"clear_human_gate":          kindOverwriteIdempotent,
	"move_task":                 kindOverwriteIdempotent,
	"release_task":              kindOverwriteIdempotent,
	"remember":                  kindOverwriteIdempotent,
	"set_human_gate":            kindOverwriteIdempotent,
	"set_project_knowledge":     kindOverwriteIdempotent,
	"update_agent_profile":      kindOverwriteIdempotent,
	"update_doc":                kindOverwriteIdempotent,
	"update_recurring_schedule": kindOverwriteIdempotent,
	"update_task":               kindOverwriteIdempotent,
}

// annotationFor returns the MCP behaviour annotation for a tool kind. All
// Mesh tools act only on the configured Mesh instance, so openWorldHint is
// false throughout.
func annotationFor(k toolKind) mcpsdk.ToolAnnotation {
	f := false
	t := true
	a := mcpsdk.ToolAnnotation{OpenWorldHint: &f}
	switch k {
	case kindRead:
		a.ReadOnlyHint, a.DestructiveHint, a.IdempotentHint = &t, &f, &t
	case kindAdditive:
		a.ReadOnlyHint, a.DestructiveHint, a.IdempotentHint = &f, &f, &f
	case kindAdditiveIdempotent:
		a.ReadOnlyHint, a.DestructiveHint, a.IdempotentHint = &f, &f, &t
	case kindOverwrite:
		a.ReadOnlyHint, a.DestructiveHint, a.IdempotentHint = &f, &t, &f
	case kindOverwriteIdempotent:
		a.ReadOnlyHint, a.DestructiveHint, a.IdempotentHint = &f, &t, &t
	}
	return a
}

// addTool registers a tool with its behaviour annotations applied from
// toolKinds. A tool missing from toolKinds is registered without hints (the
// MCP default is the most cautious reading); the test above keeps that from
// happening silently.
func (s *Server) addTool(tool mcpsdk.Tool, handler mcpserver.ToolHandlerFunc) {
	if k, ok := toolKinds[tool.Name]; ok {
		ann := annotationFor(k)
		ann.Title = tool.Annotations.Title
		tool.Annotations = ann
	}
	s.mcpServer.AddTool(tool, handler)
}
