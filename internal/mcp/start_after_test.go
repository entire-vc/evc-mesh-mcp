package mcp

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

type startAfterWriteCase struct {
	tool    string
	handler func(*Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error)
	args    func() map[string]any
	route   func(args map[string]any) string
}

func startAfterWriteCases() []startAfterWriteCase {
	project := uuid.NewString()
	return []startAfterWriteCase{
		{
			tool: "create_task",
			handler: func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return s.handleCreateTask
			},
			args:  func() map[string]any { return map[string]any{"project_id": project, "title": "nightly report"} },
			route: func(map[string]any) string { return "POST /api/v1/projects/" + project + "/tasks" },
		},
		{
			tool: "create_subtask",
			handler: func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return s.handleCreateSubtask
			},
			args: func() map[string]any {
				return map[string]any{"parent_task_id": uuid.NewString(), "title": "nightly report"}
			},
			route: func(a map[string]any) string {
				return "POST /api/v1/tasks/" + a["parent_task_id"].(string) + "/subtasks"
			},
		},
		{
			tool: "update_task",
			handler: func(s *Server) func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
				return s.handleUpdateTask
			},
			args:  func() map[string]any { return map[string]any{"task_id": uuid.NewString()} },
			route: func(a map[string]any) string { return "PATCH /api/v1/tasks/" + a["task_id"].(string) },
		},
	}
}

func withStartAfterEvidence(a map[string]any) map[string]any {
	a["start_after"] = "2026-11-01T09:00:00+03:00"
	a["start_after_origin"] = "agent_evidence"
	a["start_after_evidence"] = map[string]any{
		"event_ref": "nightly-export",
		"source":    "https://status.example.com/exports",
		"why":       "the export lands at 09:00 MSK; the report reads it",
	}
	return a
}

// The date, its origin and its evidence reach the server as given, in one
// request: the server keeps the offset on the receipt.
func TestStartAfterWrite_SendsEvidenceAsGiven(t *testing.T) {
	for _, tc := range startAfterWriteCases() {
		t.Run(tc.tool, func(t *testing.T) {
			server, calls := turnOutcomeServer(t, http.StatusOK, map[string]any{"id": uuid.NewString()})
			args := withStartAfterEvidence(tc.args())
			res := callOutcome(tc.handler(server), args)
			if res.IsError || len(*calls) != 1 {
				t.Fatalf("valid write: %s, %d requests", resultText(t, res), len(*calls))
			}
			call := (*calls)[0]
			if call.route != tc.route(args) {
				t.Errorf("route = %s, want %s", call.route, tc.route(args))
			}
			if call.body["start_after"] != "2026-11-01T09:00:00+03:00" || call.body["start_after_origin"] != "agent_evidence" {
				t.Errorf("start_after/origin not sent as given: %v", call.body)
			}
			ev, _ := call.body["start_after_evidence"].(map[string]any)
			want := args["start_after_evidence"].(map[string]any)
			for _, k := range []string{"event_ref", "source", "why"} {
				if ev[k] != want[k] {
					t.Errorf("start_after_evidence.%s = %v, want %v", k, ev[k], want[k])
				}
			}
		})
	}
}

// What only the server judges (an agent date without evidence, a retry
// posing as an event) reaches the agent with its status and error_code, after
// exactly one request.
func TestStartAfterWrite_ServerRefusalPassedThroughWithOneRequest(t *testing.T) {
	for _, tc := range startAfterWriteCases() {
		for _, code := range []string{"missing_evidence", "generic_retry"} {
			t.Run(tc.tool+"/"+code, func(t *testing.T) {
				server, calls := turnOutcomeServer(t, http.StatusUnprocessableEntity, map[string]any{
					"code": 422, "error_code": code, "message": "start_after from an agent needs start_after_evidence",
				})
				args := tc.args()
				args["start_after"] = "2026-11-01T06:00:00Z"
				res := callOutcome(tc.handler(server), args)
				text := resultText(t, res)
				for _, want := range []string{tc.tool + " refused", "422 " + code, "needs start_after_evidence"} {
					if !res.IsError || !strings.Contains(text, want) {
						t.Errorf("refusal %q does not carry %q", text, want)
					}
				}
				if len(*calls) != 1 {
					t.Errorf("a refusal must cost exactly one request, got %d", len(*calls))
				}
			})
		}
	}
}

