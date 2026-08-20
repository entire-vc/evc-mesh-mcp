package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// docStore is a fake Mesh API that implements the document write contract the
// real server implements: a monotonic version, a conditional write that refuses
// with 409 and does NOT write, and an append that retries through a conflict
// instead of failing.
//
// It is a model of the server, so a test against it proves the CLIENT's half
// only. The server's half is proved separately against the live API — see the
// task thread. Written out rather than stubbed because the behaviours that
// matter here are exactly the ones a stub would paper over.
type docStore struct {
	mu      sync.Mutex
	body    string
	title   string
	version int
	docID   string
	server  *httptest.Server
}

func newDocStore(t *testing.T, body string) *docStore {
	t.Helper()
	st := &docStore{body: body, title: "Doc", version: 1, docID: uuid.New().String()}

	st.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		if r.Method == http.MethodPost {
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			st.mu.Lock()
			defer st.mu.Unlock()
			st.title, _ = req["title"].(string)
			st.body, _ = req["body"].(string)
			st.version = 1
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(st.snapshot())
			return
		}

		if r.Method == http.MethodPatch {
			var req map[string]any
			_ = json.NewDecoder(r.Body).Decode(&req)
			st.mu.Lock()
			defer st.mu.Unlock()

			appendText, isAppend := req["append_body"].(string)
			base, hasBase := req["base_version"].(float64)

			if hasBase && int(base) != st.version {
				w.WriteHeader(http.StatusConflict)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"code":            "document_version_conflict",
					"current_version": st.version,
					"message":         "Document was modified since you read it",
				})
				return
			}

			switch {
			case isAppend:
				st.body += appendText
			default:
				if b, ok := req["body"].(string); ok {
					st.body = b
				}
			}
			if t, ok := req["title"].(string); ok {
				st.title = t
			}
			st.version++
			_ = json.NewEncoder(w).Encode(st.snapshot())
			return
		}

		st.mu.Lock()
		defer st.mu.Unlock()
		_ = json.NewEncoder(w).Encode(st.snapshot())
	}))
	t.Cleanup(st.server.Close)
	return st
}

// snapshot must be called with the lock held.
func (st *docStore) snapshot() map[string]any {
	return map[string]any{
		"id": st.docID, "title": st.title, "version": st.version,
		"body": st.body, "storage_key": "s3://bucket/" + st.docID,
		"slug": "doc", "parent_id": nil,
	}
}

func (st *docStore) read() (string, int) {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.body, st.version
}

