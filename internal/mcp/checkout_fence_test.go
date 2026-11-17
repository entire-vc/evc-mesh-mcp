package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// fenceMesh records the headers of every write and answers the reads the
// write tools make. refuse, when set, answers every POST with that 409.
type fenceMesh struct {
	mu       sync.Mutex
	writes   []fenceSeen
	revision int64
	refuse   string // error_code of a 409 for every POST, "" = accept
	refCtx   map[string]any
}

type fenceSeen struct {
	path   string
	header http.Header
}

func (f *fenceMesh) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = io.ReadAll(r.Body)
	w.Header().Set("Content-Type", "application/json")
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodGet {
		switch {
		case strings.HasSuffix(r.URL.Path, "/statuses"):
			_, _ = w.Write([]byte(`[{"id":"` + uuid.Nil.String() + `","slug":"review","name":"Review"}]`))
		default:
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": "t", "input_revision": f.revision})
		}
		return
	}
	f.writes = append(f.writes, fenceSeen{path: r.URL.Path, header: r.Header.Clone()})
	if f.refuse != "" {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 409, "error_code": f.refuse, "message": "nothing was written", "context": f.refCtx,
		})
		return
	}
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write([]byte(`{"id":"` + uuid.New().String() + `","body":"x","released":true}`))
}

func (f *fenceMesh) last(t *testing.T) fenceSeen {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.writes) == 0 {
		t.Fatal("no write reached the server")
	}
	return f.writes[len(f.writes)-1]
}

type fenceEnv struct {
	server  *Server
	ctx     context.Context
	mesh    *fenceMesh
	dir     string // <root>/<agent_id>
	agentID uuid.UUID
	taskID  string
}

func newFenceEnv(t *testing.T) *fenceEnv {
	t.Helper()
	idemTestEnv(t)
	root := t.TempDir()
	t.Setenv(checkoutFenceDirEnv, root)
	oldRevs := defaultInputRevisions
	defaultInputRevisions = &inputRevisions{revs: map[string]int64{}}
	t.Cleanup(func() { defaultInputRevisions = oldRevs })

	mesh := &fenceMesh{revision: 1}
	srv := httptest.NewServer(mesh)
	t.Cleanup(srv.Close)
	agentID := uuid.New()
	server := &Server{restClient: NewRESTClient(srv.URL, "agk_test"), tracker: NewSessionTracker()}
	ctx := ContextWithSession(context.Background(), &AgentSession{AgentID: agentID, WorkspaceID: uuid.New()})
	return &fenceEnv{server: server, ctx: ctx, mesh: mesh, dir: filepath.Join(root, agentID.String()),
		agentID: agentID, taskID: uuid.New().String()}
}

func (e *fenceEnv) publish(t *testing.T, data map[string]any) {
	t.Helper()
	if err := os.MkdirAll(e.dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(data)
	if err := os.WriteFile(filepath.Join(e.dir, e.taskID+".json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
}

func (e *fenceEnv) publishToken(t *testing.T, token string, rev int64) {
	e.publish(t, map[string]any{"version": 1, "task_id": e.taskID, "attempt_id": "att-1", "turn": 4,
		"checkout_token": token, "session_id": uuid.New().String(), "generation": 7, "input_revision": rev})
}

func (e *fenceEnv) journal(t *testing.T) []map[string]any {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(e.dir, e.taskID+".refusals.jsonl"))
	if err != nil {
		t.Fatalf("refusal journal: %v", err)
	}
	var rows []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(line), &m); err != nil {
			t.Fatalf("journal line %q: %v", line, err)
		}
		rows = append(rows, m)
	}
	return rows
}

