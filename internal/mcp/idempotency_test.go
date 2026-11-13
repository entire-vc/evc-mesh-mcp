package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// fakeIdemMesh is a Mesh write endpoint with the documented Idempotency-Key
// semantics (evc-mesh docs/idempotency.md): a key holds the fingerprint of the
// first body and its answer; a repeat with the same fingerprint gets that
// answer with Idempotent-Replayed: true and writes nothing; another body is
// 409 idempotency_key_reused. Multipart bodies are fingerprinted over their
// parsed parts, as the server does.
type fakeIdemMesh struct {
	mu         sync.Mutex
	writes     int               // rows the endpoint actually wrote
	requests   int               // HTTP requests seen
	keys       []string          // Idempotency-Key of every request, "" when absent
	stored     map[string]stored // key -> first answer
	dropAnswer func(n int) bool  // drop the answer of request n (1-based) after writing
	inProgress int               // answer this many keyed requests with 409 in_progress first
	retryAfter string
	reuseAll   bool // answer every keyed request with 409 idempotency_key_reused
}

type stored struct {
	fingerprint string
	status      int
	body        []byte
}

func newFakeIdemMesh() *fakeIdemMesh {
	return &fakeIdemMesh{stored: map[string]stored{}, dropAnswer: func(int) bool { return false }, retryAfter: "1"}
}

func fingerprint(r *http.Request, raw []byte) string {
	mt, params, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if mt != "multipart/form-data" {
		sum := sha256.Sum256(raw)
		return hex.EncodeToString(sum[:])
	}
	h := sha256.New()
	mr := multipart.NewReader(bytes.NewReader(raw), params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			break
		}
		for _, k := range []string{"Content-Disposition", "Content-Type"} {
			_, _ = io.WriteString(h, k+":"+p.Header.Get(k)+"\n")
		}
		_, _ = io.Copy(h, p)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (f *fakeIdemMesh) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	key := r.Header.Get("Idempotency-Key")
	f.mu.Lock()
	f.requests++
	n := f.requests
	f.keys = append(f.keys, key)
	if key != "" && f.inProgress > 0 {
		f.inProgress--
		f.mu.Unlock()
		w.Header().Set("Retry-After", f.retryAfter)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error_code":"idempotency_request_in_progress","message":"in progress"}`))
		return
	}
	if key != "" && f.reuseAll {
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error_code":"idempotency_key_reused","message":"key reused"}`))
		return
	}
	fp := fingerprint(r, raw)
	if prev, ok := f.stored[key]; key != "" && ok {
		drop := f.dropAnswer(n)
		f.mu.Unlock()
		if drop {
			hangUp(w)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if prev.fingerprint != fp {
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error_code":"idempotency_key_reused","message":"key reused"}`))
			return
		}
		w.Header().Set("Idempotent-Replayed", "true")
		w.WriteHeader(prev.status)
		_, _ = w.Write(prev.body)
		return
	}
	f.writes++
	body, _ := json.Marshal(map[string]any{"id": uuid.New().String(), "task_id": "t", "body": "x", "released": true})
	if key != "" {
		f.stored[key] = stored{fingerprint: fp, status: http.StatusCreated, body: body}
	}
	drop := f.dropAnswer(n)
	f.mu.Unlock()
	if drop {
		hangUp(w) // the write happened; the answer never reaches the client
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_, _ = w.Write(body)
}

// hangUp closes the connection without an answer: a timeout or reset as the
// client sees it.
func hangUp(w http.ResponseWriter) {
	if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
		_ = conn.Close()
	}
}

func (f *fakeIdemMesh) snapshot() (writes, requests int, keys []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes, f.requests, append([]string(nil), f.keys...)
}

// idemTestEnv isolates one test: keys on, a fresh key book (a fresh
// process), a fixed runner checkout, no real sleeping.
func idemTestEnv(t *testing.T) *[]time.Duration {
	t.Helper()
	t.Setenv(idempotencyKeysEnv, "")
	t.Setenv(runnerCheckoutSessionEnv, "sess-"+uuid.New().String())
	t.Setenv(runnerCheckoutGenerationEnv, "1")
	oldBook, oldSleep, oldDelays := defaultWriteKeys, idempotencySleep, lostAnswerDelays
	defaultWriteKeys = newWriteKeyBook()
	var slept []time.Duration
	var mu sync.Mutex
	idempotencySleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		slept = append(slept, d)
		mu.Unlock()
		return ctx.Err()
	}
	lostAnswerDelays = []time.Duration{time.Millisecond, 2 * time.Millisecond}
	t.Cleanup(func() { defaultWriteKeys, idempotencySleep, lostAnswerDelays = oldBook, oldSleep, oldDelays })
	return &slept
}