func (st *docStore) newServer() *Server {
	return &Server{
		restClient: NewRESTClient(st.server.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
}

// callTool returns the result without failing on IsError, so error paths can be
// asserted on directly.
func callTool(t *testing.T, fn func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error), args map[string]any) (string, bool) {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	result, err := fn(context.Background(), req)
	if err != nil {
		t.Fatalf("tool returned a transport error: %v", err)
	}
	return result.Content[0].(mcpsdk.TextContent).Text, result.IsError
}

// AC1: create_doc creates a document.
func TestCreateDoc_CreatesAndReturnsVersion(t *testing.T) {
	st := newDocStore(t, "")

	text, isErr := callTool(t, st.newServer().handleCreateDoc, map[string]any{
		"project_id": uuid.New().String(),
		"title":      "ADR 005",
		"body":       "# ADR 005\n\nТело документа.\n",
	})
	if isErr {
		t.Fatalf("create_doc failed: %s", text)
	}

	var out map[string]any
	_ = json.Unmarshal([]byte(text), &out)
	if out["version"] == nil {
		t.Errorf("create_doc did not return a version, so the caller cannot edit without a read: %v", out)
	}
	if _, present := out["body"]; present {
		t.Errorf("create_doc echoed the body back, spending context on text the caller already has: %v", out)
	}
	body, _ := st.read()
	if !strings.Contains(body, "Тело документа") {
		t.Errorf("document was not stored: %q", body)
	}
}

// AC2: a write with the right base_version lands and moves the version on.
func TestUpdateDoc_ConditionalWriteSucceedsAndBumpsVersion(t *testing.T) {
	st := newDocStore(t, "original\n")

	text, isErr := callTool(t, st.newServer().handleUpdateDoc, map[string]any{
		"doc": st.docID, "base_version": 1, "body": "rewritten\n",
	})
	if isErr {
		t.Fatalf("conditional write failed: %s", text)
	}
	body, version := st.read()
	if body != "rewritten\n" || version != 2 {
		t.Errorf("after write: body=%q version=%d, want \"rewritten\\n\" and 2", body, version)
	}
}

// AC3, the load-bearing negative control: the loser of a race must not write.
//
// Asserted on the BODY, not on the status code — a 409 that wrote and a 409 that
// did not look identical from the status alone, and it is the write that matters.
func TestUpdateDoc_StaleBaseVersionIsRefusedAndNothingIsWritten(t *testing.T) {
	st := newDocStore(t, "original\n")
	server := st.newServer()

	if _, isErr := callTool(t, server.handleUpdateDoc, map[string]any{
		"doc": st.docID, "base_version": 1, "body": "first writer won\n",
	}); isErr {
		t.Fatal("the first write should have succeeded")
	}
	afterFirst, versionAfterFirst := st.read()

	text, isErr := callTool(t, server.handleUpdateDoc, map[string]any{
		"doc": st.docID, "base_version": 1, "body": "second writer must not land\n",
	})
	if !isErr {
		t.Fatalf("stale write was accepted: %s", text)
	}

	afterSecond, versionAfterSecond := st.read()
	if afterSecond != afterFirst {
		t.Errorf("the refused write changed the document: %q -> %q", afterFirst, afterSecond)
	}
	if versionAfterSecond != versionAfterFirst {
		t.Errorf("the refused write moved the version: %d -> %d", versionAfterFirst, versionAfterSecond)
	}
	if strings.Contains(afterSecond, "must not land") {
		t.Errorf("the losing writer's text reached the document: %q", afterSecond)
	}
}

// AC4: the conflict carries the current version, and retrying with it works.
func TestUpdateDoc_ConflictCarriesCurrentVersionAndRetrySucceeds(t *testing.T) {
	st := newDocStore(t, "original\n")
	server := st.newServer()

	_, _ = callTool(t, server.handleUpdateDoc, map[string]any{
		"doc": st.docID, "base_version": 1, "body": "first\n",
	})
	text, isErr := callTool(t, server.handleUpdateDoc, map[string]any{
		"doc": st.docID, "base_version": 1, "body": "second\n",
	})
	if !isErr {
		t.Fatal("expected a conflict")
	}
	// The number has to be usable without re-reading the document — a conflict a
	// caller cannot act on turns into a loop of blind retries.
	if !strings.Contains(text, "base_version=2") {
		t.Errorf("conflict did not name the version to retry with: %s", text)
	}
	if !strings.Contains(text, "NOTHING WAS WRITTEN") {
		t.Errorf("conflict does not make it unmistakable that no write happened: %s", text)
	}

	if _, isErr := callTool(t, server.handleUpdateDoc, map[string]any{
		"doc": st.docID, "base_version": 2, "body": "second, rebased\n",
	}); isErr {
		t.Fatal("retry with the version from the conflict should have succeeded")
	}
	if body, _ := st.read(); body != "second, rebased\n" {
		t.Errorf("retry did not land: %q", body)
	}
}

// AC5: append needs no version and leaves everything before it untouched.
func TestUpdateDoc_AppendNeedsNoVersionAndPreservesPrefix(t *testing.T) {
	st := newDocStore(t, "# Doc\n\nоригинальный текст\n")

	text, isErr := callTool(t, st.newServer().handleUpdateDoc, map[string]any{
		"doc": st.docID, "append": "\n## Отчёт\n\nдописано\n",
	})
	if isErr {
		t.Fatalf("append without base_version was refused: %s", text)
	}
	body, _ := st.read()
	if !strings.HasPrefix(body, "# Doc\n\nоригинальный текст\n") {
		t.Errorf("append disturbed the text before it: %q", body)
	}
	if !strings.HasSuffix(body, "дописано\n") {
		t.Errorf("appended text is not at the end: %q", body)
	}
}

// Two concurrent appends, through the tool, both land.
//
// ⚠️ Read what this does and does not prove. The fake store serializes every
// PATCH under one mutex, so an append can never lose a race INSIDE it — against
// this fake the assertion cannot fail on server behaviour, and citing it as
// proof that "concurrent appends are safe" would be citing a probe that cannot
// fail.
//
// What it does prove is the client's half, which is not nothing: that the tool
// forwards two appends as two independent appends — no client-side read-modify-
// write, no shared cached version, no serialization that would turn the second
// into an overwrite of the first. Break that (say, by having update_doc read the
// document and resend the whole body) and this test fails.
//
// The system-level property — that the SERVER's append survives a real race —
// is M1's, it cannot be proved from here, and it is verified against the live
// API instead. See the task thread for that run.
func TestUpdateDoc_ConcurrentAppendsBothLand(t *testing.T) {
	st := newDocStore(t, "start\n")
	server := st.newServer()

	var wg sync.WaitGroup
	for _, text := range []string{"ОДИН\n", "ДВА\n"} {
		wg.Add(1)
		go func(payload string) {
			defer wg.Done()
			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = map[string]any{"doc": st.docID, "append": payload}
			_, _ = server.handleUpdateDoc(context.Background(), req)
		}(text)
	}
	wg.Wait()

	body, version := st.read()
	if !strings.Contains(body, "ОДИН") || !strings.Contains(body, "ДВА") {
		t.Errorf("an append was lost: the tool is not forwarding the two appends independently: %q", body)
	}
	if !strings.HasPrefix(body, "start\n") {
		t.Errorf("concurrent appends damaged the original text: %q", body)
	}
	if version != 3 {
		t.Errorf("version = %d after two appends, want 3", version)
	}
}

// AC7: a body write with no base_version is refused rather than written.
//
// The API itself accepts this and writes unconditionally, on purpose, because
// the existing browser editor sends exactly that. The refusal therefore has to
// live in the tool — otherwise one forgetful call silently overwrites somebody
// else's edit, which is the accident the feature was built after.
func TestUpdateDoc_MissingBaseVersionIsRefused(t *testing.T) {
	st := newDocStore(t, "original\n")

	text, isErr := callTool(t, st.newServer().handleUpdateDoc, map[string]any{
		"doc": st.docID, "body": "written with no version at all\n",
	})
	if !isErr {
		t.Fatalf("a versionless body write was accepted: %s", text)
	}
	if !strings.Contains(text, "base_version is required") {
		t.Errorf("refusal does not say what is missing: %s", text)
	}
	if !strings.Contains(text, "append") {
		t.Errorf("refusal does not point at the cheaper alternative: %s", text)
	}

	body, version := st.read()
	if body != "original\n" || version != 1 {
		t.Errorf("the refused write reached the document: body=%q version=%d", body, version)
	}
}

// A title-only edit is still a write and still needs a version — otherwise the
// gate is bypassed by simply not sending a body.
func TestUpdateDoc_TitleOnlyWriteAlsoNeedsBaseVersion(t *testing.T) {
	st := newDocStore(t, "original\n")

	text, isErr := callTool(t, st.newServer().handleUpdateDoc, map[string]any{
		"doc": st.docID, "title": "renamed with no version",
	})
	if !isErr {
		t.Fatalf("a versionless title write was accepted: %s", text)
	}
	if !strings.Contains(text, "base_version is required") {
		t.Errorf("unexpected refusal reason: %s", text)
	}
}

// Replace and append are different intentions; guessing between them is how the
// wrong text ends up in a document.
func TestUpdateDoc_BodyAndAppendTogetherIsRefused(t *testing.T) {
	st := newDocStore(t, "original\n")

	text, isErr := callTool(t, st.newServer().handleUpdateDoc, map[string]any{
		"doc": st.docID, "base_version": 1, "body": "replace\n", "append": "add\n",
	})
	if !isErr {
		t.Fatalf("body+append together was accepted: %s", text)
	}
	if !strings.Contains(text, "not both") {
		t.Errorf("unexpected refusal reason: %s", text)
	}
	if body, _ := st.read(); body != "original\n" {
		t.Errorf("the ambiguous call wrote something: %q", body)
	}
}

// An append that DOES carry a version is asking to be told about conflicts
// rather than retried through them — a legitimate "add to the end only if
// nothing changed".
func TestUpdateDoc_AppendWithStaleVersionConflictsInsteadOfRetrying(t *testing.T) {
	st := newDocStore(t, "original\n")
	server := st.newServer()

	_, _ = callTool(t, server.handleUpdateDoc, map[string]any{
		"doc": st.docID, "base_version": 1, "body": "moved on\n",
	})

	text, isErr := callTool(t, server.handleUpdateDoc, map[string]any{
		"doc": st.docID, "base_version": 1, "append": "should not land\n",
	})
	if !isErr {
		t.Fatalf("append with a stale version should have conflicted: %s", text)
	}
	if body, _ := st.read(); strings.Contains(body, "should not land") {
		t.Errorf("the conflicting append wrote anyway: %q", body)
	}
}
