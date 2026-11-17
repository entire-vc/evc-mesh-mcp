package mcp

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
)

// reassignMesh models the server with MESH_REASSIGN_RELEASES_CHECKOUT on: one
// live lock (token); a move that changes the assignee releases it in the same
// write; any later write fenced with the released token is refused with 409
// checkout_precondition_failed, as mesh-api does.
type reassignMesh struct {
	mu        sync.Mutex
	token     string // the live lock; "" once released
	assignee  string
	comments  []string
	keys      []string
	moves     int
	refuseMov string // error_code of a 409 for /move, "" = accept
	order     []string
}

func (m *reassignMesh) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	m.mu.Lock()
	defer m.mu.Unlock()
	if r.Method == http.MethodGet {
		if strings.HasSuffix(r.URL.Path, "/statuses") {
			_, _ = w.Write([]byte(`[{"id":"` + uuid.Nil.String() + `","slug":"todo","name":"To Do"}]`))
			return
		}
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": "t", "assignee_id": m.assignee, "input_revision": 1})
		return
	}
	if tok := r.Header.Get(fenceTokenHeader); tok != "" && tok != m.token {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": 409, "error_code": fenceLockLost,
			"message": "Conflict: the lock of this turn is gone"})
		return
	}
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	switch {
	case strings.HasSuffix(r.URL.Path, "/comments"):
		key := r.Header.Get(idempotencyKeyHeader)
		for _, k := range m.keys {
			if key != "" && k == key { // replay: nothing new is stored
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"replayed"}`))
				return
			}
		}
		m.keys = append(m.keys, key)
		m.comments = append(m.comments, body["body"].(string))
		m.order = append(m.order, "comment")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"` + uuid.New().String() + `"}`))
	case strings.HasSuffix(r.URL.Path, "/move"):
		if m.refuseMov != "" {
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 409, "error_code": m.refuseMov, "message": "move refused"})
			return
		}
		m.moves++
		m.order = append(m.order, "move")
		if a, _ := body["assignee_id"].(string); a != "" && a != m.assignee {
			m.assignee = a
			m.token = "" // task.checkout_released_auto {reason: reassignment}
		}
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	default:
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	}
}

func newReassignEnv(t *testing.T) (*fenceEnv, *reassignMesh) {
	t.Helper()
	e := newFenceEnv(t)
	token := uuid.New().String()
	e.publishToken(t, token, 1)
	m := &reassignMesh{token: token, assignee: e.agentID.String()}
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	e.server.restClient = NewRESTClient(srv.URL, "agk_test")
	return e, m
}

// A reviewer returns its verdict with move_task(todo, assignee_id=<author>,
// comment=<verdict>) under its live fenced lock while the reassignment release
// is on: the verdict is stored, the card is reassigned, the lock is released,
// and nothing in the result is a refusal.
func TestMoveTask_ReassignWithComment_VerdictStoredUnderLiveLock(t *testing.T) {
	e, m := newReassignEnv(t)
	author := uuid.New().String()
	res, err := e.server.handleMoveTask(e.ctx, call(map[string]any{
		"task_id": e.taskID, "status_slug": "todo", "assignee_id": author, "comment": "Verify: DO-NOT-SHIP — finding 1",
	}))
	mustOK(t, res, err)
	if text := resultText(t, res); strings.Contains(text, "refused") || strings.Contains(text, fenceLockLost) {
		t.Fatalf("result carries a refusal: %s", text)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.comments) != 1 || m.comments[0] != "Verify: DO-NOT-SHIP — finding 1" {
		t.Fatalf("verdict comments stored = %q, want the one verdict", m.comments)
	}
	if m.assignee != author || m.moves != 1 {
		t.Fatalf("assignee=%s moves=%d, want reassigned to the author once", m.assignee, m.moves)
	}
	if m.token != "" {
		t.Fatalf("lock still held after the reassignment")
	}
	if strings.Join(m.order, ",") != "comment,move" {
		t.Fatalf("write order = %v, want the comment before the move", m.order)
	}
}

// The lock is already gone when the turn calls move_task: the comment is
// refused, the agent is told to stop, and the card is not moved either.
func TestMoveTask_CommentRefused_NothingMoved(t *testing.T) {
	e, m := newReassignEnv(t)
	m.token = "someone-else"
	res, err := e.server.handleMoveTask(e.ctx, call(map[string]any{
		"task_id": e.taskID, "status_slug": "todo", "assignee_id": uuid.New().String(), "comment": "late verdict",
	}))
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("want a tool error, got %v %v", res, err)
	}
	if text := resultText(t, res); !strings.Contains(text, fenceLockLostHint) {
		t.Errorf("result %q does not tell the agent to stop", text)
	}
	if m.moves != 0 || len(m.comments) != 0 {
		t.Fatalf("moves=%d comments=%d after a refused comment, want 0/0", m.moves, len(m.comments))
	}
}

// The comment landed but the move was refused: the result says the comment is
// stored, and retrying the same call does not post it twice.
func TestMoveTask_MoveRefusedAfterComment_RetryDoesNotDuplicate(t *testing.T) {
	e, m := newReassignEnv(t)
	m.refuseMov = "conflict"
	args := map[string]any{"task_id": e.taskID, "status_slug": "todo", "assignee_id": uuid.New().String(), "comment": "verdict"}
	res, err := e.server.handleMoveTask(e.ctx, call(args))
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("want a tool error for the refused move, got %v %v", res, err)
	}
	if text := resultText(t, res); !strings.Contains(text, "comment stored") {
		t.Errorf("result %q does not say the comment is stored", text)
	}
	m.mu.Lock()
	m.refuseMov = ""
	m.mu.Unlock()
	res, err = e.server.handleMoveTask(e.ctx, call(args))
	mustOK(t, res, err)
	if len(m.comments) != 1 || m.moves != 1 {
		t.Fatalf("comments=%d moves=%d after the retry, want 1/1", len(m.comments), m.moves)
	}
}