func commentRequest(taskID, body string) mcpsdk.CallToolRequest {
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID, "body": body}
	return req
}

func newIdemServer(t *testing.T, f *fakeIdemMesh) (*Server, context.Context) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	server := &Server{restClient: NewRESTClient(srv.URL, "agk_test"), tracker: NewSessionTracker()}
	return server, withTestSession(context.Background(), server, uuid.New())
}

func allSameNonEmpty(t *testing.T, keys []string) {
	t.Helper()
	if len(keys) == 0 || keys[0] == "" || !strings.HasPrefix(keys[0], writeKeyPrefix) {
		t.Fatalf("keys = %q, want a non-empty %q key on every send", keys, writeKeyPrefix)
	}
	for _, k := range keys {
		if k != keys[0] {
			t.Fatalf("keys differ between sends of one write: %q", keys)
		}
	}
}

// A lost answer inside one tool call is re-sent with the same key; the server
// replays and the comment exists once.
func TestAddComment_LostAnswerResentWithSameKey(t *testing.T) {
	idemTestEnv(t)
	f := newFakeIdemMesh()
	f.dropAnswer = func(n int) bool { return n == 1 }
	server, ctx := newIdemServer(t, f)

	res, err := server.handleAddComment(ctx, commentRequest("task-1", "Done, verified"))
	m := decodeResultMap(t, mustOK(t, res, err))

	writes, requests, keys := f.snapshot()
	if writes != 1 {
		t.Fatalf("writes = %d, want 1", writes)
	}
	if requests < 2 {
		t.Fatalf("requests = %d, want the lost answer re-sent", requests)
	}
	allSameNonEmpty(t, keys)
	if m["idempotent_replayed"] != true {
		t.Errorf("result = %v, want idempotent_replayed=true", m)
	}
}

// Every internal send loses its answer, the tool call fails; the agent repeats
// the tool call with the same arguments and gets the first write back: one
// comment, not two.
func TestAddComment_AgentRepeatAfterLostAnswer_OneComment(t *testing.T) {
	idemTestEnv(t)
	f := newFakeIdemMesh()
	var lost atomic.Bool
	lost.Store(true)
	f.dropAnswer = func(int) bool { return lost.Load() } // every send of the first call loses its answer
	server, ctx := newIdemServer(t, f)

	res, err := server.handleAddComment(ctx, commentRequest("task-1", "Done, verified"))
	if err != nil || res == nil || !res.IsError {
		t.Fatalf("first call: want a tool error after every answer was lost, got res=%v err=%v", res, err)
	}
	lost.Store(false)
	res, err = server.handleAddComment(ctx, commentRequest("task-1", "Done, verified"))
	m := decodeResultMap(t, mustOK(t, res, err))

	writes, _, keys := f.snapshot()
	if writes != 1 {
		t.Fatalf("writes = %d, want 1 (the repeat must not post a second comment)", writes)
	}
	allSameNonEmpty(t, keys)
	if m["idempotent_replayed"] != true {
		t.Errorf("repeat result = %v, want idempotent_replayed=true", m)
	}
}

// The MCP process restarts between the send and the answer: the new process
// (fresh key book, same runner checkout) derives the same key, so the repeat
// is a replay.
func TestAddComment_RestartBetweenSendAndAnswer(t *testing.T) {
	idemTestEnv(t)
	f := newFakeIdemMesh()
	var lost atomic.Bool
	lost.Store(true)
	f.dropAnswer = func(int) bool { return lost.Load() }
	server, ctx := newIdemServer(t, f)
	res, _ := server.handleAddComment(ctx, commentRequest("task-1", "Done, verified"))
	if res == nil || !res.IsError {
		t.Fatalf("first process: want a lost-answer error, got %v", res)
	}
	lost.Store(false)

	defaultWriteKeys = newWriteKeyBook() // the restarted process
	restarted := &Server{restClient: NewRESTClient(server.restClient.BaseURL(), "agk_test"), tracker: NewSessionTracker()}
	res, err := restarted.handleAddComment(ctx, commentRequest("task-1", "Done, verified"))
	mustOK(t, res, err)

	writes, _, keys := f.snapshot()
	if writes != 1 {
		t.Fatalf("writes = %d, want 1 across the restart", writes)
	}
	allSameNonEmpty(t, keys)
}

