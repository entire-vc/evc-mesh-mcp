package mcp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// Typed turn outcomes: the commands an agent ends its turn with, instead
// of a marker line in a comment. Contract: docs/api/typed-outcomes.md in
// evc-mesh. The server validates the target, writes the state, the comment and
// the release in one transaction, so these tools stay thin: they shape the
// body, refuse only what the contract says must never leave the client, and
// hand the server's refusal back unchanged. One call, one request — a refused
// command is never retried here, because a retry of a refusal is the same
// refusal and a retry of a success is a second outcome.

// waitKinds are the values of wait_for.on. A wait on a person is
// ask_human, a wait on another agent is ask_agent.
var waitKinds = map[string]bool{"card": true, "mr": true, "pipeline": true, "time": true, "merge": true, "registered": true, "manual": true}

// resultKinds are the values of finish_task.result_kind (tasks.result_kind).
var resultKinds = []string{"change", "defect", "research", "content", "ops"}

func (s *Server) registerTurnOutcomeTools() {
	sessionParams := []mcpsdk.ToolOption{
		mcpsdk.WithString("session_id", mcpsdk.Description("Optional runner session id (max 200 chars), copied onto the recorded outcome and wait.")),
		mcpsdk.WithString("turn_id", mcpsdk.Description("Optional turn id within the session (max 200 chars), copied onto the outcome.")),
	}
	with := func(opts ...mcpsdk.ToolOption) []mcpsdk.ToolOption { return append(opts, sessionParams...) }

	s.addTool(mcpsdk.NewTool("wait_for", with(
		mcpsdk.WithDescription("End your turn waiting on an event: another card (on=card), a merge request (on=mr), a CI pipeline or GitHub Actions run (on=pipeline), the runner's merge gate on a reviewed head (on=merge), a runner registration (on=registered), the published moment of an external event (on=time, with event_ref, source and why) or, only on a card a lead labelled wake:manual, a passive wait with no observer and no timer that a new comment or the lead's action ends (on=manual, with note saying why). A GitHub PR merge is on=mr provider=github until=merged (project_path owner/repo, mr_iid the PR number, after_sha the head, after_status opened); on=merge is GitLab only. The server checks the target, records the wait (replacing your previous one on this card), writes the comment and releases your checkout; you are woken when the event happens. A refusal writes nothing. Waiting on another agent's answer is ask_agent; on a person, ask_human."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("The card you are working on (UUID or 6-12 hex short id).")),
		mcpsdk.WithString("on", mcpsdk.Required(), mcpsdk.Description("What you wait on: card, mr, pipeline, time, merge, registered or manual. Only the fields of that kind are read; after_* and registration* on a kind that does not take them are refused.")),
		mcpsdk.WithString("target_task_id", mcpsdk.Description("on=card: the card you wait on. Creates or reuses a blocks dependency on it. on=manual: optional, must be this card itself.")),
		mcpsdk.WithString("target_condition", mcpsdk.Description("on=card: what the card must reach: task_done (default), shipped, deployed or accepted.")),
		mcpsdk.WithString("vcs_link_id", mcpsdk.Description("on=mr: id of a VCS link of this card. Alternative to provider + project_path + mr_iid.")),
		mcpsdk.WithString("provider", mcpsdk.Description("on=mr: gitlab or github. on=pipeline: gitlab (default) or github for an Actions run (project_path owner/repo, pipeline_id the run id). on=merge: gitlab only; a GitHub PR merge is on=mr provider=github until=merged.")),
		mcpsdk.WithString("project_path", mcpsdk.Description("on=mr, pipeline or merge: project path, e.g. entire-vc/evc-mesh.")),
		mcpsdk.WithNumber("mr_iid", mcpsdk.Description("on=mr or merge: merge request iid within project_path (not the global id).")),
		mcpsdk.WithNumber("pipeline_id", mcpsdk.Description("on=pipeline: the pipeline id to wait on.")),
		mcpsdk.WithArray("required_jobs", mcpsdk.Description("on=pipeline, or on=mr with until green or changed: job names to read the wait from; omit for the whole pipeline."), mcpsdk.WithStringItems()),
		mcpsdk.WithString("until", mcpsdk.Description("on=mr: merged, green or changed. on=pipeline: finished, green or changed.")),
		mcpsdk.WithString("after_sha", mcpsdk.Description("on=merge, required: full 40 or 64 hex SHA of the reviewed head. on=mr or pipeline, optional baseline: the head SHA you observed.")),
		mcpsdk.WithNumber("after_pipeline_id", mcpsdk.Description("Baseline of until=green or changed: the pipeline id you observed (on=pipeline: equal to pipeline_id). Send with after_sha and after_status.")),
		mcpsdk.WithString("after_status", mcpsdk.Description("Baseline status: opened for until=merged; for until=green or changed the pipeline status you observed (running, failed, success, manual, ...).")),
		mcpsdk.WithString("registration", mcpsdk.Description("on=registered: ops-review, gmail, receipt, lifecycle, stall, recovery, queue or native.")),
		mcpsdk.WithString("registration_ref", mcpsdk.Description("on=registered: ops-review or native: the 64-hex id; gmail: gmr_<12 hex>; receipt, lifecycle, stall, recovery, queue: this card's 8-hex id (may be omitted).")),
		mcpsdk.WithString("until_at", mcpsdk.Description("on=time: when the external event happens, RFC3339 with Z or an offset (2026-10-29T06:00:00Z, 2026-10-29T09:00:00+03:00), 5 minutes to 30 days from now. Not a retry or poll interval: a wait on a card, MR or pipeline waits on that event.")),
		mcpsdk.WithString("event_ref", mcpsdk.Description("on=time, required: the external event the moment belongs to, e.g. nightly-export (max 500 chars).")),
		mcpsdk.WithString("source", mcpsdk.Description("on=time, required: where the event's time is published: an https:// URL, comment:<uuid> or artifact:<uuid> written by someone else. A card is not a source.")),
		mcpsdk.WithString("why", mcpsdk.Description("on=time, required: what happens at until_at and why not earlier, 10 to 500 chars.")),
		mcpsdk.WithString("note", mcpsdk.Description("Why you wait and what ends the wait; the generated comment shows it whole, the wait itself keeps its first 500 characters. Required for on=manual.")),
	)...), s.tracked("wait_for", s.handleWaitFor))

	s.addTool(mcpsdk.NewTool("continue_next", with(
		mcpsdk.WithDescription("End your turn with a piece of work done and more to do right away: records the outcome and the comment, keeps the card with you and extends your checkout. Reading the card or writing a plan is not a piece of work — keep working instead. Refused with 409 when you do not hold the card's checkout."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("The card you are working on (UUID or 6-12 hex short id).")),
		mcpsdk.WithString("note", mcpsdk.Required(), mcpsdk.Description("What was done this turn and what comes next, max 4096 chars.")),
	)...), s.tracked("continue_next", s.handleContinueNext))

	s.addTool(mcpsdk.NewTool("ask_agent", with(
		mcpsdk.WithDescription("End your turn with a question only another agent can answer: opens a decision request to that agent (or returns your identical open one), records a wait on it, writes the comment and releases your checkout; you are woken when they answer. The addressee must be an agent of the team directory, not a person (ask_human) and not yourself. Work another agent must do is hand_off, not a question."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("The card the question is about (UUID or 6-12 hex short id).")),
		mcpsdk.WithString("to", mcpsdk.Required(), mcpsdk.Description("Agent name or UUID from get_team_directory; an @-handle prefix is not needed.")),
		mcpsdk.WithString("question", mcpsdk.Required(), mcpsdk.Description("The question with the options you see, max 4000 chars.")),
	)...), s.tracked("ask_agent", s.handleAskAgent))

	s.addTool(mcpsdk.NewTool("ask_human", with(
		mcpsdk.WithDescription("End your turn with a question only a person can answer: posts it as a Blocking question to that person, which arms the card's human gate and notifies them, records your wait on the answer and releases your checkout; you are woken when the gate is lifted. Refused, with nothing posted, when to is not a person of the workspace or the previous question already has an answer (name what is new in supersedes). A question to an agent is ask_agent."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("The card the question is about (UUID or 6-12 hex short id).")),
		mcpsdk.WithString("to", mcpsdk.Required(), mcpsdk.Description("The person's username, e.g. alex; an @ prefix is not needed.")),
		mcpsdk.WithString("question", mcpsdk.Required(), mcpsdk.Description("The question with the options you see, max 2000 chars.")),
		mcpsdk.WithString("recommended_default", mcpsdk.Description("What you will do if nobody answers by the deadline; one line, max 500 chars.")),
		mcpsdk.WithString("deadline", mcpsdk.Description("When the default applies: RFC3339 with Z or an offset, in the future.")),
		mcpsdk.WithBoolean("soft", mcpsdk.Description("true: the gate may be released by the default-on-timeout sweep. Default false (hard).")),
		mcpsdk.WithString("canonical_key", mcpsdk.Description("Names the question; an existing recorded decision under the same key refuses the ask.")),
		mcpsdk.WithString("supersedes", mcpsdk.Description("What the previous, already answered, question did not cover; one line.")),
	)...), s.tracked("ask_human", s.handleAskHuman))

	s.addTool(mcpsdk.NewTool("answer_decision",
		mcpsdk.WithDescription("Answer a decision request another agent addressed to you: writes the answer as a comment on the asking card, marks the request answered and wakes the asker. Only the addressee or a person may answer; an answered or withdrawn request is refused with 409. Does not touch your own card or checkout."),
		mcpsdk.WithString("request_id", mcpsdk.Required(), mcpsdk.Description("Decision request id, as given in the question you received.")),
		mcpsdk.WithString("answer", mcpsdk.Required(), mcpsdk.Description("Your answer, max 4000 chars: the choice and the reason.")),
	), s.tracked("answer_decision", s.handleAnswerDecision))

	s.addTool(mcpsdk.NewTool("finish_task", with(
		mcpsdk.WithDescription("Close the card with its result: sets result_kind, moves the card to done (or review, when someone must accept a live result), writes the evidence as the closing comment, cancels your active wait and releases your checkout. Both result_kind and evidence are required; the existing move gates (closing comment length, linked MRs merged, open subtasks) still apply and their refusals are returned as is."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("The card to close (UUID or 6-12 hex short id).")),
		mcpsdk.WithString("result_kind", mcpsdk.Required(), mcpsdk.Description("What the card delivered: change, defect, research, content or ops.")),
		mcpsdk.WithString("evidence", mcpsdk.Required(), mcpsdk.Description("Proof of each acceptance criterion — commands and their quoted output, max 4096 chars.")),
		mcpsdk.WithArray("evidence_links", mcpsdk.Description("Optional URLs backing the evidence (MR, pipeline, artifact)."), mcpsdk.WithStringItems()),
		mcpsdk.WithString("to", mcpsdk.Description("Target status: done or review. review means the result is live and awaits someone's acceptance.")),
	)...), s.tracked("finish_task", s.handleFinishTask))

	s.addTool(mcpsdk.NewTool("hand_off", with(
		mcpsdk.WithDescription("Give the card to another agent who must do the next part: assigns it to them, moves it to todo, writes the comment, cancels your active wait and releases your checkout, in one step instead of assign + move + release. The addressee must be an agent of the team directory. Use ask_agent when you only need their answer and keep the card."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("The card to hand over (UUID or 6-12 hex short id).")),
		mcpsdk.WithString("to", mcpsdk.Required(), mcpsdk.Description("Agent name or UUID from get_team_directory who takes the card.")),
		mcpsdk.WithString("why", mcpsdk.Required(), mcpsdk.Description("What they must do and what is already done, max 4000 chars.")),
	)...), s.tracked("hand_off", s.handleHandOff))

	s.addTool(mcpsdk.NewTool("add_note", with(
		mcpsdk.WithDescription("Write a note on a card that wakes nobody: no mention or handoff delivery, no outcome, the checkout untouched. For progress logs and findings that need no action; a note does not end your turn — finish it with wait_for, continue_next, ask_agent, ask_human, finish_task or hand_off."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("The card to write the note on (UUID or 6-12 hex short id).")),
		mcpsdk.WithString("text", mcpsdk.Required(), mcpsdk.Description("Note body in markdown.")),
	)...), s.tracked("add_note", s.handleAddNote))
}

