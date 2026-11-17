package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// claimFake is a Mesh that creates every card with the given assignee and
// records the checkout and release requests it gets.
type claimFake struct {
	mu          sync.Mutex
	checkouts   []map[string]any
	releases    []map[string]any
	checkoutErr int // status of the checkout answer; 0 = 200 with a token
}

const claimToken = "11111111-2222-3333-4444-555555555555" // pragma: allowlist secret

func (f *claimFake) server(t *testing.T, projectID, parentID, taskID, assignee string) *httptest.Server {
	t.Helper()
	statuses := []map[string]any{
		{"id": "st-todo", "slug": "todo", "name": "Todo", "category": "todo"},
		{"id": "st-prog", "slug": "in_progress", "name": "In Progress", "category": "in_progress"},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && (r.URL.Path == "/api/v1/projects/"+projectID+"/statuses" ||
			r.URL.Path == "/api/v1/tasks/"+parentID+"/statuses"):
			_ = json.NewEncoder(w).Encode(statuses)
		case r.Method == http.MethodPost && (r.URL.Path == "/api/v1/projects/"+projectID+"/tasks" ||
			r.URL.Path == "/api/v1/tasks/"+parentID+"/subtasks"):
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID, "assignee_id": assignee,
				"assignee_type": "agent", "status_id": body["status_id"]})
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/"+taskID+"/checkout":
			f.checkouts = append(f.checkouts, body)
			if f.checkoutErr != 0 {
				w.WriteHeader(f.checkoutErr)
				_ = json.NewEncoder(w).Encode(map[string]any{"code": f.checkoutErr, "error_code": "task_already_checked_out",
					"message": "Task is already checked out"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"checkout_token": claimToken,
				"expires_at": "2026-11-16T19:00:00Z", "generation": 1})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/tasks/"+taskID+"/checkout":
			f.releases = append(f.releases, body)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

type claimCase struct {
	agent    uuid.UUID
	project  string
	parent   string
	task     string
	fake     *claimFake
	server   *Server
	ctx      context.Context
	assignee string
}

func newClaimCase(t *testing.T, assignee string) *claimCase {
	t.Helper()
	c := &claimCase{agent: uuid.New(), project: uuid.NewString(), parent: uuid.NewString(),
		task: uuid.NewString(), fake: &claimFake{}}
	if assignee == "" {
		assignee = c.agent.String()
	}
	c.assignee = assignee
	srv := c.fake.server(t, c.project, c.parent, c.task, assignee)
	c.server = &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	c.ctx = ContextWithSession(context.Background(), &AgentSession{AgentID: c.agent, WorkspaceID: uuid.New()})
	return c
}

func (c *claimCase) create(t *testing.T, tool, slug string) map[string]any {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	args := map[string]any{"title": "rogozhin.ru master", "assignee_id": c.assignee, "assignee_type": "agent"}
	if slug != "" {
		args["status_slug"] = slug
	}
	var h func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error)
	if tool == "create_subtask" {
		args["parent_task_id"] = c.parent
		h = c.server.handleCreateSubtask
	} else {
		args["project_id"] = c.project
		h = c.server.handleCreateTask
	}
	req.Params.Arguments = args
	res, err := h(c.ctx, req)
	if err != nil || res.IsError {
		t.Fatalf("%s failed: err=%v result=%s", tool, err, resultText(t, res))
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &out); err != nil {
		t.Fatalf("%s result is not JSON: %v", tool, err)
	}
	return out
}

func claimEnv(t *testing.T, on, runnerSession string) {
	t.Setenv(claimOwnCreatedEnv, on)
	t.Setenv(claimOwnCreatedTTLEnv, "")
	t.Setenv(runnerCheckoutSessionEnv, runnerSession)
}

