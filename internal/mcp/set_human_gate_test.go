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
// predicateArgs is the four-question block every set_human_gate call must now carry
// (task #5d3dc714). Merged into the older cases so they keep testing what they were
// written for instead of silently becoming predicate tests.
func predicateArgs(extra map[string]any) map[string]any {
	m := map[string]any{
		"credential_exists":     true,
		"credential_reason":     "token is in keys.env",
		"reversible":            false,
		"reversible_reason":     "an outbound payment cannot be un-sent",
		"blocked_by_other_task": false,
		"blocked_reason":        "no other card owns this",
		"customer_visible_now":  false,
		"customer_reason":       "gateway inactive, nobody can be charged",
	}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func TestSetHumanGate_RefusesIncompleteAskBeforeAnyRoundTrip(t *testing.T) {
	cases := []struct {
		name     string
		args     map[string]any
		wantWord string
	}{
		{
			name:     "no task_id",
			args:     predicateArgs(map[string]any{"reason": "r", "recommended_default": "d"}),
			wantWord: "task_id",
		},
		{
			name:     "no reason",
			args:     predicateArgs(map[string]any{"task_id": "t", "recommended_default": "d"}),
			wantWord: "reason",
		},
		{
			name:     "no recommended_default — the gate could never time out",
			args:     predicateArgs(map[string]any{"task_id": "t", "reason": "r"}),
			wantWord: "recommended_default",
		},
		{
			name:     "whitespace-only default is not a default",
			args:     predicateArgs(map[string]any{"task_id": "t", "reason": "r", "recommended_default": "   "}),
			wantWord: "recommended_default",
		},
		{
			name:     "class outside the enum",
			args:     predicateArgs(map[string]any{"task_id": "t", "reason": "r", "recommended_default": "d", "class": "medium"}),
			wantWord: "class",
		},
		{
			// A deadline the caller believes they set, which silently became "no
			// deadline", is exactly the silent-miss shape this card removes.
			name:     "unparseable deadline is refused, not dropped",
			args:     predicateArgs(map[string]any{"task_id": "t", "reason": "r", "recommended_default": "d", "deadline": "к пятнице"}),
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
	req.Params.Arguments = predicateArgs(map[string]any{
		"task_id":             "t",
		"reason":              "мёржим сейчас или ждём?",
		"recommended_default": "жду ответа до дедлайна",
		"class":               "soft",
		"deadline":            "2026-09-09T12:00:00Z",
	})

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
	req.Params.Arguments = predicateArgs(map[string]any{
		"task_id":             "t",
		"reason":              "r",
		"recommended_default": "d",
	})

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

// Task #5d3dc714. Each of the four answers needs one line of justification, refused
// LOCALLY so the message can say what to write — the server's 422 can only name the
// field.
func TestSetHumanGate_RefusesPredicateAnswerWithNoReason(t *testing.T) {
	for _, missing := range []string{
		"credential_reason", "reversible_reason", "blocked_reason", "customer_reason",
	} {
		t.Run(missing, func(t *testing.T) {
			args := predicateArgs(map[string]any{
				"task_id": "t", "reason": "r", "recommended_default": "d",
			})
			args[missing] = "   " // whitespace is absent, not present

			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = args

			parsed, refusal := parseSetHumanGateArgs(req)
			if refusal == "" {
				t.Fatalf("expected a refusal for blank %s, got args: %+v", missing, parsed)
			}
			if !strings.Contains(refusal, missing) {
				t.Errorf("refusal must name %s; got: %s", missing, refusal)
			}
		})
	}
}

// The predicate must reach the server verbatim. The client deliberately does NOT decide
// it — a second copy of the rule here would drift from the server's, and the server's is
// the one that governs the write.
func TestSetHumanGate_ForwardsPredicateWithoutJudgingIt(t *testing.T) {
	req := mcpsdk.CallToolRequest{}
	// Answers that the SERVER will refuse (reversible + safe + self-served). The client
	// must still forward them rather than pre-empting the decision.
	req.Params.Arguments = predicateArgs(map[string]any{
		"task_id": "t", "reason": "r", "recommended_default": "d",
		"reversible": true, "reversible_reason": "revertible by a migration down",
	})

	args, refusal := parseSetHumanGateArgs(req)
	if refusal != "" {
		t.Fatalf("client must not pre-judge the predicate; got refusal: %s", refusal)
	}
	if args.Predicate["reversible"] != true {
		t.Errorf("reversible not forwarded: %+v", args.Predicate)
	}
	if args.Predicate["customer_reason"] != "gateway inactive, nobody can be charged" {
		t.Errorf("customer_reason not forwarded: %+v", args.Predicate)
	}
	if len(args.Predicate) != 8 {
		t.Errorf("all four answers plus reasons must be forwarded, got %d keys: %+v",
			len(args.Predicate), args.Predicate)
	}
}
