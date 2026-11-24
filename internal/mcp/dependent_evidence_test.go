package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// fakeCrossWorkspaceAPI is the API as a blocker owner's key sees it: the waiting
// card lives in another workspace (every read or write naming it is 403, as
// RequireWorkspaceMember answers), the blocker is the caller's own, and its
// incoming edges list the waiting card's edge by id. Calls are recorded by
// "METHOD path".
func fakeCrossWorkspaceAPI(t *testing.T, waiting, blocker, edgeID string, readStatus int) (*Server, map[string]map[string]any, *[]string) {
	t.Helper()
	bodies := map[string]map[string]any{}
	var calls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/v1/tasks/"+waiting):
			w.WriteHeader(readStatus)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": readStatus, "message": "workspace access denied"})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+blocker+"/dependencies":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"outgoing": []map[string]any{},
				"incoming": []map[string]any{
					{"id": uuid.NewString(), "task_id": uuid.NewString(), "depends_on_task_id": blocker},
					{"id": edgeID, "task_id": waiting, "depends_on_task_id": blocker, "criterion": "the named result"},
				},
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/tasks/"+blocker+"/dependents/"+edgeID+"/evidence":
			var b map[string]any
			_ = json.NewDecoder(r.Body).Decode(&b)
			bodies["evidence"] = b
			_ = json.NewEncoder(w).Encode(map[string]any{"id": edgeID, "evidence_status": "trusted", "released_by": "evidence"})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}, bodies, &calls
}

func attestArgs(waiting, blocker string) map[string]any {
	return map[string]any{
		"task_id": waiting, "depends_on_task_id": blocker,
		"evidence_url": "https://git.example/mr/7", "evidence_criterion": "the named result", "attest": true,
	}
}

func TestUpdateDependency_BlockerOwnerAttestsFromBlockerSide(t *testing.T) {
	waiting, blocker, edgeID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	server, bodies, _ := fakeCrossWorkspaceAPI(t, waiting, blocker, edgeID, http.StatusForbidden)

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = attestArgs(waiting, blocker)
	req.Params.Arguments.(map[string]any)["expected_generation"] = float64(3)
	res, err := server.handleUpdateDependency(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("blocker-side attestation failed: %v %+v", err, res)
	}
	got := bodies["evidence"]
	if got == nil {
		t.Fatal("the evidence was not sent to the blocker-side route")
	}
	want := map[string]any{"evidence_url": "https://git.example/mr/7", "evidence_criterion": "the named result", "attest": true, "expected_generation": float64(3)}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %v, want %v (body %v)", k, got[k], v, got)
		}
	}
	for _, k := range []string{"condition", "criterion"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s must not be sent from the blocker side: %v", k, got)
		}
	}
	if text := resultText(t, res); !strings.Contains(text, `"trusted"`) {
		t.Errorf("result does not carry the stored evidence state: %s", text)
	}
}

func TestUpdateDependency_NoBlockerSideFallback(t *testing.T) {
	waiting, blocker, edgeID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	cases := map[string]struct {
		status int
		args   func(map[string]any)
	}{
		"not a 403":        {http.StatusNotFound, func(map[string]any) {}},
		"short task id":    {http.StatusForbidden, func(a map[string]any) { a["task_id"] = waiting[:8] }},
		"condition change": {http.StatusForbidden, func(a map[string]any) { a["condition"] = "task_done" }},
		"criterion change": {http.StatusForbidden, func(a map[string]any) { a["criterion"] = "other" }},
		"no evidence_url":  {http.StatusForbidden, func(a map[string]any) { delete(a, "evidence_url"); a["criterion"] = "x" }},
		"clearing":         {http.StatusForbidden, func(a map[string]any) { a["evidence_url"] = "" }},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server, bodies, calls := fakeCrossWorkspaceAPI(t, waiting, blocker, edgeID, tc.status)
			args := attestArgs(waiting, blocker)
			tc.args(args)
			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = args
			res, err := server.handleUpdateDependency(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if !res.IsError {
				t.Fatalf("expected the original refusal, got %s", resultText(t, res))
			}
			if bodies["evidence"] != nil {
				t.Errorf("must not fall back to the blocker-side route: %v", *calls)
			}
			for _, c := range *calls {
				if strings.Contains(c, "/dependents/") {
					t.Errorf("unexpected blocker-side call %s", c)
				}
			}
		})
	}
}

func TestUpdateDependency_BlockerSideEdgeMissing(t *testing.T) {
	waiting, blocker, edgeID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	server, bodies, _ := fakeCrossWorkspaceAPI(t, waiting, blocker, edgeID, http.StatusForbidden)
	req := mcpsdk.CallToolRequest{}
	// The pair in the wrong order: the blocker is not the waiting card's blocker.
	req.Params.Arguments = attestArgs(waiting, uuid.NewString())
	res, err := server.handleUpdateDependency(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError || bodies["evidence"] != nil {
		t.Fatalf("an edge that is not the blocker's must not be written: %s", resultText(t, res))
	}
}