// The duplicate-session case, from the MCP's side: a chat session (no Runner checkout in
// its environment) creates a card on itself in in_progress. The card is checked
// out to this session at once, so the Runner's feed shows a live checkout and
// starts no lane session on it; the token stays in the process for release_task.
func TestClaimOwnCreated_LiveSessionHoldsItsOwnInProgressCard(t *testing.T) {
	for _, tool := range []string{"create_task", "create_subtask"} {
		t.Run(tool, func(t *testing.T) {
			claimEnv(t, "1", "")
			c := newClaimCase(t, "")
			out := c.create(t, tool, "in_progress")
			if len(c.fake.checkouts) != 1 {
				t.Fatalf("want one checkout of the new card, got %d", len(c.fake.checkouts))
			}
			if ttl := c.fake.checkouts[0]["ttl_minutes"]; ttl != float64(claimOwnCreatedTTL) {
				t.Errorf("checkout ttl_minutes = %v, want %d", ttl, claimOwnCreatedTTL)
			}
			claim, _ := out[claimResultField].(map[string]any)
			if claim == nil || claim["held"] != true || claim["expires_at"] != "2026-11-16T19:00:00Z" {
				t.Fatalf("result %s = %v, want held=true with expires_at", claimResultField, out[claimResultField])
			}
			raw, _ := json.Marshal(out)
			if strings.Contains(string(raw), claimToken) {
				t.Errorf("the checkout token leaked into the tool result: %s", raw)
			}
			if tok, ok := c.server.checkouts.Load(c.task); !ok || tok != claimToken {
				t.Errorf("token not kept for release_task: %v", tok)
			}
			// release_task of this session hands the card to the lane: it sends the kept token.
			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = map[string]any{"task_id": c.task}
			res, _ := c.server.handleReleaseTask(c.ctx, req)
			if res.IsError || len(c.fake.releases) != 1 || c.fake.releases[0]["checkout_token"] != claimToken {
				t.Fatalf("release_task after the claim: %s, releases=%v", resultText(t, res), c.fake.releases)
			}
		})
	}
}

// Nothing is claimed where the card is the lane's queue or not this agent's.
func TestClaimOwnCreated_NoClaim(t *testing.T) {
	other := uuid.NewString()
	cases := []struct {
		name, on, runner, slug, assignee string
	}{
		{"switch off", "", "", "in_progress", ""},
		{"switch off explicitly", "0", "", "in_progress", ""},
		{"inside a Runner card session", "1", "07564092-dfd4-4cc8-b2d8-38f33b0555bd", "in_progress", ""},
		{"created in todo: the lane's queue", "1", "", "todo", ""},
		{"project default status", "1", "", "", ""},
		{"assigned to another agent", "1", "", "in_progress", other},
	}
	for _, tc := range cases {
		for _, tool := range []string{"create_task", "create_subtask"} {
			t.Run(tc.name+"/"+tool, func(t *testing.T) {
				claimEnv(t, tc.on, tc.runner)
				c := newClaimCase(t, tc.assignee)
				out := c.create(t, tool, tc.slug)
				if len(c.fake.checkouts) != 0 {
					t.Errorf("checkout sent: %v", c.fake.checkouts)
				}
				if _, ok := out[claimResultField]; ok {
					t.Errorf("result carries %s: %v", claimResultField, out[claimResultField])
				}
			})
		}
	}
}

// A refused checkout (the Runner's lane got there first) never fails the
// create: the card exists and the result says it is not held.
func TestClaimOwnCreated_RefusedCheckoutKeepsTheCreate(t *testing.T) {
	claimEnv(t, "1", "")
	c := newClaimCase(t, "")
	c.fake.checkoutErr = http.StatusConflict
	out := c.create(t, "create_task", "in_progress")
	if out["id"] != c.task {
		t.Fatalf("create result lost the card: %v", out)
	}
	claim, _ := out[claimResultField].(map[string]any)
	if claim == nil || claim["held"] != false || !strings.Contains(claim["reason"].(string), "checkout failed") {
		t.Fatalf("result %s = %v, want held=false with the reason", claimResultField, out[claimResultField])
	}
	if _, ok := c.server.checkouts.Load(c.task); ok {
		t.Error("a refused checkout left a token behind")
	}
}

func TestClaimOwnCreated_TTLFromEnv(t *testing.T) {
	for raw, want := range map[string]int{"": 60, "15": 15, "0": 60, "-3": 60, "x": 60, "1440": 1440, "1441": 60} {
		t.Setenv(claimOwnCreatedTTLEnv, raw)
		if got := claimOwnCreatedTTLMinutes(); got != want {
			t.Errorf("%s=%q: ttl %d, want %d", claimOwnCreatedTTLEnv, raw, got, want)
		}
	}
}
