package mcp

import "testing"

// These mirror evc-mesh's defaultMinImportance and the score it mints for
// kind:session-checkpoint (internal/service/memory_service.go). They are
// duplicated here only because the two repos cannot import each other; the
// comment at the recall handler explains why the real fix is for this client to
// send nothing and let the server decide.
//
// The numbers are not the point. The point is that this client's fallback must
// never be STRICTER than the server's: ParseFloat64 fills the field in, so a
// stricter value here does not "also apply", it REPLACES the server's default and
// silently wins. That is exactly how 0.4 here defeated the server-side fix for
// the whole fleet (#a9752575).
const (
	serverDefaultMinImportance   = 0.3
	serverSessionCheckpointScore = 0.3
)

func TestRecallDefaultMinImportanceDoesNotOutrankServer(t *testing.T) {
	if recallDefaultMinImportance > serverDefaultMinImportance {
		t.Errorf("MCP fallback %.2f is stricter than the server default %.2f; this "+
			"client always sends the field, so it overrides the server rather than "+
			"deferring to it", recallDefaultMinImportance, serverDefaultMinImportance)
	}
	if recallDefaultMinImportance > serverSessionCheckpointScore {
		t.Errorf("a plain recall() would not return kind:session-checkpoint "+
			"(scored %.2f) under fallback %.2f — that class is written specifically "+
			"for the next session to read", serverSessionCheckpointScore,
			recallDefaultMinImportance)
	}
}

// multi-session deliberately reaches BELOW the checkpoint score (it is the
// recovery profile); factual deliberately sits above it. Pinned so a future
// default change does not quietly flatten the two into one behaviour.
func TestRecallProfilePresetsBracketTheDefault(t *testing.T) {
	if multi := GetProfileParams(ProfileMultiSession); multi.MinImportance >= serverSessionCheckpointScore {
		t.Errorf("multi-session MinImportance %.2f must reach below checkpoints (%.2f)",
			multi.MinImportance, serverSessionCheckpointScore)
	}
	if factual := GetProfileParams(ProfileFactual); factual.MinImportance <= serverSessionCheckpointScore {
		t.Errorf("factual MinImportance %.2f must exclude ephemeral hand-offs (%.2f)",
			factual.MinImportance, serverSessionCheckpointScore)
	}
}
