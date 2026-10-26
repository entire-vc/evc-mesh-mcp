package mcp

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// staleUpdateAfter is how long the project's Updates feed may stay silent
// before the latest-update block says so. The daily report is a fleet rule,
// not a Mesh schedule, so the server only makes the silence visible.
const staleUpdateAfter = 36 * time.Hour

const projectUpdatesPurpose = "Project Updates (/w/<workspace>/p/<slug>/updates) is the project's daily status report written by its lead: a short health call, what moved, what is blocked, what comes next. Task counts are computed by the server."

// CreateProjectUpdate posts a report to a project's Updates feed.
func (c *RESTClient) CreateProjectUpdate(ctx context.Context, projectID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/projects/"+projectID+"/updates", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ListProjectUpdates returns one page of a project's updates, newest first.
func (c *RESTClient) ListProjectUpdates(ctx context.Context, projectID string, pageSize int) (map[string]any, error) {
	q := url.Values{}
	q.Set("page_size", strconv.Itoa(pageSize))
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/projects/"+projectID+"/updates?"+q.Encode(), nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// latestProjectUpdateBlock is the compact "last report" block attached to
// get_project and get_task_context, so a report is read without a separate
// step. It never fails the caller: on a read error it returns nil.
func (s *Server) latestProjectUpdateBlock(ctx context.Context, projectID string) map[string]any {
	if projectID == "" {
		return nil
	}
	page, err := s.getRESTClient(ctx).ListProjectUpdates(ctx, projectID, 1)
	if err != nil {
		return nil
	}
	items, _ := page["items"].([]any)
	var latest map[string]any
	if len(items) > 0 {
		latest, _ = items[0].(map[string]any)
	}
	return buildLatestUpdateBlock(latest, s.projectUpdatesPageURL(ctx, projectID), time.Now())
}

// projectUpdatesPageURL is the UI link /w/<workspace-slug>/p/<project-slug>/updates
// on the public origin. "" when the project, its workspace or the origin cannot
// be resolved: a wrong link is worse than none.
func (s *Server) projectUpdatesPageURL(ctx context.Context, projectID string) string {
	c := s.getRESTClient(ctx)
	origin := strings.TrimSuffix(c.taskURLPrefix(), "/t/")
	if origin == "" {
		return ""
	}
	project, err := c.GetProject(ctx, projectID)
	if err != nil {
		return ""
	}
	projectSlug, _ := project["slug"].(string)
	workspaceID, _ := project["workspace_id"].(string)
	if projectSlug == "" || workspaceID == "" {
		return ""
	}
	var ws map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/workspaces/"+workspaceID, nil, &ws); err != nil {
		return ""
	}
	wsSlug, _ := ws["slug"].(string)
	if wsSlug == "" {
		return ""
	}
	return origin + "/w/" + wsSlug + "/p/" + projectSlug + "/updates"
}

// buildLatestUpdateBlock shapes the block from the newest update (nil when the
// feed is empty). A feed silent for more than staleUpdateAfter, or empty, is
// stated outright rather than left as an absence.
func buildLatestUpdateBlock(latest map[string]any, pageURL string, now time.Time) map[string]any {
	block := map[string]any{
		"purpose": "Daily lead report for this project. Read it before you start; if it is missing or stale, the lead owes one.",
	}
	if pageURL != "" {
		block["url"] = pageURL
	}
	if latest == nil {
		block["missing"] = true
		block["notice"] = "no project update has been posted yet"
		return block
	}
	created, _ := latest["created_at"].(string)
	block["created_at"] = created
	block["title"] = latest["title"]
	block["status"] = latest["status"]
	block["summary"] = latest["summary"]
	block["blockers"] = latest["blockers"]
	block["highlights"] = latest["highlights"]
	block["next_steps"] = latest["next_steps"]
	if ts, err := time.Parse(time.RFC3339Nano, created); err == nil && now.Sub(ts) > staleUpdateAfter {
		block["stale"] = true
		block["notice"] = fmt.Sprintf("no project update since %s", ts.UTC().Format("2006-01-02 15:04 UTC"))
	}
	return block
}

func (s *Server) handlePostProjectUpdate(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	projectID := mcpsdk.ParseString(request, "project_id", "")
	title := mcpsdk.ParseString(request, "title", "")
	status := mcpsdk.ParseString(request, "status", "")
	summary := mcpsdk.ParseString(request, "summary", "")
	if projectID == "" || title == "" || status == "" || summary == "" {
		return errResult("project_id, title, status and summary are required")
	}

	body := map[string]any{"title": title, "status": status, "summary": summary}
	for _, key := range []string{"highlights", "blockers", "next_steps"} {
		raw, _ := request.GetArguments()[key].([]any)
		list := make([]map[string]string, 0, len(raw))
		for _, it := range raw {
			if text, _ := it.(string); strings.TrimSpace(text) != "" {
				list = append(list, map[string]string{"text": strings.TrimSpace(text)})
			}
		}
		if len(list) > 0 {
			body[key] = list
		}
	}

	update, err := s.getRESTClient(ctx).CreateProjectUpdate(ctx, projectID, body)
	if err != nil {
		return errResult("failed to post project update: %v", err)
	}
	return jsonResult(update)
}

func (s *Server) handleGetProjectUpdates(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}
	limit := int(mcpsdk.ParseFloat64(request, "limit", 1))
	if limit < 1 {
		limit = 1
	}
	if limit > 50 {
		limit = 50
	}
	var since time.Time
	if raw := mcpsdk.ParseString(request, "since", ""); raw != "" {
		ts, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			return errResult("since must be an RFC3339 timestamp, got %q", raw)
		}
		since = ts
	}

	// The REST list has no time filter; a since-bounded read takes the widest
	// page and cuts it here, newest first.
	pageSize := limit
	if !since.IsZero() {
		pageSize = 50
	}
	page, err := s.getRESTClient(ctx).ListProjectUpdates(ctx, projectID, pageSize)
	if err != nil {
		return errResult("failed to get project updates: %v", err)
	}
	items, _ := page["items"].([]any)
	out := make([]any, 0, len(items))
	for _, it := range items {
		u, _ := it.(map[string]any)
		if !since.IsZero() {
			created, _ := u["created_at"].(string)
			ts, perr := time.Parse(time.RFC3339Nano, created)
			if perr != nil || ts.Before(since) {
				continue
			}
		}
		out = append(out, it)
		if len(out) == limit {
			break
		}
	}
	return jsonResult(map[string]any{
		"updates": out,
		"total":   page["total_count"],
		"purpose": projectUpdatesPurpose,
	})
}
