package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

type outcomeCall struct {
	route string
	body  map[string]any
}

// turnOutcomeServer answers every request with status and payload, and records
// each one so a test can count them.
func turnOutcomeServer(t *testing.T, status int, payload map[string]any) (*Server, *[]outcomeCall) {
	t.Helper()
	var calls []outcomeCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		calls = append(calls, outcomeCall{route: r.Method + " " + r.URL.Path, body: b})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(srv.Close)
	return &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}, &calls
}

type turnCommandCase struct {
	tool    string
	handler func(*Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error)
	args    map[string]any
	route   string
	body    map[string]any
	// refusal is the contract's error code for a wrong target of this command.
	refusal string
}

func turnCommandCases() []turnCommandCase {
	task, other, req := uuid.NewString(), uuid.NewString(), uuid.NewString()
	return []turnCommandCase{
		{
			tool: "wait_for", handler: func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return s.handleWaitFor
			},
			args:  map[string]any{"task_id": task, "on": "mr", "provider": "gitlab", "project_path": "entire-vc/evc-mesh", "mr_iid": float64(12), "until": "merged", "note": "merge", "session_id": "sess-1", "target_task_id": other},
			route: "POST /api/v1/tasks/" + task + "/wait",
			body:  map[string]any{"on": "mr", "provider": "gitlab", "project_path": "entire-vc/evc-mesh", "mr_iid": float64(12), "until": "merged", "note": "merge", "session_id": "sess-1", "target_task_id": other},
			// target_task_id is passed through: the server reads only the fields of `on`.
			refusal: "invalid_target",
		},
		{
			tool: "continue_next", handler: func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return s.handleContinueNext
			},
			args:    map[string]any{"task_id": task, "note": "tests written", "turn_id": "t-3"},
			route:   "POST /api/v1/tasks/" + task + "/next",
			body:    map[string]any{"note": "tests written", "turn_id": "t-3"},
			refusal: "conflict",
		},
		{
			tool: "ask_agent", handler: func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return s.handleAskAgent
			},
			args:    map[string]any{"task_id": task, "to": "@Garfield", "question": "A or B?"},
			route:   "POST /api/v1/tasks/" + task + "/decision-requests",
			body:    map[string]any{"to": "Garfield", "question": "A or B?"},
			refusal: "not_an_agent",
		},
		{
			tool: "answer_decision", handler: func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return s.handleAnswerDecision
			},
			args:    map[string]any{"request_id": req, "answer": "B, because"},
			route:   "POST /api/v1/decision-requests/" + req + "/answer",
			body:    map[string]any{"answer": "B, because"},
			refusal: "not_found",
		},
		{
			tool: "finish_task", handler: func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return s.handleFinishTask
			},
			args:    map[string]any{"task_id": task, "result_kind": "change", "evidence": "go test: ok", "evidence_links": []any{"https://example.test/mr/1"}, "to": "review"},
			route:   "POST /api/v1/tasks/" + task + "/finish",
			body:    map[string]any{"result_kind": "change", "evidence": "go test: ok", "evidence_links": []any{"https://example.test/mr/1"}, "to": "review"},
			refusal: "not_found",
		},
		{
			tool: "hand_off", handler: func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return s.handleHandOff
			},
			args:    map[string]any{"task_id": task, "to": "Riker", "why": "accept the epic"},
			route:   "POST /api/v1/tasks/" + task + "/handoff",
			body:    map[string]any{"to": "Riker", "why": "accept the epic"},
			refusal: "not_an_agent",
		},
		{
			tool: "add_note", handler: func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return s.handleAddNote
			},
			args:    map[string]any{"task_id": task, "text": "found X"},
			route:   "POST /api/v1/tasks/" + task + "/notes",
			body:    map[string]any{"text": "found X"},
			refusal: "not_found",
		},
	}
}

func callOutcome(h func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error), args map[string]any) *mcpsdk.CallToolResult {
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	res, _ := h(context.Background(), req)
	return res
}

