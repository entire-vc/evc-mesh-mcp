package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// refusalBody is what the API answers for an author whose delivery proof is incomplete.
func refusalBody(linkID string) map[string]any {
	return map[string]any{
		"code": 403, "error_code": "forbidden", "message": "workflow_transition_blocked",
		"details": `actor type "agent" not permitted for transition "review"→"done"; author_if_merged_shipped evidence missing (MR 276: missing_ship_sha). Restore the existing valid receipt`,
		"context": map[string]any{
			"gate":        "author_if_merged_shipped",
			"reasons":     []any{"missing_ship_sha"},
			"links":       []any{map[string]any{"link_id": linkID, "external_id": "276", "reasons": []any{"missing_ship_sha"}}},
			"next_action": "Restore the existing valid receipt; do not ask a human",
		},
	}
}

func assertRefusalText(t *testing.T, msg, linkID string) {
	t.Helper()
	for _, want := range []string{"workflow_transition_blocked", "gate=author_if_merged_shipped", "reasons=missing_ship_sha",
		"link_id=" + linkID, "mr=276", "next_action: Restore the existing valid receipt"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
}

func TestMoveTask_GateRefusalShowsEvidenceGaps(t *testing.T) {
	taskID, todoID, doneID, linkID := uuid.New().String(), uuid.New().String(), uuid.New().String(), uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+taskID+"/statuses":
			_ = json.NewEncoder(w).Encode(statusFixture(todoID, doneID))
		case strings.HasSuffix(r.URL.Path, "/move"):
			w.WriteHeader(http.StatusForbidden)
			_ = json.NewEncoder(w).Encode(refusalBody(linkID))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	ctx := withTestSession(context.Background(), server, uuid.New())

	result, err := server.handleMoveTask(ctx, buildMoveTaskRequest(taskID, "done"))
	if err != nil || !result.IsError {
		t.Fatalf("want an error result, got err=%v result=%v", err, result)
	}
	assertRefusalText(t, resultText(t, result), linkID)
}

func TestFinishTask_GateRefusalShowsEvidenceGaps(t *testing.T) {
	taskID, linkID := uuid.New().String(), uuid.New().String()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(refusalBody(linkID))
	}))
	defer srv.Close()
	server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
	res := callOutcome(server.handleFinishTask, map[string]any{"task_id": taskID, "result_kind": resultKinds[0], "evidence": "proof"})
	if !res.IsError {
		t.Fatal("want an error result")
	}
	assertRefusalText(t, resultText(t, res), linkID)
}

// Other refusals keep their text: no gate context, nothing appended.
func TestGateRefusalText_OtherErrorsUnchanged(t *testing.T) {
	for _, e := range []error{
		&APIError{StatusCode: 403, Message: "Forbidden", Body: map[string]any{"message": "Forbidden"}},
		&APIError{StatusCode: 403, Message: "x", Body: nil},
		context.Canceled,
		// same context.gate on a non-403, and on a 403 with another code
		&APIError{StatusCode: 409, Message: "workflow_transition_blocked", Body: refusalBody("l")},
		&APIError{StatusCode: 403, Message: "something_else", Body: refusalBody("l")},
	} {
		if got := gateRefusalText(e); got != "" {
			t.Errorf("gateRefusalText(%v) = %q, want empty", e, got)
		}
	}
}