// turnCommand posts one typed-outcome command and returns the server's
// envelope. Exactly one request: no retry on any status.
func (c *RESTClient) turnCommand(ctx context.Context, path string, body map[string]any) (map[string]any, error) {
	return c.turnCommandKeyed(ctx, path, body, "")
}

// turnCommandKeyed is turnCommand with an Idempotency-Key: one request unless
// the key makes a repeat safe (idempotency.go). Only finish_task sends one;
// the server accepts the header on /finish only.
func (c *RESTClient) turnCommandKeyed(ctx context.Context, path string, body map[string]any, key string) (map[string]any, error) {
	var result map[string]any
	replayed, err := c.doJSONKeyed(ctx, http.MethodPost, path, body, &result, key)
	if err != nil {
		return nil, err
	}
	return markReplayed(result, replayed), nil
}

// turnOutcomeError renders a refusal with the HTTP status and the contract's
// error_code first, so an agent can branch on the code without parsing prose.
func turnOutcomeError(tool string, err error) (*mcpsdk.CallToolResult, error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return errResult("%s failed: %v", tool, err)
	}
	code, _ := apiErr.Body["error_code"].(string)
	if code == "" {
		code = strings.ToLower(strings.ReplaceAll(http.StatusText(apiErr.StatusCode), " ", "_"))
	}
	msg := fmt.Sprintf("%s refused: %d %s: %s", tool, apiErr.StatusCode, code, apiErr.Message)
	if details, ok := apiErr.Body["details"].(string); ok && details != "" && details != apiErr.Message {
		msg += " (" + details + ")"
	}
	if refusal := gateRefusalText(err); refusal != "" {
		msg += " [" + refusal + "]"
	}
	return errResult("%s", msg)
}