func TestTurnOutcomeCommands_PostTheContractBodyToTheContractRoute(t *testing.T) {
	for _, tc := range turnCommandCases() {
		t.Run(tc.tool, func(t *testing.T) {
			server, calls := turnOutcomeServer(t, http.StatusOK, map[string]any{"comment_id": "c1", "released": true})
			res := callOutcome(tc.handler(server), tc.args)
			if res.IsError {
				t.Fatalf("%s failed: %s", tc.tool, resultText(t, res))
			}
			if len(*calls) != 1 {
				t.Fatalf("want exactly one request, got %d", len(*calls))
			}
			got := (*calls)[0]
			if got.route != tc.route {
				t.Errorf("route = %q, want %q", got.route, tc.route)
			}
			gotJSON, _ := json.Marshal(got.body)
			wantJSON, _ := json.Marshal(tc.body)
			if string(gotJSON) != string(wantJSON) {
				t.Errorf("body = %s, want %s", gotJSON, wantJSON)
			}
			if !strings.Contains(resultText(t, res), `"comment_id":"c1"`) {
				t.Errorf("the server envelope must reach the agent: %s", resultText(t, res))
			}
		})
	}
}

// A wrong target is refused by the server; the agent must see the status and
// the contract's error_code, and the tool must not try again or do anything else.
func TestTurnOutcomeCommands_WrongTargetReturnsServerCodeWithOneRequest(t *testing.T) {
	statusFor := map[string]int{"conflict": http.StatusConflict, "not_found": http.StatusNotFound}
	for _, tc := range turnCommandCases() {
		t.Run(tc.tool, func(t *testing.T) {
			status, ok := statusFor[tc.refusal]
			if !ok {
				status = http.StatusUnprocessableEntity
			}
			server, calls := turnOutcomeServer(t, status, map[string]any{
				"code": status, "error_code": tc.refusal, "message": "target refused by server", "details": "target 42 is unknown",
			})
			res := callOutcome(tc.handler(server), tc.args)
			if !res.IsError {
				t.Fatalf("a refusal must be an error result: %s", resultText(t, res))
			}
			text := resultText(t, res)
			for _, want := range []string{tc.tool + " refused", fmt.Sprintf("%d %s", status, tc.refusal), "target refused by server", "target 42 is unknown"} {
				if !strings.Contains(text, want) {
					t.Errorf("refusal %q does not carry %q", text, want)
				}
			}
			if len(*calls) != 1 {
				t.Errorf("a refusal must cost exactly one request, got %d: %v", len(*calls), *calls)
			}
		})
	}
}

func timeWaitArgs() map[string]any {
	return map[string]any{
		"task_id": uuid.NewString(), "on": "time", "until_at": "2026-10-29T09:00:00+03:00",
		"event_ref": "nightly-export", "source": "https://status.example.com/exports",
		"why": "the export lands at 09:00 MSK; the report reads it",
	}
}

// A time wait carries its evidence to the server unchanged, and until_at keeps
// the offset it was given in: the server records until_at_offset from it.
func TestWaitForTime_SendsEvidenceAndOffsetAsGiven(t *testing.T) {
	server, calls := turnOutcomeServer(t, http.StatusOK, map[string]any{"until_at_offset": "+03:00", "evidence_status": "unverified"})
	args := timeWaitArgs()
	res := callOutcome(server.handleWaitFor, args)
	if res.IsError || len(*calls) != 1 {
		t.Fatalf("valid time wait: %s, %d requests", resultText(t, res), len(*calls))
	}
	body := (*calls)[0].body
	for _, k := range []string{"until_at", "event_ref", "source", "why"} {
		if body[k] != args[k] {
			t.Errorf("%s = %v, want %v", k, body[k], args[k])
		}
	}
	if text := resultText(t, res); !strings.Contains(text, `"evidence_status":"unverified"`) {
		t.Errorf("the server envelope must reach the agent: %s", text)
	}
}