// Shape errors are refused before any request.
func TestStartAfterWrite_RefusesBadShapeBeforeAnyRequest(t *testing.T) {
	ev := func(a map[string]any) map[string]any { return a["start_after_evidence"].(map[string]any) }
	cases := map[string]struct {
		edit func(map[string]any)
		want string
	}{
		"naive time":            {func(a map[string]any) { a["start_after"] = "2026-11-01T09:00:00" }, "invalid start_after format"},
		"relative time":         {func(a map[string]any) { a["start_after"] = "6h" }, "invalid start_after format"},
		"human origin":          {func(a map[string]any) { a["start_after_origin"] = "human_explicit" }, "origin_forbidden"},
		"legacy origin":         {func(a map[string]any) { a["start_after_origin"] = "legacy_unknown" }, "origin_forbidden"},
		"evidence without date": {func(a map[string]any) { delete(a, "start_after"); delete(a, "start_after_origin") }, "start_after_evidence is given without start_after"},
		"evidence not object":   {func(a map[string]any) { a["start_after_evidence"] = 42.0 }, "must be an object"},
		"null evidence":         {func(a map[string]any) { a["start_after_evidence"] = nil }, "must be an object"},
		"no event_ref":          {func(a map[string]any) { delete(ev(a), "event_ref") }, "missing_evidence: start_after_evidence needs event_ref"},
		"blank source":          {func(a map[string]any) { ev(a)["source"] = " " }, "missing_evidence: start_after_evidence needs source"},
		"short why":             {func(a map[string]any) { ev(a)["why"] = "retry" }, "shorter than 10"},
		"long why":              {func(a map[string]any) { ev(a)["why"] = strings.Repeat("w", 501) }, "why is 501 chars"},
		"card as source":        {func(a map[string]any) { ev(a)["source"] = "card:#some-card" }, "a card is not a source"},
		"http source":           {func(a map[string]any) { ev(a)["source"] = "http://status.example.com" }, "must be an https:// URL"},
		"bad artifact uuid":     {func(a map[string]any) { ev(a)["source"] = "artifact:42" }, "artifact:<uuid>"},
	}
	for _, tc := range startAfterWriteCases() {
		for name, c := range cases {
			t.Run(tc.tool+"/"+name, func(t *testing.T) {
				server, calls := turnOutcomeServer(t, http.StatusOK, map[string]any{})
				args := withStartAfterEvidence(tc.args())
				if tc.tool != "update_task" {
					// A status lookup would be a request too; the shape
					// refusal must come before it.
					args["status_slug"] = "todo"
				}
				c.edit(args)
				res := callOutcome(tc.handler(server), args)
				if text := resultText(t, res); !res.IsError || !strings.Contains(text, c.want) {
					t.Errorf("want a refusal with %q, got %q", c.want, text)
				}
				if len(*calls) != 0 {
					t.Errorf("a shape refusal must not reach the API: %v", *calls)
				}
			})
		}
	}
	// A null evidence without a date is an unset field, not a malformed one.
	server, calls := turnOutcomeServer(t, http.StatusOK, map[string]any{"id": uuid.NewString()})
	args := startAfterWriteCases()[2].args()
	args["title"], args["start_after_evidence"] = "renamed", nil
	if res := callOutcome(server.handleUpdateTask, args); res.IsError || len(*calls) != 1 {
		t.Errorf("null evidence without start_after must pass: %s", resultText(t, res))
	}
	// Positive control: a comment receipt and an evidence object sent as a
	// JSON string (some clients do) both pass the local check.
	for _, edit := range []func(map[string]any){
		func(a map[string]any) { ev(a)["source"] = "comment:" + uuid.NewString() },
		func(a map[string]any) {
			a["start_after_evidence"] = `{"event_ref":"nightly-export","source":"https://status.example.com","why":"the export lands at 09:00 MSK"}`
		},
	} {
		server, calls := turnOutcomeServer(t, http.StatusOK, map[string]any{"id": uuid.NewString()})
		args := withStartAfterEvidence(startAfterWriteCases()[2].args())
		edit(args)
		if res := callOutcome(server.handleUpdateTask, args); res.IsError || len(*calls) != 1 {
			t.Errorf("valid evidence must pass the local check: %s", resultText(t, res))
		}
	}
}

func clearStartAfterArgs() map[string]any {
	return map[string]any{
		"task_id": uuid.NewString(), "expected_start_after": "2026-11-01T06:00:00Z", "expected_version": 42.0,
		"expected_origin": "agent_evidence", "schedule_receipt_id": uuid.NewString(),
		"idempotency_key": "runner-turn-7f3a", "reason": "the export ran",
	}
}

