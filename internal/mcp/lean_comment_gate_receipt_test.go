package mcp

import "testing"

// The lean add_comment response must keep gate_receipt: it is the only place a
// withdrawal-shaped comment learns cleared / no_op / not_owner / stale.
func TestLeanCommentResult_KeepsGateReceiptAndDropsBody(t *testing.T) {
	in := map[string]any{
		"id": "c1", "body": "Вопрос снят — не нужно.",
		"gate_receipt": map[string]any{"outcome": "not_owner", "next_action": "x"},
	}
	out := leanCommentResult(in)
	if _, ok := out["gate_receipt"]; !ok {
		t.Fatalf("gate_receipt dropped: %v", out)
	}
	if _, ok := out["body"]; ok {
		t.Fatalf("body must stay out of the lean echo: %v", out)
	}
}

func TestLeanCommentResult_GateReceiptAllOutcomesAndAbsence(t *testing.T) {
	for _, outcome := range []string{"cleared", "no_op", "not_owner", "stale"} {
		in := map[string]any{"id": "c1", "gate_receipt": map[string]any{"outcome": outcome, "next_action": "n"}}
		got, ok := leanCommentResult(in)["gate_receipt"].(map[string]any)
		if !ok || got["outcome"] != outcome || got["next_action"] != "n" {
			t.Fatalf("outcome %q not preserved intact: %v", outcome, got)
		}
	}
	// a comment with no receipt must not grow one
	if _, ok := leanCommentResult(map[string]any{"id": "c2"})["gate_receipt"]; ok {
		t.Fatal("gate_receipt appeared on a comment that had none")
	}
}
