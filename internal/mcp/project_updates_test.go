package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// projectUpdatesHarness is an in-memory /projects/:id/updates: POST appends,
// GET lists newest first, so write → read → get_project is a real round trip.
func projectUpdatesHarness(t *testing.T, projectID string, seeded []map[string]any) (*Server, *[]map[string]any) {
	t.Helper()
	store := append([]map[string]any(nil), seeded...)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		base := "/api/v1/projects/" + projectID
		switch {
		case r.URL.Path == base+"/updates" && r.Method == http.MethodPost:
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			body["id"] = uuid.New().String()
			body["created_at"] = time.Now().UTC().Format(time.RFC3339Nano)
			store = append([]map[string]any{body}, store...)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(body)
		case r.URL.Path == base+"/updates":
			_ = json.NewEncoder(w).Encode(map[string]any{"items": store, "total_count": len(store)})
		case strings.HasSuffix(r.URL.Path, "/context"):
			_ = json.NewEncoder(w).Encode(map[string]any{"task": map[string]any{"id": "t", "project_id": projectID}})
		case r.URL.Path == base:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": projectID, "name": "P", "slug": "mesh-dev", "workspace_id": "ws-uuid"})
		case r.URL.Path == "/api/v1/workspaces/ws-uuid":
			_ = json.NewEncoder(w).Encode(map[string]any{"id": "ws-uuid", "slug": "ws-006901c7"})
		case r.URL.Path == base+"/statuses", r.URL.Path == base+"/custom-fields":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}, &store
}

func callUpdTool(t *testing.T, h func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error), args map[string]any) (bool, map[string]any) {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	res, err := h(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if txt, ok := res.Content[0].(mcpsdk.TextContent); ok {
		_ = json.Unmarshal([]byte(txt.Text), &out)
	}
	return res.IsError, out
}

func TestProjectUpdate_PostThenReadThenGetProject(t *testing.T) {
	pid := uuid.New().String()
	s, _ := projectUpdatesHarness(t, pid, nil)

	isErr, posted := callUpdTool(t, s.handlePostProjectUpdate, map[string]any{
		"project_id": pid, "title": "Day 1", "status": "on_track", "summary": "all fine",
		"blockers":   []any{"waiting on CI", "  "},
		"highlights": []any{"shipped X"}, "next_steps": []any{"do Y"},
	})
	if isErr {
		t.Fatalf("post failed: %v", posted)
	}
	blockers, _ := posted["blockers"].([]any)
	if len(blockers) != 1 || blockers[0].(map[string]any)["text"] != "waiting on CI" {
		t.Errorf("blockers must be sent as [{text}] with blanks dropped, got %v", posted["blockers"])
	}

	_, read := callUpdTool(t, s.handleGetProjectUpdates, map[string]any{"project_id": pid})
	ups, _ := read["updates"].([]any)
	if len(ups) != 1 || ups[0].(map[string]any)["title"] != "Day 1" {
		t.Fatalf("read-back must return the posted report, got %v", read)
	}

	_, proj := callUpdTool(t, s.handleGetProject, map[string]any{"project_id": pid})
	lu, _ := proj["latest_update"].(map[string]any)
	if lu["title"] != "Day 1" || lu["summary"] != "all fine" || lu["status"] != "on_track" {
		t.Errorf("get_project must carry the latest update, got %v", lu)
	}
	if hl, _ := lu["highlights"].([]any); len(hl) != 1 {
		t.Errorf("latest_update must carry highlights, got %v", lu["highlights"])
	}
	if ns, _ := lu["next_steps"].([]any); len(ns) != 1 {
		t.Errorf("latest_update must carry next_steps, got %v", lu["next_steps"])
	}
	if _, stale := lu["stale"]; stale {
		t.Errorf("a fresh report must not be marked stale: %v", lu)
	}
}

func TestProjectUpdate_EmptyFeedIsExplicit(t *testing.T) {
	pid := uuid.New().String()
	s, _ := projectUpdatesHarness(t, pid, nil)
	_, proj := callUpdTool(t, s.handleGetProject, map[string]any{"project_id": pid})
	lu, _ := proj["latest_update"].(map[string]any)
	if lu["missing"] != true || !strings.Contains(lu["notice"].(string), "no project update") {
		t.Errorf("an empty feed must be stated, got %v", lu)
	}
}

func TestBuildLatestUpdateBlock_StaleAfter36h(t *testing.T) {
	now := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	old := map[string]any{"title": "t", "status": "at_risk", "summary": "s", "created_at": now.Add(-37 * time.Hour).Format(time.RFC3339)}
	b := buildLatestUpdateBlock(old, "", now)
	if b["stale"] != true || !strings.Contains(b["notice"].(string), "no project update since 2026-10-23 23:00 UTC") {
		t.Errorf("37h-old report must be stale with its date, got %v", b)
	}
	fresh := map[string]any{"title": "t", "created_at": now.Add(-35 * time.Hour).Format(time.RFC3339)}
	if _, stale := buildLatestUpdateBlock(fresh, "", now)["stale"]; stale {
		t.Error("35h-old report must not be stale")
	}
}

func TestGetProjectUpdates_LimitAndSince(t *testing.T) {
	pid := uuid.New().String()
	mk := func(title string, ago time.Duration) map[string]any {
		return map[string]any{"title": title, "created_at": time.Now().Add(-ago).UTC().Format(time.RFC3339Nano)}
	}
	s, _ := projectUpdatesHarness(t, pid, []map[string]any{mk("new", time.Hour), mk("mid", 30*time.Hour), mk("old", 80*time.Hour)})

	_, r := callUpdTool(t, s.handleGetProjectUpdates, map[string]any{"project_id": pid, "limit": float64(2)})
	if n := len(r["updates"].([]any)); n != 2 {
		t.Errorf("limit=2 want 2, got %d", n)
	}
	since := time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339)
	_, r = callUpdTool(t, s.handleGetProjectUpdates, map[string]any{"project_id": pid, "limit": float64(10), "since": since})
	if n := len(r["updates"].([]any)); n != 2 {
		t.Errorf("since=-48h want new+mid, got %d", n)
	}
	if isErr, _ := callUpdTool(t, s.handleGetProjectUpdates, map[string]any{"project_id": pid, "since": "yesterday"}); !isErr {
		t.Error("a malformed since must be refused")
	}
}