// Outside a runner checkout a restarted process has a new scope: its key is a
// new one. Pinned so nobody assumes cross-restart safety without the runner.
func TestWriteKey_ScopeWithoutRunnerIsPerProcess(t *testing.T) {
	t.Setenv(runnerCheckoutSessionEnv, "")
	a := newWriteKeyBook()
	b := newWriteKeyBook()
	b.scope = func() string { return "proc-other" }
	if a.key("c", "add_comment", "t", map[string]any{"body": "x"}) == b.key("c", "add_comment", "t", map[string]any{"body": "x"}) {
		t.Fatal("two processes without a runner checkout must not share a key")
	}
}

// Same arguments within the window: same key. After the window: a new write.
// Different card, tool, caller, arguments or checkout: a different key.
func TestWriteKey_WindowAndScope(t *testing.T) {
	now := time.Date(2026, 11, 12, 20, 0, 0, 0, time.UTC)
	b := newWriteKeyBook()
	b.now = func() time.Time { return now }
	b.window = func() time.Duration { return 10 * time.Minute }
	b.scope = func() string { return "runner-s-1" }
	args := map[string]any{"body": "x", "is_internal": false}

	k1 := b.key("caller", "add_comment", "t1", args)
	now = now.Add(9 * time.Minute)
	if k := b.key("caller", "add_comment", "t1", args); k != k1 {
		t.Fatal("a repeat within the window must keep the key")
	}
	now = now.Add(11 * time.Minute)
	k3 := b.key("caller", "add_comment", "t1", args)
	if k3 == k1 {
		t.Fatal("the same arguments after the window are a new write and need a new key")
	}
	for name, k := range map[string]string{
		"task":   b.key("caller", "add_comment", "t2", args),
		"tool":   b.key("caller", "upload_artifact", "t1", args),
		"caller": b.key("other", "add_comment", "t1", args),
		"args":   b.key("caller", "add_comment", "t1", map[string]any{"body": "y", "is_internal": false}),
	} {
		if k == k3 {
			t.Errorf("a different %s must give a different key", name)
		}
	}
	b.scope = func() string { return "runner-s-2" }
	if b.key("caller", "add_comment", "t1", args) == k3 {
		t.Error("a new runner checkout must give a different key")
	}
	if len(k1) > 255 || strings.ContainsAny(k1, " \t") {
		t.Errorf("key %q breaks the server's 1-255 visible ASCII rule", k1)
	}
}

// in_progress: the repeat waits the server's Retry-After and is sent again
// with the same key; never earlier.
func TestAddComment_InProgressWaitsRetryAfter(t *testing.T) {
	slept := idemTestEnv(t)
	f := newFakeIdemMesh()
	f.inProgress = 1
	f.retryAfter = "2"
	server, ctx := newIdemServer(t, f)

	res, err := server.handleAddComment(ctx, commentRequest("task-1", "x"))
	mustOK(t, res, err)
	writes, requests, keys := f.snapshot()
	if writes != 1 || requests != 2 {
		t.Fatalf("writes=%d requests=%d, want 1 write after one in_progress answer", writes, requests)
	}
	allSameNonEmpty(t, keys)
	if len(*slept) != 1 || (*slept)[0] < 2*time.Second {
		t.Fatalf("slept %v, want one wait of at least Retry-After 2s", *slept)
	}
}

// A Retry-After longer than the client may wait is never cut short: the
// refusal goes back to the agent, nothing is re-sent.
func TestAddComment_InProgressLongRetryAfterNotRepeated(t *testing.T) {
	slept := idemTestEnv(t)
	f := newFakeIdemMesh()
	f.inProgress = 1
	f.retryAfter = "120"
	server, ctx := newIdemServer(t, f)

	res, _ := server.handleAddComment(ctx, commentRequest("task-1", "x"))
	if res == nil || !res.IsError {
		t.Fatalf("want the in_progress refusal returned, got %v", res)
	}
	if _, requests, _ := f.snapshot(); requests != 1 || len(*slept) != 0 {
		t.Fatalf("requests=%d slept=%v, want one send and no wait", requests, *slept)
	}
}

// reused: the key already holds a write; counted as written, not an error.
func TestAddComment_KeyReusedCountsAsWritten(t *testing.T) {
	idemTestEnv(t)
	f := newFakeIdemMesh()
	server, ctx := newIdemServer(t, f)
	key := server.restClient.writeKey("add_comment", "task-1", map[string]any{"body": "x", "is_internal": false})
	defaultWriteKeys = newWriteKeyBook() // same process state as before the first send
	f.stored[key] = stored{fingerprint: "another body", status: http.StatusCreated, body: []byte(`{}`)}

	res, err := server.handleAddComment(ctx, commentRequest("task-1", "x"))
	m := decodeResultMap(t, mustOK(t, res, err))
	if m["idempotent_reused"] != true {
		t.Fatalf("result = %v, want idempotent_reused=true", m)
	}
	if writes, _, _ := f.snapshot(); writes != 0 {
		t.Fatalf("writes = %d, want 0", writes)
	}
}

