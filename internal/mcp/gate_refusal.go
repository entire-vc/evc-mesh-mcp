package mcp

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// gateRefusalText renders the machine facts of a workflow-gate refusal (403
// workflow_transition_blocked carrying context.gate) as one readable block:
// the gate, every missing-evidence reason, the offending MR link ids and the
// safe next action. "" when err is not such a refusal, so every other error
// keeps its text byte-for-byte.
//
// The server's Message is only the stable code; without this the caller saw
// "Forbidden: workflow_transition_blocked" and concluded a human must close.
func gateRefusalText(err error) string {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return ""
	}
	// Only the workflow gate's own refusal: a 403 whose code is
	// workflow_transition_blocked. Another failure reusing context.gate keeps its text.
	if apiErr.StatusCode != http.StatusForbidden || apiErr.Message != "workflow_transition_blocked" {
		return ""
	}
	ctx, _ := apiErr.Body["context"].(map[string]any)
	gate, _ := ctx["gate"].(string)
	if gate == "" {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "gate=%s", gate)
	if reasons := stringList(ctx["reasons"]); len(reasons) > 0 {
		fmt.Fprintf(&b, "; reasons=%s", strings.Join(reasons, ","))
	}
	links, _ := ctx["links"].([]any)
	for _, raw := range links {
		l, _ := raw.(map[string]any)
		rs := stringList(l["reasons"])
		if len(rs) == 0 {
			continue
		}
		id, _ := l["link_id"].(string)
		ext, _ := l["external_id"].(string)
		fmt.Fprintf(&b, "; link_id=%s mr=%s missing=%s", id, ext, strings.Join(rs, ","))
	}
	if next, _ := ctx["next_action"].(string); next != "" {
		fmt.Fprintf(&b, "; next_action: %s", next)
	}
	return b.String()
}

func stringList(v any) []string {
	items, _ := v.([]any)
	out := make([]string, 0, len(items))
	for _, it := range items {
		if s, ok := it.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