func call(args map[string]any) mcpsdk.CallToolRequest {
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

// Every write tool of the card, in turn: add_comment, upload_artifact,
// move_task, finish_task. Returns the tool result of each.
func (e *fenceEnv) writeAll(t *testing.T) map[string]fenceSeen {
	t.Helper()
	out := map[string]fenceSeen{}
	_, _ = e.server.handleAddComment(e.ctx, call(map[string]any{"task_id": e.taskID, "body": "hello"}))
	out["add_comment"] = e.mesh.last(t)
	_, _ = e.server.handleUploadArtifact(e.ctx, call(map[string]any{"task_id": e.taskID, "name": "r.md", "content": "# r\n", "artifact_type": "file"}))
	out["upload_artifact"] = e.mesh.last(t)
	_, _ = e.server.handleMoveTask(e.ctx, call(map[string]any{"task_id": e.taskID, "status_slug": "review"}))
	out["move_task"] = e.mesh.last(t)
	_, _ = e.server.handleFinishTask(e.ctx, call(map[string]any{"task_id": e.taskID, "result_kind": "change", "evidence": "ok"}))
	out["finish_task"] = e.mesh.last(t)
	for tool, seen := range out {
		want := map[string]string{"add_comment": "/comments", "upload_artifact": "/artifacts", "move_task": "/move", "finish_task": "/finish"}[tool]
		if !strings.HasSuffix(seen.path, want) {
			t.Fatalf("%s: last write went to %s, want *%s", tool, seen.path, want)
		}
	}
	return out
}

var fenceHeaders = []string{fenceTokenHeader, fenceSessionHeader, fenceGenerationHeader, fenceRevisionHeader}

func TestFence_NoFile_NoHeaders(t *testing.T) {
	e := newFenceEnv(t)
	for tool, seen := range e.writeAll(t) {
		for _, h := range fenceHeaders {
			if v := seen.header.Get(h); v != "" {
				t.Errorf("%s without a fence file sent %s=%q", tool, h, v)
			}
		}
	}
}

func TestFence_FileToken_OnEveryWrite_RevisionOnlyOnFinish(t *testing.T) {
	e := newFenceEnv(t)
	token := uuid.New().String()
	e.publishToken(t, token, 3)
	for tool, seen := range e.writeAll(t) {
		if got := seen.header.Get(fenceTokenHeader); got != token {
			t.Errorf("%s: %s=%q, want the file's token", tool, fenceTokenHeader, got)
		}
		if seen.header.Get(fenceSessionHeader) != "" || seen.header.Get(fenceGenerationHeader) != "" {
			t.Errorf("%s: token present, but session/generation sent too: %v", tool, seen.header)
		}
		rev := seen.header.Get(fenceRevisionHeader)
		if tool == "finish_task" && rev != "3" {
			t.Errorf("finish_task: %s=%q, want 3", fenceRevisionHeader, rev)
		}
		if tool != "finish_task" && rev != "" {
			t.Errorf("%s sent %s=%q; the revision goes only on finish", tool, fenceRevisionHeader, rev)
		}
	}
}

func TestFence_NoToken_SessionAndGeneration(t *testing.T) {
	e := newFenceEnv(t)
	session := uuid.New().String()
	e.publish(t, map[string]any{"version": 1, "task_id": e.taskID, "attempt_id": "att-1", "turn": 1,
		"checkout_token": "", "session_id": session, "generation": 7, "input_revision": 2})
	_, _ = e.server.handleAddComment(e.ctx, call(map[string]any{"task_id": e.taskID, "body": "hello"}))
	seen := e.mesh.last(t)
	if seen.header.Get(fenceTokenHeader) != "" {
		t.Errorf("token header sent with an empty token")
	}
	if seen.header.Get(fenceSessionHeader) != session || seen.header.Get(fenceGenerationHeader) != "7" {
		t.Errorf("session/generation = %q/%q, want %s/7", seen.header.Get(fenceSessionHeader), seen.header.Get(fenceGenerationHeader), session)
	}
}

// The file is read on every write: a new turn's lock is used at once, a
// removed file stops the headers.
func TestFence_FileReadAgainOnEveryWrite(t *testing.T) {
	e := newFenceEnv(t)
	first, second := uuid.New().String(), uuid.New().String()
	e.publishToken(t, first, 1)
	_, _ = e.server.handleAddComment(e.ctx, call(map[string]any{"task_id": e.taskID, "body": "a"}))
	if got := e.mesh.last(t).header.Get(fenceTokenHeader); got != first {
		t.Fatalf("first write token %q, want %q", got, first)
	}
	e.publishToken(t, second, 1)
	_, _ = e.server.handleAddComment(e.ctx, call(map[string]any{"task_id": e.taskID, "body": "b"}))
	if got := e.mesh.last(t).header.Get(fenceTokenHeader); got != second {
		t.Fatalf("after the Runner rewrote the file the token is %q, want %q", got, second)
	}
	if err := os.Remove(filepath.Join(e.dir, e.taskID+".json")); err != nil {
		t.Fatal(err)
	}
	_, _ = e.server.handleAddComment(e.ctx, call(map[string]any{"task_id": e.taskID, "body": "c"}))
	if got := e.mesh.last(t).header.Get(fenceTokenHeader); got != "" {
		t.Fatalf("file removed, token still sent: %q", got)
	}
}

// Another agent's file, or another card's, never fences this write; a short
// id finds this agent's file.
func TestFence_FileScope(t *testing.T) {
	e := newFenceEnv(t)
	token := uuid.New().String()
	e.publishToken(t, token, 1)

	other := ContextWithSession(context.Background(), &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()})
	_, _ = e.server.handleAddComment(other, call(map[string]any{"task_id": e.taskID, "body": "x"}))
	if got := e.mesh.last(t).header.Get(fenceTokenHeader); got != "" {
		t.Errorf("another agent's write sent this agent's token")
	}
	_, _ = e.server.handleAddComment(e.ctx, call(map[string]any{"task_id": uuid.New().String(), "body": "x"}))
	if got := e.mesh.last(t).header.Get(fenceTokenHeader); got != "" {
		t.Errorf("another card's write sent this card's token")
	}
	_, _ = e.server.handleAddComment(e.ctx, call(map[string]any{"task_id": e.taskID[:8], "body": "x"}))
	if got := e.mesh.last(t).header.Get(fenceTokenHeader); got != token {
		t.Errorf("short id: token %q, want the card's", got)
	}
}

