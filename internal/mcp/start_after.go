package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// start_after with provenance. Contract: "`start_after` — provenance, evidence
// and compare-clear" in docs/api/typed-outcomes.md of evc-mesh. The server
// derives the origin from the caller and checks the evidence; this side checks
// only the shape, so a malformed write costs no request, and hands every
// server refusal (missing_evidence, generic_retry, cas_conflict ...) back with
// its error_code after exactly one request.

const startAfterOriginAgent = "agent_evidence"

// startAfterParams are the start_after fields shared by create_task,
// create_subtask and update_task.
func startAfterParams(subject string, clearHint bool) []mcpsdk.ToolOption {
	desc := "Hold the " + subject + " out of the queue until this moment, RFC3339 with Z or an offset (2026-11-01T09:00:00+03:00). " +
		"From an agent it must be the published time of an external event, given with start_after_evidence; not a retry or poll interval. Independent of due_date."
	if clearHint {
		desc += " An empty value is refused: clear a date with clear_start_after."
	}
	return []mcpsdk.ToolOption{
		mcpsdk.WithString("start_after", mcpsdk.Description(desc)),
		mcpsdk.WithString("start_after_origin", mcpsdk.Description("Optional, only agent_evidence. The server derives the origin from the caller; a person's date needs no evidence.")),
		mcpsdk.WithObject("start_after_evidence",
			mcpsdk.Description("Required by the server for an agent's start_after: {event_ref, source, why}, the same rules as wait_for on=time."),
			mcpsdk.Properties(map[string]any{
				"event_ref": map[string]any{"type": "string", "description": "The external event the moment belongs to, e.g. nightly-export (max 500 chars)."},
				"source":    map[string]any{"type": "string", "description": "Where the event's time is published: an https:// URL, comment:<uuid> or artifact:<uuid> written by someone else."},
				"why":       map[string]any{"type": "string", "description": "What happens at start_after and why not earlier, 10 to 500 chars."},
			}),
		),
	}
}

// applyStartAfter copies start_after, start_after_origin and
// start_after_evidence onto a task write body after the local shape check.
// isUpdate refuses an empty start_after: an unconditional clear is no longer
// sent, the agent clears with clear_start_after. On create an empty value
// still means "not set". The returned string is a refusal; empty means ok.
func applyStartAfter(request mcpsdk.CallToolRequest, body map[string]any, isUpdate bool) string {
	args := request.GetArguments()
	_, present := args["start_after"]
	raw := strings.TrimSpace(mcpsdk.ParseString(request, "start_after", ""))
	if present && raw == "" && isUpdate {
		return "clear_requires_compare: an empty start_after is not sent; clear the date with clear_start_after, which compares it with the schedule you read"
	}
	if raw != "" {
		if _, err := time.Parse(time.RFC3339, raw); err != nil {
			return fmt.Sprintf("invalid start_after format: %v (RFC3339 with Z or an offset, e.g. 2026-11-01T06:00:00Z)", err)
		}
		// Sent as given: the server keeps the offset on the receipt.
		body["start_after"] = raw
	}

	if origin := strings.TrimSpace(mcpsdk.ParseString(request, "start_after_origin", "")); origin != "" {
		if origin != startAfterOriginAgent {
			return fmt.Sprintf("origin_forbidden: start_after_origin can only be %s (got %q); the server derives the origin from the caller", startAfterOriginAgent, origin)
		}
		if raw == "" {
			return "start_after_origin is given without start_after"
		}
		body["start_after_origin"] = origin
	}

	evArg, evPresent := args["start_after_evidence"]
	if !evPresent || (evArg == nil && raw == "") {
		// A null next to no date is how some clients send an unset field.
		return ""
	}
	if raw == "" {
		return "start_after_evidence is given without start_after"
	}
	ev, refusal := startAfterEvidence(evArg)
	if refusal != "" {
		return refusal
	}
	body["start_after_evidence"] = ev
	return ""
}

// startAfterEvidence reads the evidence object (some clients send objects as
// a JSON string) and checks it by the time-wait rules.
func startAfterEvidence(v any) (map[string]any, string) {
	m, ok := v.(map[string]any)
	if s, isString := v.(string); isString {
		ok = json.Unmarshal([]byte(s), &m) == nil
	}
	if !ok {
		return nil, "start_after_evidence must be an object {event_ref, source, why}"
	}
	ev := map[string]any{}
	var missing []string
	for _, k := range []string{"event_ref", "source", "why"} {
		s, _ := m[k].(string)
		if s = strings.TrimSpace(s); s == "" {
			missing = append(missing, k)
			continue
		}
		if n := len([]rune(s)); n > 500 {
			return nil, fmt.Sprintf("start_after_evidence.%s is %d chars, max 500", k, n)
		}
		ev[k] = s
	}
	if len(missing) > 0 {
		return nil, fmt.Sprintf("missing_evidence: start_after_evidence needs %s — the external event, where its time is published and why the card waits until then", strings.Join(missing, ", "))
	}
	if why := ev["why"].(string); len([]rune(why)) < 10 {
		return nil, fmt.Sprintf("missing_evidence: start_after_evidence.why %q is shorter than 10 chars", why)
	}
	if src := ev["source"].(string); !isTimeWaitSource(src) {
		return nil, fmt.Sprintf("missing_evidence: start_after_evidence.source %q must be an https:// URL, comment:<uuid> or artifact:<uuid>; a card is not a source", src)
	}
	return ev, ""
}

