package mcp

import (
	"strings"
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// Task #4545660b. These cases never reach the network: each one must be refused
// LOCALLY, with a message that says what to write next.
//
// Why local refusal matters even though the server also returns 422: the server can only
// say WHICH field was wrong. It cannot say "state what you will do if nobody answers",
// and an agent that only learns "recommended_default: required" tends to retry the same
// call verbatim or conclude it is not allowed to raise a gate at all. That misreading is
// the documented failure mode this whole card is about.
func TestSetHumanGate_RefusesIncompleteAskBeforeAnyRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		args     map[string]any
		wantWord string
	}{
		{
			name:     "no task_id",
			args:     map[string]any{"reason": "r", "recommended_default": "d"},
			wantWord: "task_id",
		},
		{
			name:     "no reason",
			args:     map[string]any{"task_id": "t", "recommended_default": "d"},
			wantWord: "reason",
		},
		{
			name:     "no recommended_default — the gate could never time out",
			args:     map[string]any{"task_id": "t", "reason": "r"},
			wantWord: "recommended_default",
		},
		{
			name:     "whitespace-only default is not a default",
			args:     map[string]any{"task_id": "t", "reason": "r", "recommended_default": "   "},
			wantWord: "recommended_default",
		},
		{
			name:     "class outside the enum",
			args:     map[string]any{"task_id": "t", "reason": "r", "recommended_default": "d", "class": "medium"},
			wantWord: "class",
		},
		{
			// A deadline the caller believes they set, which silently became "no
			// deadline", is exactly the silent-miss shape this card removes.
			name:     "unparseable deadline is refused, not dropped",
			args:     map[string]any{"task_id": "t", "reason": "r", "recommended_default": "d", "deadline": "к пятнице"},
			wantWord: "RFC3339",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = tc.args

			args, refusal := parseSetHumanGateArgs(req)
			if refusal == "" {
				t.Fatalf("expected a refusal, but the args were accepted: %+v", args)
			}
			if args != nil {
				t.Errorf("a refused call must not yield parsed args")
			}
			if !strings.Contains(strings.ToLower(refusal), strings.ToLower(tc.wantWord)) {
				t.Errorf("refusal must name what to fix (%q); got: %s", tc.wantWord, refusal)
			}
		})
	}
}

// TestSetHumanGate_CompleteAskPassesLocalValidation is the POSITIVE CONTROL. Without it,
// a handler that refused every input would satisfy every case above.
func TestSetHumanGate_CompleteAskPassesLocalValidation(t *testing.T) {
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"task_id":             "t",
		"reason":              "мёржим сейчас или ждём?",
		"recommended_default": "жду ответа до дедлайна",
		"class":               "soft",
		"deadline":            "2026-09-09T12:00:00Z",
	}

	args, refusal := parseSetHumanGateArgs(req)
	if refusal != "" {
		t.Fatalf("a complete ask must be accepted; got refusal: %s", refusal)
	}
	if args.Reason != "мёржим сейчас или ждём?" {
		t.Errorf("reason not carried through: %q", args.Reason)
	}
	if args.RecommendedDefault != "жду ответа до дедлайна" {
		t.Errorf("recommended_default not carried through: %q", args.RecommendedDefault)
	}
	if args.Class != "soft" {
		t.Errorf("class not carried through: %q", args.Class)
	}
	if args.Deadline == nil || args.Deadline.Year() != 2026 || args.Deadline.Day() != 9 {
		t.Errorf("deadline not parsed: %v", args.Deadline)
	}
}

// TestSetHumanGate_OmittedOptionalsAreEmptyNotGuessed: an omitted class must arrive
// empty so the SERVER applies its fail-closed default ("hard"), and an omitted deadline
// must be nil. Guessing either here would put a value on the fields the timeout sweep
// acts on that nobody actually asked for.
func TestSetHumanGate_OmittedOptionalsAreEmptyNotGuessed(t *testing.T) {
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"task_id":             "t",
		"reason":              "r",
		"recommended_default": "d",
	}

	args, refusal := parseSetHumanGateArgs(req)
	if refusal != "" {
		t.Fatalf("unexpected refusal: %s", refusal)
	}
	if args.Class != "" {
		t.Errorf("omitted class must stay empty so the server defaults it; got %q", args.Class)
	}
	if args.Deadline != nil {
		t.Errorf("omitted deadline must be nil, never a guessed time; got %v", args.Deadline)
	}
}