func TestClearStartAfter_SendsCompareClear(t *testing.T) {
	server, calls := turnOutcomeServer(t, http.StatusOK, map[string]any{"replayed": false, "version": 43, "receipt": map[string]any{"op": "clear"}})
	args := clearStartAfterArgs()
	res := callOutcome(server.handleClearStartAfter, args)
	if res.IsError || len(*calls) != 1 {
		t.Fatalf("valid clear: %s, %d requests", resultText(t, res), len(*calls))
	}
	call := (*calls)[0]
	if want := "POST /api/v1/tasks/" + args["task_id"].(string) + "/start-after/compare-clear"; call.route != want {
		t.Errorf("route = %s, want %s", call.route, want)
	}
	for _, k := range []string{"expected_start_after", "expected_version", "expected_origin", "schedule_receipt_id", "idempotency_key", "reason"} {
		if call.body[k] != args[k] {
			t.Errorf("%s = %v, want %v", k, call.body[k], args[k])
		}
	}
	if text := resultText(t, res); !strings.Contains(text, `"version":43`) {
		t.Errorf("the server envelope must reach the agent: %s", text)
	}

	// Without a key one is generated, so the server's required field is never
	// missing; a person may omit origin and receipt.
	server, calls = turnOutcomeServer(t, http.StatusOK, map[string]any{})
	args = clearStartAfterArgs()
	for _, k := range []string{"idempotency_key", "expected_origin", "schedule_receipt_id", "reason"} {
		delete(args, k)
	}
	if res := callOutcome(server.handleClearStartAfter, args); res.IsError || len(*calls) != 1 {
		t.Fatalf("clear without optional fields: %s", resultText(t, res))
	}
	if key, _ := (*calls)[0].body["idempotency_key"].(string); !strings.HasPrefix(key, "mcp-") {
		t.Errorf("idempotency_key not generated: %v", (*calls)[0].body)
	}
}

// A changed card comes back as 409 cas_conflict with its current schedule,
// after one request; nothing is retried.
func TestClearStartAfter_CASConflictPassedThroughWithOneRequest(t *testing.T) {
	receipt := uuid.NewString()
	server, calls := turnOutcomeServer(t, http.StatusConflict, map[string]any{
		"code": "cas_conflict", "error_code": "cas_conflict", "message": "start_after changed since it was read",
		"current_version": 44, "current_start_after": "2026-11-02T06:00:00Z", "current_origin": "human_explicit", "current_receipt": receipt,
	})
	res := callOutcome(server.handleClearStartAfter, clearStartAfterArgs())
	text := resultText(t, res)
	for _, want := range []string{"clear_start_after refused", "409 cas_conflict", "changed since it was read", `"current_version":44`, `"current_origin":"human_explicit"`, receipt} {
		if !res.IsError || !strings.Contains(text, want) {
			t.Errorf("refusal %q does not carry %q", text, want)
		}
	}
	if len(*calls) != 1 {
		t.Errorf("a refusal must cost exactly one request, got %d", len(*calls))
	}
}

func TestClearStartAfter_RefusesBadShapeBeforeAnyRequest(t *testing.T) {
	cases := map[string]struct {
		edit func(map[string]any)
		want string
	}{
		"no task":           {func(a map[string]any) { delete(a, "task_id") }, "task_id is required"},
		"no expected date":  {func(a map[string]any) { delete(a, "expected_start_after") }, "expected_start_after is required"},
		"bad expected date": {func(a map[string]any) { a["expected_start_after"] = "tomorrow" }, "not RFC3339"},
		"no version":        {func(a map[string]any) { delete(a, "expected_version") }, "expected_version is required"},
		"bad receipt":       {func(a map[string]any) { a["schedule_receipt_id"] = "r-1" }, "not a UUID"},
		"long key":          {func(a map[string]any) { a["idempotency_key"] = strings.Repeat("k", 201) }, "max 200"},
		"long reason":       {func(a map[string]any) { a["reason"] = strings.Repeat("r", 501) }, "max 500"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			server, calls := turnOutcomeServer(t, http.StatusOK, map[string]any{})
			args := clearStartAfterArgs()
			c.edit(args)
			res := callOutcome(server.handleClearStartAfter, args)
			if text := resultText(t, res); !res.IsError || !strings.Contains(text, c.want) {
				t.Errorf("want a refusal with %q, got %q", c.want, text)
			}
			if len(*calls) != 0 {
				t.Errorf("a shape refusal must not reach the API: %v", *calls)
			}
		})
	}
}

// get_task's compact view carries what clear_start_after compares against.
func TestLeanGetTaskView_KeepsStartAfterSchedule(t *testing.T) {
	receipt := uuid.NewString()
	task := map[string]any{"id": "t", "title": "x", "start_after": "2026-11-01T06:00:00Z", "start_after_origin": "agent_evidence", "start_after_receipt_id": receipt, "version": 42.0}
	leanGetTaskView(task)
	if task["start_after_origin"] != "agent_evidence" || task["start_after_receipt_id"] != receipt || task["version"] != 42.0 {
		t.Errorf("schedule fields dropped from the compact view: %v", task)
	}
	plain := map[string]any{"id": "t", "title": "x", "version": 42.0}
	leanGetTaskView(plain)
	if _, ok := plain["version"]; ok {
		t.Errorf("version without a start_after is noise in the compact view: %v", plain)
	}
}