// taskWriteError renders a refused task write. A contract refusal (one with an
// error_code) is shown like a typed-outcome refusal, so the agent sees the code
// it must act on; any other error keeps the tool's usual wording.
func taskWriteError(tool, prefix string, err error) (*mcpsdk.CallToolResult, error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		if code, _ := apiErr.Body["error_code"].(string); code != "" {
			return turnOutcomeError(tool, err)
		}
	}
	return errResult("%s: %v", prefix, err)
}

func (s *Server) registerClearStartAfter() {
	s.addTool(mcpsdk.NewTool("clear_start_after",
		mcpsdk.WithDescription("Clear a card's start_after only if it is still exactly the schedule you read (compare-and-clear). Pass the start_after, version, start_after_origin and start_after_receipt_id from get_task. A changed card is refused with 409 cas_conflict and its current values — nothing is written; re-read before deciding anything. Repeating a call with the same idempotency_key returns the first receipt (replayed=true). Does not touch human gates, dependencies, waits or due_date."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("The card (UUID or 6-12 hex short id).")),
		mcpsdk.WithString("expected_start_after", mcpsdk.Required(), mcpsdk.Description("The card's start_after as you read it, RFC3339.")),
		mcpsdk.WithNumber("expected_version", mcpsdk.Required(), mcpsdk.Description("The card's version as you read it.")),
		mcpsdk.WithString("expected_origin", mcpsdk.Description("The card's start_after_origin as you read it: agent_evidence, human_explicit or legacy_unknown. Required for an agent.")),
		mcpsdk.WithString("schedule_receipt_id", mcpsdk.Description("The card's start_after_receipt_id as you read it. Required for an agent unless the origin is legacy_unknown.")),
		mcpsdk.WithString("idempotency_key", mcpsdk.Description("Key of this clear, max 200 chars; reuse it when you repeat the same call. Generated when omitted.")),
		mcpsdk.WithString("reason", mcpsdk.Description("Why the date is no longer needed, max 500 chars; stored on the clear receipt.")),
	), s.tracked("clear_start_after", s.handleClearStartAfter))
}

func (s *Server) handleClearStartAfter(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	req, refusal := requiredStrings(request, "task_id", "expected_start_after")
	if refusal != "" {
		return errResult("%s", refusal)
	}
	if !hasArgument(request, "expected_version") {
		return errResult("expected_version is required: the card's version as you read it")
	}
	if _, err := time.Parse(time.RFC3339Nano, req[1]); err != nil {
		return errResult("expected_start_after %q is not RFC3339: %v", req[1], err)
	}
	body := map[string]any{
		"expected_start_after": req[1],
		"expected_version":     mcpsdk.ParseInt64(request, "expected_version", 0),
	}
	if origin := strings.TrimSpace(mcpsdk.ParseString(request, "expected_origin", "")); origin != "" {
		body["expected_origin"] = origin
	}
	if rid := strings.TrimSpace(mcpsdk.ParseString(request, "schedule_receipt_id", "")); rid != "" {
		if _, err := uuid.Parse(rid); err != nil {
			return errResult("schedule_receipt_id %q is not a UUID", rid)
		}
		body["schedule_receipt_id"] = rid
	}
	key := strings.TrimSpace(mcpsdk.ParseString(request, "idempotency_key", ""))
	if key == "" {
		key = "mcp-" + uuid.NewString()
	}
	if len(key) > 200 {
		return errResult("idempotency_key is %d chars, max 200", len(key))
	}
	body["idempotency_key"] = key
	if reason := strings.TrimSpace(mcpsdk.ParseString(request, "reason", "")); reason != "" {
		if n := len([]rune(reason)); n > 500 {
			return errResult("reason is %d chars, max 500", n)
		}
		body["reason"] = reason
	}

	result, err := s.getRESTClient(ctx).turnCommand(ctx,
		"/api/v1/tasks/"+url.PathEscape(req[0])+"/start-after/compare-clear", body)
	if err != nil {
		return clearStartAfterError(err)
	}
	return jsonResult(result)
}

// clearStartAfterError adds the card's current schedule to a cas_conflict, so
// the agent sees what changed without parsing prose.
func clearStartAfterError(err error) (*mcpsdk.CallToolResult, error) {
	res, _ := turnOutcomeError("clear_start_after", err)
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return res, nil
	}
	current := map[string]any{}
	for _, k := range []string{"current_version", "current_start_after", "current_origin", "current_receipt"} {
		if v, ok := apiErr.Body[k]; ok {
			current[k] = v
		}
	}
	if len(current) == 0 {
		return res, nil
	}
	text := ""
	if len(res.Content) > 0 {
		if tc, ok := res.Content[0].(mcpsdk.TextContent); ok {
			text = tc.Text
		}
	}
	b, _ := json.Marshal(current)
	return errResult("%s current=%s", text, b)
}