// sessionFields copies the optional session_id / turn_id onto a command body.
func sessionFields(request mcpsdk.CallToolRequest, body map[string]any) map[string]any {
	for _, k := range []string{"session_id", "turn_id"} {
		if v := strings.TrimSpace(mcpsdk.ParseString(request, k, "")); v != "" {
			body[k] = v
		}
	}
	return body
}

// requiredStrings reads trimmed required string arguments in order and names
// the first missing one.
func requiredStrings(request mcpsdk.CallToolRequest, keys ...string) ([]string, string) {
	out := make([]string, len(keys))
	for i, k := range keys {
		out[i] = strings.TrimSpace(mcpsdk.ParseString(request, k, ""))
		if out[i] == "" {
			return nil, k + " is required"
		}
	}
	return out, ""
}

// runTaskCommand posts a task-scoped command and forgets the cached checkout
// token when the server reports it released the card.
func (s *Server) runTaskCommand(ctx context.Context, tool, taskID, suffix string, body map[string]any) (*mcpsdk.CallToolResult, error) {
	client := s.getRESTClient(ctx)
	key := ""
	if tool == "finish_task" {
		key = client.writeKey(tool, taskID, body)
	}
	result, err := client.turnCommandKeyed(ctx, "/api/v1/tasks/"+url.PathEscape(taskID)+suffix, body, key)
	if err != nil {
		if key != "" && isKeyReused(err) {
			// The close already went through under this key; its answer was
			// lost. The server released the card then, so forget the token too.
			s.checkouts.Delete(taskID)
			return jsonResult(keyReusedResult(taskID))
		}
		return turnOutcomeError(tool, err)
	}
	if released, _ := result["released"].(bool); released {
		s.checkouts.Delete(taskID)
	}
	return jsonResult(result)
}