func TestPostProjectUpdate_RequiredFields(t *testing.T) {
	s, _ := projectUpdatesHarness(t, "p", nil)
	if isErr, _ := callUpdTool(t, s.handlePostProjectUpdate, map[string]any{"project_id": "p", "title": "x"}); !isErr {
		t.Error("missing status/summary must be refused")
	}
}

func TestGetTaskContext_CarriesLatestProjectUpdate(t *testing.T) {
	pid := uuid.New().String()
	s, _ := projectUpdatesHarness(t, pid, []map[string]any{
		{"title": "Day 1", "status": "at_risk", "summary": "s", "created_at": time.Now().UTC().Format(time.RFC3339Nano)},
	})
	taskID := uuid.New().String()
	_, ctxOut := callUpdTool(t, s.handleGetTaskContext, map[string]any{"task_id": taskID})
	pu, _ := ctxOut["project_update"].(map[string]any)
	if pu["title"] != "Day 1" || pu["status"] != "at_risk" {
		t.Errorf("get_task_context must carry the project's latest update, got %v", ctxOut)
	}
}

func TestProjectUpdate_BlockLinksToUIPage(t *testing.T) {
	pid := uuid.New().String()
	s, _ := projectUpdatesHarness(t, pid, nil)
	_, proj := callUpdTool(t, s.handleGetProject, map[string]any{"project_id": pid})
	lu, _ := proj["latest_update"].(map[string]any)
	u, _ := lu["url"].(string)
	if !strings.HasSuffix(u, "/w/ws-006901c7/p/mesh-dev/updates") || strings.Contains(u, "/api/") {
		t.Errorf("latest_update.url must be the UI page, got %q", u)
	}
}