// Shape errors the contract defines are refused before any request.
func TestWaitForTime_RefusesBadEvidenceShapeBeforeAnyRequest(t *testing.T) {
	cases := map[string]struct {
		edit func(map[string]any)
		want string
	}{
		"no until_at":       {func(a map[string]any) { delete(a, "until_at") }, "until_at is required"},
		"naive until_at":    {func(a map[string]any) { a["until_at"] = "2026-10-29T09:00:00" }, "RFC3339"},
		"relative until_at": {func(a map[string]any) { a["until_at"] = "6h" }, "RFC3339"},
		"no event_ref":      {func(a map[string]any) { delete(a, "event_ref") }, "missing_evidence: on=time needs event_ref"},
		"no source":         {func(a map[string]any) { delete(a, "source") }, "missing_evidence: on=time needs source"},
		"blank why":         {func(a map[string]any) { a["why"] = "  " }, "missing_evidence: on=time needs why"},
		"short why":         {func(a map[string]any) { a["why"] = "retry" }, "shorter than 10"},
		"long event_ref":    {func(a map[string]any) { a["event_ref"] = strings.Repeat("e", 501) }, "event_ref is 501 chars"},
		"card as source":    {func(a map[string]any) { a["source"] = "card:#some-card" }, "a card is not a source"},
		"http source":       {func(a map[string]any) { a["source"] = "http://status.example.com" }, "must be an https:// URL"},
		"bad comment uuid":  {func(a map[string]any) { a["source"] = "comment:42" }, "comment:<uuid>"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			server, calls := turnOutcomeServer(t, http.StatusOK, map[string]any{})
			args := timeWaitArgs()
			tc.edit(args)
			res := callOutcome(server.handleWaitFor, args)
			if text := resultText(t, res); !res.IsError || !strings.Contains(text, tc.want) {
				t.Errorf("want a refusal with %q, got %q", tc.want, text)
			}
			if len(*calls) != 0 {
				t.Errorf("a shape refusal must not reach the API: %v", *calls)
			}
		})
	}
	for _, src := range []string{"comment:" + uuid.NewString(), "artifact:" + uuid.NewString()} {
		server, calls := turnOutcomeServer(t, http.StatusOK, map[string]any{})
		args := timeWaitArgs()
		args["source"] = src
		if res := callOutcome(server.handleWaitFor, args); res.IsError || len(*calls) != 1 {
			t.Errorf("source %s must pass the local check: %s", src, resultText(t, res))
		}
	}
}

// What only the server can judge (a receipt the waiter wrote, a stale card
// version) comes back as the server said it, after exactly one request.
func TestWaitForTime_ServerRefusalPassedThroughWithOneRequest(t *testing.T) {
	for status, code := range map[int]string{http.StatusUnprocessableEntity: "missing_evidence", http.StatusConflict: "conflict"} {
		server, calls := turnOutcomeServer(t, status, map[string]any{"code": status, "error_code": code, "message": "receipt was written by the waiting agent"})
		res := callOutcome(server.handleWaitFor, timeWaitArgs())
		want := fmt.Sprintf("wait_for refused: %d %s: receipt was written by the waiting agent", status, code)
		if text := resultText(t, res); !res.IsError || !strings.Contains(text, want) {
			t.Errorf("want %q, got %q", want, text)
		}
		if len(*calls) != 1 {
			t.Errorf("%d: a refusal must cost exactly one request, got %d", status, len(*calls))
		}
	}
}

func TestTurnOutcomeCommands_ServerWithoutErrorCodeStillNamesTheStatus(t *testing.T) {
	server, _ := turnOutcomeServer(t, http.StatusForbidden, map[string]any{"message": "agent key required"})
	res := callOutcome(server.handleContinueNext, map[string]any{"task_id": uuid.NewString(), "note": "x"})
	if text := resultText(t, res); !res.IsError || !strings.Contains(text, "403 forbidden: agent key required") {
		t.Errorf("got %q", text)
	}
}

func TestFinishTask_RefusesWithoutResultKindOrEvidenceBeforeAnyRequest(t *testing.T) {
	server, calls := turnOutcomeServer(t, http.StatusOK, map[string]any{})
	task := uuid.NewString()
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no result_kind", map[string]any{"task_id": task, "evidence": "ok"}, "missing_result_kind"},
		{"unknown result_kind", map[string]any{"task_id": task, "result_kind": "feature", "evidence": "ok"}, "missing_result_kind"},
		{"no evidence", map[string]any{"task_id": task, "result_kind": "change"}, "missing_evidence"},
		{"blank evidence", map[string]any{"task_id": task, "result_kind": "change", "evidence": "   "}, "missing_evidence"},
		{"bad target status", map[string]any{"task_id": task, "result_kind": "change", "evidence": "ok", "to": "closed"}, "to must be done or review"},
	}
	for _, tc := range cases {
		res := callOutcome(server.handleFinishTask, tc.args)
		if !res.IsError || !strings.Contains(resultText(t, res), tc.want) {
			t.Errorf("%s: got %q, want refusal naming %q", tc.name, resultText(t, res), tc.want)
		}
	}
	if len(*calls) != 0 {
		t.Errorf("an invalid finish_task must not reach the API: %v", *calls)
	}
}