func (s *Server) handleWaitFor(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	req, refusal := requiredStrings(request, "task_id", "on")
	if refusal != "" {
		return errResult("%s", refusal)
	}
	taskID, on := req[0], req[1]
	if !waitKinds[on] {
		return errResult("on must be card, mr, pipeline, time, merge, registered or manual (got %q); a question to an agent is ask_agent, to a person ask_human", on)
	}
	body := map[string]any{"on": on}
	for _, k := range []string{"target_task_id", "target_condition", "vcs_link_id", "provider", "project_path", "until", "note", "event_ref", "source", "why",
		"after_sha", "after_status", "registration", "registration_ref"} {
		if v := strings.TrimSpace(mcpsdk.ParseString(request, k, "")); v != "" {
			body[k] = v
		}
	}
	for _, k := range []string{"mr_iid", "pipeline_id", "after_pipeline_id"} {
		if hasArgument(request, k) {
			body[k] = mcpsdk.ParseInt64(request, k, 0)
		}
	}
	if jobs := parseStringSlice(request, "required_jobs"); len(jobs) > 0 {
		body["required_jobs"] = jobs
	}
	if raw := strings.TrimSpace(mcpsdk.ParseString(request, "until_at", "")); raw != "" {
		if _, err := time.Parse(time.RFC3339, raw); err != nil {
			return errResult("until_at %q is not RFC3339 with Z or an offset (e.g. 2026-10-29T06:00:00Z): %v", raw, err)
		}
		// Sent as given: the server keeps the offset (until_at_offset).
		body["until_at"] = raw
	}
	if on == "time" {
		if refusal := timeWaitRefusal(body); refusal != "" {
			return errResult("wait_for refused locally: %s", refusal)
		}
	}
	return s.runTaskCommand(ctx, "wait_for", taskID, "/wait", sessionFields(request, body))
}