// Rollback: MESH_MCP_IDEMPOTENCY_KEYS=0 sends no key and re-sends nothing.
func TestAddComment_KeysOff_ExactlyAsBefore(t *testing.T) {
	idemTestEnv(t)
	t.Setenv(idempotencyKeysEnv, "0")
	f := newFakeIdemMesh()
	f.dropAnswer = func(n int) bool { return n == 1 }
	server, ctx := newIdemServer(t, f)

	res, _ := server.handleAddComment(ctx, commentRequest("task-1", "x"))
	if res == nil || !res.IsError {
		t.Fatalf("keys off: a lost answer must surface as before, got %v", res)
	}
	_, requests, keys := f.snapshot()
	if requests != 1 || keys[0] != "" {
		t.Fatalf("keys off: requests=%d keys=%q, want one send without Idempotency-Key", requests, keys)
	}
}

// Artifact: a lost answer is re-sent with the same key and the same parts;
// an agent repeat rebuilds identical parts (sorted fields), so it replays.
func TestUploadArtifact_LostAnswerAndAgentRepeat_OneArtifact(t *testing.T) {
	idemTestEnv(t)
	f := newFakeIdemMesh()
	var lost atomic.Bool
	lost.Store(true)
	f.dropAnswer = func(int) bool { return lost.Load() }
	server, ctx := newIdemServer(t, f)
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"task_id": "task-1", "name": "report.md", "content": "# report\n",
		"artifact_type": "file", "metadata": map[string]any{"b": "2", "a": "1"},
	}

	res, _ := server.handleUploadArtifact(ctx, req)
	if res == nil || !res.IsError {
		t.Fatalf("first call: want a lost-answer error, got %v", res)
	}
	lost.Store(false)
	res, err := server.handleUploadArtifact(ctx, req)
	m := decodeResultMap(t, mustOK(t, res, err))

	writes, _, keys := f.snapshot()
	if writes != 1 {
		t.Fatalf("writes = %d, want 1 artifact", writes)
	}
	allSameNonEmpty(t, keys)
	if m["idempotent_replayed"] != true {
		t.Errorf("result = %v, want idempotent_replayed=true", m)
	}
}

// finish_task: a lost answer is re-sent with the same key; other turn
// commands send no key.
func TestFinishTask_LostAnswerResentWithSameKey(t *testing.T) {
	idemTestEnv(t)
	f := newFakeIdemMesh()
	f.dropAnswer = func(n int) bool { return n == 1 }
	server, ctx := newIdemServer(t, f)
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": "task-1", "result_kind": "change", "evidence": "go test ./... ok"}

	res, err := server.handleFinishTask(ctx, req)
	m := decodeResultMap(t, mustOK(t, res, err))
	writes, _, keys := f.snapshot()
	if writes != 1 {
		t.Fatalf("writes = %d, want 1", writes)
	}
	allSameNonEmpty(t, keys)
	if m["idempotent_replayed"] != true {
		t.Errorf("result = %v, want idempotent_replayed=true", m)
	}

	cont := mcpsdk.CallToolRequest{}
	cont.Params.Arguments = map[string]any{"task_id": "task-1", "note": "next"}
	_, _ = server.handleContinueNext(ctx, cont)
	if _, _, keys := f.snapshot(); keys[len(keys)-1] != "" {
		t.Errorf("continue_next sent Idempotency-Key %q; only finish_task is keyed", keys[len(keys)-1])
	}
}

// finish_task answered key_reused: the close already went through, so it is
// counted as written and the cached checkout token is forgotten, as on an
// ordinary released answer.
func TestFinishTask_KeyReusedClearsCheckout(t *testing.T) {
	idemTestEnv(t)
	f := newFakeIdemMesh()
	f.reuseAll = true
	server, ctx := newIdemServer(t, f)
	server.checkouts.Store("task-1", "tok-1")
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": "task-1", "result_kind": "change", "evidence": "go test ./... ok"}

	res, err := server.handleFinishTask(ctx, req)
	m := decodeResultMap(t, mustOK(t, res, err))
	if m["idempotent_reused"] != true {
		t.Fatalf("result = %v, want idempotent_reused=true", m)
	}
	if _, ok := server.checkouts.Load("task-1"); ok {
		t.Fatal("checkout token still cached after a finish the server already applied")
	}
	if writes, _, _ := f.snapshot(); writes != 0 {
		t.Fatalf("writes = %d, want 0", writes)
	}
}