func TestTurnOutcomeCommands_RequiredArgumentsAreCheckedLocally(t *testing.T) {
	server, calls := turnOutcomeServer(t, http.StatusOK, map[string]any{})
	for _, tc := range turnCommandCases() {
		for key := range tc.args {
			if !isRequired(coreTool(t, tc.tool), key) {
				continue
			}
			args := map[string]any{}
			for k, v := range tc.args {
				if k != key {
					args[k] = v
				}
			}
			res := callOutcome(tc.handler(server), args)
			if !res.IsError || !strings.Contains(resultText(t, res), key) {
				t.Errorf("%s without %s: %s", tc.tool, key, resultText(t, res))
			}
		}
	}
	res := callOutcome(server.handleWaitFor, map[string]any{"task_id": uuid.NewString(), "on": "agent"})
	if !res.IsError || !strings.Contains(resultText(t, res), "ask_agent") {
		t.Errorf("wait_for on an agent must point to ask_agent: %s", resultText(t, res))
	}
	res = callOutcome(server.handleWaitFor, map[string]any{"task_id": uuid.NewString(), "on": "time", "until_at": "tomorrow"})
	if !res.IsError || !strings.Contains(resultText(t, res), "RFC3339") {
		t.Errorf("a non-RFC3339 until_at must be refused: %s", resultText(t, res))
	}
	if len(*calls) != 0 {
		t.Errorf("locally refused calls must not reach the API: %v", *calls)
	}
}

func TestTurnOutcomeCommands_ReleasedDropsCachedCheckout(t *testing.T) {
	server, _ := turnOutcomeServer(t, http.StatusOK, map[string]any{"released": true})
	task := uuid.NewString()
	server.checkouts.Store(task, "tok")
	callOutcome(server.handleHandOff, map[string]any{"task_id": task, "to": "Riker", "why": "accept"})
	if _, ok := server.checkouts.Load(task); ok {
		t.Error("a released card must not keep its cached checkout token")
	}

	server, _ = turnOutcomeServer(t, http.StatusOK, map[string]any{"released": false})
	server.checkouts.Store(task, "tok")
	callOutcome(server.handleContinueNext, map[string]any{"task_id": task, "note": "more"})
	if _, ok := server.checkouts.Load(task); !ok {
		t.Error("continue_next keeps the checkout, so its token must stay cached")
	}
}

func TestTurnOutcomeTools_RegisteredInCoreProfile(t *testing.T) {
	tools := NewServer(ServerConfig{Profile: ProfileCore}).MCPServer().ListTools()
	for _, name := range []string{"wait_for", "continue_next", "ask_agent", "answer_decision", "finish_task", "hand_off", "add_note"} {
		st, ok := tools[name]
		if !ok {
			t.Errorf("%s is not registered in the core profile", name)
			continue
		}
		if strings.Contains(st.Tool.Description, "WAIT") {
			t.Errorf("%s: the description must not teach marker lines: %s", name, st.Tool.Description)
		}
	}
	full := NewServer(ServerConfig{Profile: ProfileFull}).MCPServer().ListTools()
	if !strings.Contains(full["set_human_gate"].Tool.Description, "only this way") {
		t.Error("set_human_gate must say it is the only way to ask a person")
	}
}

// coreTool returns the registered schema of a core-profile tool.
func coreTool(t *testing.T, name string) mcpsdk.Tool {
	t.Helper()
	tools := NewServer(ServerConfig{Profile: ProfileCore}).MCPServer().ListTools()
	st, ok := tools[name]
	if !ok {
		t.Fatalf("%s not registered", name)
	}
	return st.Tool
}

func isRequired(tool mcpsdk.Tool, key string) bool {
	for _, r := range tool.InputSchema.Required {
		if r == key {
			return true
		}
	}
	return false
}