// timeWaitRefusal checks the shape of a time wait's evidence before the
// request, by the contract's rules; the server checks the same and also that a
// comment/artifact receipt exists and is not the waiter's own. An empty string
// means the shape is fine.
func timeWaitRefusal(body map[string]any) string {
	if _, ok := body["until_at"]; !ok {
		return "until_at is required for on=time"
	}
	var missing []string
	for _, k := range []string{"event_ref", "source", "why"} {
		if _, ok := body[k]; !ok {
			missing = append(missing, k)
		}
	}
	if len(missing) > 0 {
		return fmt.Sprintf("missing_evidence: on=time needs %s — the external event, where its time is published and why the card waits until then", strings.Join(missing, ", "))
	}
	for _, k := range []string{"event_ref", "source", "why"} {
		if v := body[k].(string); len([]rune(v)) > 500 {
			return fmt.Sprintf("%s is %d chars, max 500", k, len([]rune(v)))
		}
	}
	if why := body["why"].(string); len([]rune(why)) < 10 {
		return fmt.Sprintf("missing_evidence: why %q is shorter than 10 chars", why)
	}
	if !isTimeWaitSource(body["source"].(string)) {
		return fmt.Sprintf("missing_evidence: source %q must be an https:// URL, comment:<uuid> or artifact:<uuid>; a card is not a source — wait on it with on=card", body["source"])
	}
	return ""
}

func isTimeWaitSource(src string) bool {
	if strings.HasPrefix(src, "https://") {
		u, err := url.Parse(src)
		return err == nil && u.Host != ""
	}
	for _, prefix := range []string{"comment:", "artifact:"} {
		if rest, ok := strings.CutPrefix(src, prefix); ok {
			_, err := uuid.Parse(rest)
			return err == nil
		}
	}
	return false
}

func (s *Server) handleContinueNext(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	req, refusal := requiredStrings(request, "task_id", "note")
	if refusal != "" {
		return errResult("%s", refusal)
	}
	return s.runTaskCommand(ctx, "continue_next", req[0], "/next", sessionFields(request, map[string]any{"note": req[1]}))
}