// A get_task in this process supersedes the file's input_revision on finish.
func TestFence_FinishUsesFreshRevisionAfterGetTask(t *testing.T) {
	e := newFenceEnv(t)
	e.publishToken(t, uuid.New().String(), 3)
	fin := call(map[string]any{"task_id": e.taskID, "result_kind": "change", "evidence": "ok"})

	_, _ = e.server.handleFinishTask(e.ctx, fin)
	if got := e.mesh.last(t).header.Get(fenceRevisionHeader); got != "3" {
		t.Fatalf("before get_task: revision %q, want the file's 3", got)
	}
	e.mesh.mu.Lock()
	e.mesh.revision = 5
	e.mesh.mu.Unlock()
	if res, err := e.server.handleGetTask(e.ctx, call(map[string]any{"task_id": e.taskID})); err != nil || res.IsError {
		t.Fatalf("get_task: %v %v", res, err)
	}
	_, _ = e.server.handleFinishTask(e.ctx, call(map[string]any{"task_id": e.taskID, "result_kind": "change", "evidence": "ok 2"}))
	if got := e.mesh.last(t).header.Get(fenceRevisionHeader); got != "5" {
		t.Fatalf("after get_task: revision %q, want the fresh 5", got)
	}
}

func TestFence_LockLost_JournalledAndAgentTold(t *testing.T) {
	e := newFenceEnv(t)
	e.publishToken(t, uuid.New().String(), 3)
	at := time.Date(2026, 11, 14, 4, 30, 0, 0, time.UTC)
	old := fenceRefusalNow
	fenceRefusalNow = func() time.Time { return at }
	t.Cleanup(func() { fenceRefusalNow = old })
	e.mesh.refuse = fenceLockLost
	e.mesh.refCtx = map[string]any{"reassigned": true, "checkout_generation": 8}

	res, err := e.server.handleAddComment(e.ctx, call(map[string]any{"task_id": e.taskID, "body": "late"}))
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("want a tool error, got %v %v", res, err)
	}
	if text := resultText(t, res); !strings.Contains(text, fenceLockLostHint) {
		t.Errorf("tool result %q does not tell the agent to stop", text)
	}
	rows := e.journal(t)
	if len(rows) != 1 {
		t.Fatalf("journal rows = %d, want 1", len(rows))
	}
	want := map[string]any{"attempt_id": "att-1", "turn": float64(4), "code": fenceLockLost,
		"at": "2026-11-14T04:30:00Z", "tool": "add_comment", "reassigned": true, "input_revision": nil}
	for k, v := range want {
		if rows[0][k] != v {
			t.Errorf("journal %s = %v, want %v (row %v)", k, rows[0][k], v, rows[0])
		}
	}
}

func TestFence_RevisionChanged_JournalledAndAgentTold(t *testing.T) {
	e := newFenceEnv(t)
	e.publishToken(t, uuid.New().String(), 3)
	e.mesh.refuse = fenceRevisionChanged
	e.mesh.refCtx = map[string]any{"input_revision": 9}

	res, err := e.server.handleFinishTask(e.ctx, call(map[string]any{"task_id": e.taskID, "result_kind": "change", "evidence": "ok"}))
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("want a tool error, got %v %v", res, err)
	}
	if text := resultText(t, res); !strings.Contains(text, fenceRevisionChangedHint) || !strings.Contains(text, fenceRevisionChanged) {
		t.Errorf("tool result %q does not tell the agent to re-read the task", text)
	}
	rows := e.journal(t)
	if len(rows) != 1 || rows[0]["code"] != fenceRevisionChanged || rows[0]["tool"] != "finish_task" ||
		rows[0]["input_revision"] != float64(9) || rows[0]["attempt_id"] != "att-1" || rows[0]["turn"] != float64(4) ||
		rows[0]["reassigned"] != false {
		t.Fatalf("journal = %v", rows)
	}
}

