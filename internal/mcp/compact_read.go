package mcp

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// A failed status lookup must never prevent a task read or invent a status.
// Its UUID and explicit error remain available as a fail-open fallback.
func (s *Server) compactTaskStatus(ctx context.Context, task map[string]any, id string) {
	if slug, ok := task["status_slug"].(string); ok && slug != "" {
		task["status"] = slug
		delete(task, "status_id")
		return
	}
	if slug, ok := task["status"].(string); ok && slug != "" {
		delete(task, "status_id")
		return
	}
	statusID, ok := task["status_id"].(string)
	if !ok || statusID == "" {
		return
	}
	statuses, err := s.getRESTClient(ctx).GetTaskStatuses(ctx, id)
	if err == nil {
		for _, status := range statuses {
			if status["id"] == statusID {
				if slug, ok := status["slug"].(string); ok && slug != "" {
					task["status"] = slug
					delete(task, "status_id")
					return
				}
			}
		}
	}
	task["status_lookup_error"] = "status slug unavailable; status_id retained (retry or use full=true)"
}

// One small object per edge, preserving direction, identity and relation type.
func compactDependencies(edges []map[string]any, incoming bool) []map[string]any {
	out := make([]map[string]any, 0, len(edges))
	for _, edge := range edges {
		row := map[string]any{}
		key := "depends_on_task_id"
		if incoming {
			key = "task_id"
		}
		if v, ok := edge[key]; ok {
			row["task_id"] = v
		}
		for _, k := range []string{"related_task_title", "related_task_status_id", "dependency_type"} {
			if v, ok := edge[k]; ok {
				row[k] = v
			}
		}
		out = append(out, row)
	}
	return out
}

// after is resolved against the complete newest-first walk BEFORE slicing a
// page. Filtering just the requested page would silently lose newer comments
// when a cursor is older than that page. Unknown IDs fail rather than return
// an ambiguous empty success. The API currently has no server-side after.
func (s *Server) commentsAfter(ctx context.Context, taskID, after string, params map[string]string) (map[string]any, error) {
	stamp, stampErr := time.Parse(time.RFC3339Nano, after)
	if stampErr != nil {
		cursor, err := uuid.Parse(after)
		if err != nil {
			return nil, fmt.Errorf("after must be a comment UUID or RFC3339 timestamp")
		}
		after = cursor.String()
	}
	limit := defaultListCommentsLimit
	if n, err := strconv.Atoi(params["page_size"]); err == nil && n > 0 {
		limit = n
	}
	if limit > 200 {
		limit = 200
	}
	page := 1
	if n, err := strconv.Atoi(params["page"]); err == nil && n > 0 {
		page = n
	}
	if params["sort_dir"] != "asc" && params["sort_dir"] != "desc" {
		return nil, fmt.Errorf("order must be asc or desc")
	}
	walk := map[string]string{"page_size": "200", "sort_dir": "desc"}
	if v, ok := params["include_internal"]; ok {
		walk["include_internal"] = v
	}
	items := []any{}
	found := false
	var totalCount any
	for n := 1; n <= 1000; n++ {
		walk["page"] = strconv.Itoa(n)
		result, err := s.getRESTClient(ctx).ListComments(ctx, taskID, walk)
		if err != nil {
			return nil, err
		}
		if n == 1 {
			totalCount = result["total_count"]
		} else if result["total_count"] != totalCount {
			return nil, fmt.Errorf("comments changed during after walk; retry from page 1")
		}
		rows, ok := result["items"].([]any)
		if !ok {
			return nil, fmt.Errorf("invalid comments page")
		}
		for _, item := range rows {
			m, ok := item.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("invalid comment item")
			}
			if stampErr != nil {
				if m["id"] == after {
					found = true
					break
				}
				items = append(items, item)
			} else {
				t, err := time.Parse(time.RFC3339Nano, stringField(m, "created_at"))
				if err != nil {
					return nil, fmt.Errorf("invalid comment timestamp")
				}
				if t.After(stamp) {
					items = append(items, item)
				}
			}
		}
		more, _ := result["has_more"].(bool)
		if more && len(rows) == 0 {
			return nil, fmt.Errorf("empty comments page claims more items")
		}
		if found || !more {
			if stampErr != nil && !found {
				return nil, fmt.Errorf("after comment not found in this visible task thread")
			}
			if params["sort_dir"] == "asc" {
				for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
					items[i], items[j] = items[j], items[i]
				}
			}
			// Compare page before multiplication to avoid overflow on huge input.
			start := len(items)
			if page-1 <= len(items)/limit {
				start = (page - 1) * limit
			}
			end := start + limit
			if end > len(items) {
				end = len(items)
			}
			totalPages := (len(items) + limit - 1) / limit
			return map[string]any{"items": items[start:end], "page": page, "page_size": limit, "total_count": len(items), "total_pages": totalPages, "has_more": end < len(items)}, nil
		}
	}
	return nil, fmt.Errorf("after walk exceeded 1000 pages; use a narrower timestamp")
}

func stringField(m map[string]any, key string) string { v, _ := m[key].(string); return v }
