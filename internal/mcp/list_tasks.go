package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

const listTasksCategories = "backlog, todo, in_progress, review, done, cancelled"

// Keep this list in sync with the tool schema; the schema parity test pins it.
var listTasksParameters = []string{"assignee", "assignee_id", "assignee_type", "full", "labels", "limit", "list_revision", "order", "page", "priority", "project_id", "search", "sort", "status_category", "workspace_id"}

func validateListTasksArguments(request mcpsdk.CallToolRequest) error {
	args := request.GetArguments()
	keys := make([]string, 0, len(args))
	for k := range args {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		i := sort.SearchStrings(listTasksParameters, k)
		if i == len(listTasksParameters) || listTasksParameters[i] != k {
			return fmt.Errorf("unknown parameter %q; allowed parameters: %s; status_category values: %s", k, strings.Join(listTasksParameters, ", "), listTasksCategories)
		}
	}
	if v, ok := args["status_category"]; ok {
		cat, valid := v.(string)
		if !valid || !validListTasksCategory(cat) {
			return fmt.Errorf("invalid status_category %q; allowed parameters: %s; allowed values: %s", v, strings.Join(listTasksParameters, ", "), listTasksCategories)
		}
	}
	if v, ok := args["assignee_id"]; ok {
		value, valid := v.(string)
		if _, err := uuid.Parse(value); !valid || err != nil {
			return fmt.Errorf("assignee_id must be a UUID; use assignee for a name or me")
		}
	}
	if v, ok := args["assignee"]; ok {
		if text, valid := v.(string); !valid || strings.TrimSpace(text) == "" {
			return fmt.Errorf("assignee must be a name, UUID or me")
		}
	}
	return nil
}
func validListTasksCategory(cat string) bool {
	switch cat {
	case "backlog", "todo", "in_progress", "review", "done", "cancelled":
		return true
	}
	return false
}

func (s *Server) listTasksAssignee(ctx context.Context, request mcpsdk.CallToolRequest) (string, error) {
	alias := mcpsdk.ParseString(request, "assignee_id", "")
	if alias != "" {
		id, _ := uuid.Parse(alias)
		alias = id.String()
	}
	name := strings.TrimSpace(mcpsdk.ParseString(request, "assignee", ""))
	if name == "" {
		return alias, nil
	}
	var id string
	if parsed, err := uuid.Parse(name); err == nil {
		id = parsed.String()
	} else {
		session := s.getSession(ctx)
		if session == nil {
			return "", fmt.Errorf("assignee name/me requires an authenticated agent session")
		}
		if strings.EqualFold(name, "me") {
			id = session.AgentID.String()
		} else {
			team, err := s.getRESTClient(ctx).GetTeamDirectory(ctx, session.WorkspaceID.String())
			if err != nil {
				return "", fmt.Errorf("failed to resolve assignee: %w", err)
			}
			ids := map[string]bool{}
			for _, kind := range []string{"agents", "humans"} {
				rows, _ := team[kind].([]any)
				for _, raw := range rows {
					row, ok := raw.(map[string]any)
					if !ok {
						continue
					}
					if strings.EqualFold(name, stringField(row, "name")) {
						if parsed, err := uuid.Parse(stringField(row, "id")); err == nil {
							ids[parsed.String()] = true
						}
					}
				}
			}
			if len(ids) == 0 {
				return "", fmt.Errorf("unknown assignee %q; use an exact name, UUID or me", name)
			}
			if len(ids) > 1 {
				return "", fmt.Errorf("ambiguous assignee %q; use a UUID", name)
			}
			for matched := range ids {
				id = matched
			}
		}
	}
	if alias != "" && alias != id {
		return "", fmt.Errorf("assignee and assignee_id identify different principals")
	}
	return id, nil
}

// Look up statuses once per project, never once per row. Refuse a compact
// response without a resolved status slug; full mode remains available.
func (s *Server) compactListTasksPage(ctx context.Context, result map[string]any, full bool, projectID string) (map[string]any, error) {
	if full {
		return result, nil
	}
	items, ok := result["items"].([]any)
	if !ok {
		return result, nil
	}
	statuses := map[string]map[string]string{}
	rows := make([]any, 0, len(items))
	for _, raw := range items {
		task, ok := raw.(map[string]any)
		if !ok {
			rows = append(rows, raw)
			continue
		}
		row := map[string]any{}
		for _, key := range []string{"id", "title", "priority", "assignee_name", "labels", "updated_at", "parent_task_id", "start_after"} {
			if value, exists := task[key]; exists && value != nil {
				row[key] = value
			}
		}
		if projectID == "" {
			if value, exists := task["project_id"]; exists {
				row["project_id"] = value
			}
		}
		slug := stringField(task, "status_slug")
		if slug == "" {
			slug = stringField(task, "status")
		}
		statusID := stringField(task, "status_id")
		if slug == "" && statusID != "" {
			pid := stringField(task, "project_id")
			if pid == "" {
				pid = projectID
			}
			lookup, done := statuses[pid]
			if !done {
				lookup = map[string]string{}
				if pid != "" {
					client := s.getRESTClient(ctx)
					list, err := client.GetProjectStatuses(ctx, pid)
					var apiErr *APIError
					if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden && stringField(task, "id") != "" {
						// Workspace task visibility can exceed project membership.
						// This route applies the task's existing workspace access gate.
						list, err = client.GetTaskStatuses(ctx, stringField(task, "id"))
					}
					if err == nil {
						for _, st := range list {
							lookup[stringField(st, "id")] = stringField(st, "slug")
						}
					}
				}
				statuses[pid] = lookup
			}
			slug = lookup[statusID]
		}
		if slug != "" {
			row["status"] = slug
		} else if statusID != "" {
			return nil, fmt.Errorf("status slug unavailable for project %q; use full=true or retry", stringField(task, "project_id"))
		}
		rows = append(rows, row)
	}
	result["items"] = rows
	return result, nil
}