// A 409 that is not a fence refusal, or a refusal of an unfenced write,
// journals nothing.
func TestFence_OtherConflictsNotJournalled(t *testing.T) {
	e := newFenceEnv(t)
	e.mesh.refuse = fenceLockLost
	_, _ = e.server.handleAddComment(e.ctx, call(map[string]any{"task_id": e.taskID, "body": "x"}))
	e.publishToken(t, uuid.New().String(), 3)
	e.mesh.refuse = "conflict"
	_, _ = e.server.handleAddComment(e.ctx, call(map[string]any{"task_id": e.taskID, "body": "y"}))
	if _, err := os.Stat(filepath.Join(e.dir, e.taskID+".refusals.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("journal written for a non-fence refusal: %v", err)
	}
}

// The fence rides on the same request as the Idempotency-Key, JSON and
// multipart alike, and a lost answer is re-sent with both.
func TestFence_KeyAndFenceOnOneRequest(t *testing.T) {
	e := newFenceEnv(t)
	token := uuid.New().String()
	e.publishToken(t, token, 3)
	for tool, seen := range e.writeAll(t) {
		key := seen.header.Get(idempotencyKeyHeader)
		keyed := tool == "add_comment" || tool == "upload_artifact" || tool == "finish_task"
		if keyed && !strings.HasPrefix(key, writeKeyPrefix) {
			t.Errorf("%s: Idempotency-Key %q missing next to the fence", tool, key)
		}
		if seen.header.Get(fenceTokenHeader) != token {
			t.Errorf("%s: fence missing next to the key", tool)
		}
	}

	f := newFakeIdemMesh()
	f.dropAnswer = func(n int) bool { return n == 1 }
	var mu sync.Mutex
	var tokens []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		tokens = append(tokens, r.Header.Get(fenceTokenHeader))
		mu.Unlock()
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e.server.restClient = NewRESTClient(srv.URL, "agk_test")
	res, err := e.server.handleAddComment(e.ctx, call(map[string]any{"task_id": e.taskID, "body": "lost once"}))
	mustOK(t, res, err)
	writes, requests, keys := f.snapshot()
	if writes != 1 || requests != 2 {
		t.Fatalf("writes/requests = %d/%d, want 1/2", writes, requests)
	}
	allSameNonEmpty(t, keys)
	for _, tok := range tokens {
		if tok != token {
			t.Fatalf("re-sent request lost the fence: %q", tokens)
		}
	}
}

// The move went through but its optional comment was refused by the fence:
// the tool result must say so, it is not a best-effort comment failure.
func TestFence_MoveCommentRefusalReachesAgent(t *testing.T) {
	e := newFenceEnv(t)
	e.publishToken(t, uuid.New().String(), 3)
	mesh := e.mesh
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments") {
			mesh.mu.Lock()
			mesh.refuse = fenceLockLost
			mesh.mu.Unlock()
		} else if r.Method == http.MethodPost {
			mesh.mu.Lock()
			mesh.refuse = ""
			mesh.mu.Unlock()
		}
		mesh.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e.server.restClient = NewRESTClient(srv.URL, "agk_test")

	res, err := e.server.handleMoveTask(e.ctx, call(map[string]any{"task_id": e.taskID, "status_slug": "review", "comment": "moving"}))
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("want a tool error for the refused move comment, got %v %v", res, err)
	}
	if text := resultText(t, res); !strings.Contains(text, fenceLockLostHint) {
		t.Errorf("tool result %q does not tell the agent to stop", text)
	}
	if rows := e.journal(t); len(rows) != 1 || rows[0]["tool"] != "move_task" {
		t.Errorf("journal = %v, want one move_task row", rows)
	}
}

// A fresh revision read by get_task survives any number of other cards read
// before the finish.
func TestFence_FreshRevisionSurvivesManyOtherReads(t *testing.T) {
	e := newFenceEnv(t)
	e.publishToken(t, uuid.New().String(), 3)
	defaultInputRevisions.observe(map[string]any{"id": e.taskID, "input_revision": float64(5)})
	for i := 0; i < 5000; i++ {
		defaultInputRevisions.observe(map[string]any{"id": uuid.New().String(), "input_revision": float64(1)})
	}
	_, _ = e.server.handleFinishTask(e.ctx, call(map[string]any{"task_id": e.taskID, "result_kind": "change", "evidence": "ok"}))
	if got := e.mesh.last(t).header.Get(fenceRevisionHeader); got != "5" {
		t.Fatalf("revision %q after many other reads, want the fresh 5", got)
	}
}

// Without a fence directory on the host nothing is recorded.
func TestFence_NoFenceDir_NoRevisionsRecorded(t *testing.T) {
	newFenceEnv(t)
	t.Setenv(checkoutFenceDirEnv, filepath.Join(t.TempDir(), "absent"))
	id := uuid.New().String()
	defaultInputRevisions.observe(map[string]any{"id": id, "input_revision": float64(5)})
	if got := defaultInputRevisions.get(id); got != 0 {
		t.Fatalf("recorded %d without a fence directory", got)
	}
}