func (s *Server) handleAskAgent(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	req, refusal := requiredStrings(request, "task_id", "to", "question")
	if refusal != "" {
		return errResult("%s", refusal)
	}
	body := map[string]any{"to": strings.TrimPrefix(req[1], "@"), "question": req[2]}
	return s.runTaskCommand(ctx, "ask_agent", req[0], "/decision-requests", sessionFields(request, body))
}

func (s *Server) handleAskHuman(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	req, refusal := requiredStrings(request, "task_id", "to", "question")
	if refusal != "" {
		return errResult("%s", refusal)
	}
	body := map[string]any{"to": strings.TrimPrefix(req[1], "@"), "question": req[2]}
	for _, k := range []string{"recommended_default", "canonical_key", "supersedes"} {
		if v := strings.TrimSpace(mcpsdk.ParseString(request, k, "")); v != "" {
			body[k] = v
		}
	}
	if raw := strings.TrimSpace(mcpsdk.ParseString(request, "deadline", "")); raw != "" {
		if _, err := time.Parse(time.RFC3339, raw); err != nil {
			return errResult("deadline %q is not RFC3339 with Z or an offset (e.g. 2026-11-12T09:00:00Z): %v", raw, err)
		}
		body["deadline"] = raw
	}
	if mcpsdk.ParseBoolean(request, "soft", false) {
		body["soft"] = true
	}
	return s.runTaskCommand(ctx, "ask_human", req[0], "/ask-human", sessionFields(request, body))
}

func (s *Server) handleAnswerDecision(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	req, refusal := requiredStrings(request, "request_id", "answer")
	if refusal != "" {
		return errResult("%s", refusal)
	}
	result, err := s.getRESTClient(ctx).turnCommand(ctx,
		"/api/v1/decision-requests/"+url.PathEscape(req[0])+"/answer", map[string]any{"answer": req[1]})
	if err != nil {
		return turnOutcomeError("answer_decision", err)
	}
	return jsonResult(result)
}

func (s *Server) handleFinishTask(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	taskID := strings.TrimSpace(mcpsdk.ParseString(request, "task_id", ""))
	if taskID == "" {
		return errResult("task_id is required")
	}
	// Both refusals are local on purpose: a closing without a kind or without
	// proof is never valid, so it must not cost a round trip or reach the
	// move gates with half a body.
	resultKind := strings.TrimSpace(mcpsdk.ParseString(request, "result_kind", ""))
	valid := false
	for _, k := range resultKinds {
		valid = valid || k == resultKind
	}
	if !valid {
		return errResult("missing_result_kind: result_kind must be one of %s (got %q)", strings.Join(resultKinds, ", "), resultKind)
	}
	evidence := strings.TrimSpace(mcpsdk.ParseString(request, "evidence", ""))
	if evidence == "" {
		return errResult("missing_evidence: evidence is required — the command and quoted output proving each acceptance criterion")
	}
	body := map[string]any{"result_kind": resultKind, "evidence": evidence}
	if links := parseStringSlice(request, "evidence_links"); len(links) > 0 {
		body["evidence_links"] = links
	}
	if to := strings.TrimSpace(mcpsdk.ParseString(request, "to", "")); to != "" {
		if to != "done" && to != "review" {
			return errResult("to must be done or review (got %q)", to)
		}
		body["to"] = to
	}
	return s.runTaskCommand(ctx, "finish_task", taskID, "/finish", sessionFields(request, body))
}

func (s *Server) handleHandOff(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	req, refusal := requiredStrings(request, "task_id", "to", "why")
	if refusal != "" {
		return errResult("%s", refusal)
	}
	body := map[string]any{"to": strings.TrimPrefix(req[1], "@"), "why": req[2]}
	return s.runTaskCommand(ctx, "hand_off", req[0], "/handoff", sessionFields(request, body))
}

func (s *Server) handleAddNote(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	req, refusal := requiredStrings(request, "task_id", "text")
	if refusal != "" {
		return errResult("%s", refusal)
	}
	return s.runTaskCommand(ctx, "add_note", req[0], "/notes", sessionFields(request, map[string]any{"text": req[1]}))
}
