package mcp

import "testing"

// The FOURTH way a preset overruled the caller — same rule as limit
// (recall_limit_precedence_test.go) and order_by, one parameter later.
//
// A preset is a default: it fills in what the caller left unsaid, it does not
// overrule what the caller said. min_importance was the last recall parameter
// still breaking that. `factual` carries MinImportance 0.5, so a caller who
// explicitly asked for 0.3 silently got 0.5 — and every kind:session-checkpoint
// (scored 0.30) vanished with nothing in the response to say why.
//
// That is what made the spawn prompts' `min_importance=0.3` only partly work: on
// any query the classifier read as factual, the very hand-off memories the prompt
// was reaching for were filtered out anyway.
//
// Measured through the real stdio binary 2026-09-06, one query, three arms:
//
//	explicit min_importance=0.3   → 0 checkpoints   (preset won — the defect)
//	recall_profile=multi_session  → 1 checkpoint
//	plain-language query, default → 3 checkpoints
//
// These call resolveProfileMinImportance — the function the handler itself runs.
// An inline copy of the rule here would stay green while the handler regressed,
// which is precisely how this family of precedence bugs survives.
func TestProfileMinImportance_DoesNotOverrideExplicitValue(t *testing.T) {
	pp := GetProfileParams(ProfileFactual)
	if pp.MinImportance <= 0 {
		t.Fatalf("precondition: factual MinImportance = %v, want > 0 so the preset could win",
			pp.MinImportance)
	}
	if pp.MinImportance <= recallDefaultMinImportance {
		t.Fatalf("precondition: factual (%v) must be stricter than the default (%v), "+
			"otherwise this test cannot tell an override from a no-op",
			pp.MinImportance, recallDefaultMinImportance)
	}

	if got := resolveProfileMinImportance(pp.MinImportance, 0.3, true); got != 0.3 {
		t.Errorf("explicit min_importance resolved to %v, want 0.3 — the preset overruled "+
			"the caller, so session-checkpoints at 0.30 disappear from the result", got)
	}

	if got := resolveProfileMinImportance(pp.MinImportance, recallDefaultMinImportance, false); got != pp.MinImportance {
		t.Errorf("with nothing supplied the preset must still apply: got %v, want %v",
			got, pp.MinImportance)
	}
}

// The default profile carries no MinImportance, so the client fallback must
// survive it — otherwise a plain recall() would silently filter at 0.
func TestDefaultProfileLeavesTheClientFallbackIntact(t *testing.T) {
	pp := GetProfileParams(RecallProfile("default"))
	got := resolveProfileMinImportance(pp.MinImportance, recallDefaultMinImportance, false)
	if got != recallDefaultMinImportance {
		t.Errorf("default profile changed the fallback to %v, want %v", got, recallDefaultMinImportance)
	}
}
