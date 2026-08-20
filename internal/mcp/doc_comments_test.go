package mcp

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// commentStore is a fake Mesh API implementing the half of the document-comment
// contract these tools depend on: resolve-anchor really searches the body and
// really counts BYTES, create really refuses the shapes the service refuses, and
// the listing really paginates.
//
// It resolves quotes for itself rather than returning canned offsets, because the
// defect this unit exists to prevent is an offset that is plausible and wrong. A
// stub handing back a hard-coded 137 would pass whether or not the tool ever
// looked at the document — the test would be green for a reason unrelated to the
// thing under test. The server's own resolver is proved separately, against the
// live API; what is proved here is that the tool asks it and carries the answer
// through untouched.
type commentStore struct {
	mu       sync.Mutex
	body     string
	docID    string
	comments []map[string]any
	server   *httptest.Server

	// pageSizeSeen records what the tool asked for, so the pagination walk can be
	// asserted on rather than assumed.
	pageSizeSeen string

	// pageCap is the largest page this fake will serve, however big a page the
	// caller asks for — the way a real server clamps to pagination.MaxPageSize.
	// Lowering it is what makes a multi-page walk actually happen in a test
	// instead of being asserted against a single page that never needed one.
	pageCap int
}

func newCommentStore(t *testing.T, body string) *commentStore {
	t.Helper()
	st := &commentStore{body: body, docID: uuid.New().String(), pageCap: docCommentPageSize}

	st.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/resolve-anchor"):
			st.handleResolveAnchor(w, r)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/comments"):
			st.handleCreate(w, r)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/comments"):
			st.handleList(w, r)
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"message": "not found"})
		}
	}))
	t.Cleanup(st.server.Close)
	return st
}

// handleResolveAnchor is a byte-honest model of pkg/mdoc.ResolveQuote: literal
// occurrences, narrowed by prefix/suffix, refused when several survive.
func (st *commentStore) handleResolveAnchor(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)
	quote, _ := req["quote"].(string)
	prefix, _ := req["prefix"].(string)
	suffix, _ := req["suffix"].(string)

	st.mu.Lock()
	body := st.body
	st.mu.Unlock()

	var starts []int
	for from := 0; ; {
		at := strings.Index(body[from:], quote)
		if at < 0 || quote == "" {
			break
		}
		starts = append(starts, from+at)
		from += at + 1
	}

	if prefix != "" || suffix != "" {
		var narrowed []int
		for _, s := range starts {
			okPrefix := prefix == "" || strings.HasSuffix(body[:s], prefix)
			okSuffix := suffix == "" || strings.HasPrefix(body[s+len(quote):], suffix)
			if okPrefix && okSuffix {
				narrowed = append(narrowed, s)
			}
		}
		starts = narrowed
	}

	switch len(starts) {
	case 0:
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": 400, "message": "No such quote in this document",
		})
	case 1:
		start := starts[0]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"exact": quote, "prefix": prefix, "suffix": suffix,
			"start": start, "end": start + len(quote),
		})
	default:
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code": "ambiguous_quote", "matches": len(starts),
			"message": "this quote occurs several times in the document",
		})
	}
}

func (st *commentStore) handleCreate(w http.ResponseWriter, r *http.Request) {
	var req map[string]any
	_ = json.NewDecoder(r.Body).Decode(&req)

	st.mu.Lock()
	defer st.mu.Unlock()

	comment := map[string]any{
		"id":                uuid.New().String(),
		"document_id":       st.docID,
		"parent_comment_id": req["parent_comment_id"],
		"author_name":       "Bill",
		"author_type":       "agent",
		"body":              req["body"],
		"resolved_at":       nil,
		"anchor":            nil,
	}
	if anchor, ok := req["anchor"].(map[string]any); ok {
		anchor["orphaned"] = anchor["start"] == nil
		comment["anchor"] = anchor
	}
	st.comments = append(st.comments, comment)

	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(comment)
}

func (st *commentStore) handleList(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	defer st.mu.Unlock()

	st.pageSizeSeen = r.URL.Query().Get("page_size")
	includeResolved := r.URL.Query().Get("include_resolved") == "true"

	visible := make([]map[string]any, 0, len(st.comments))
	hidden := make(map[string]bool)
	for _, c := range st.comments {
		if !includeResolved && c["resolved_at"] != nil {
			hidden[c["id"].(string)] = true
			continue
		}
		visible = append(visible, c)
	}
	// A reply is hidden with its thread, the way a COALESCE to the root does it.
	if !includeResolved {
		kept := visible[:0]
		for _, c := range visible {
			if parent, ok := c["parent_comment_id"].(string); ok && hidden[parent] {
				continue
			}
			kept = append(kept, c)
		}
		visible = kept
	}

	pageSize, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
	if pageSize < 1 {
		pageSize = 50
	}
	if st.pageCap > 0 && pageSize > st.pageCap {
		pageSize = st.pageCap
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}

	from := (page - 1) * pageSize
	if from > len(visible) {
		from = len(visible)
	}
	to := from + pageSize
	if to > len(visible) {
		to = len(visible)
	}

	_ = json.NewEncoder(w).Encode(map[string]any{
		"items":       visible[from:to],
		"total_count": len(visible),
		"page":        page,
		"page_size":   pageSize,
		"has_more":    to < len(visible),
	})
}

func (st *commentStore) newServer() *Server {
	return &Server{
		restClient: NewRESTClient(st.server.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
}

// stored returns the anchor the server ended up holding for the nth comment.
func (st *commentStore) storedAnchor(n int) map[string]any {
	st.mu.Lock()
	defer st.mu.Unlock()
	anchor, _ := st.comments[n]["anchor"].(map[string]any)
	return anchor
}

func (st *commentStore) resolveComment(t *testing.T, n int) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	st.comments[n]["resolved_at"] = "2026-08-20T00:00:00Z"
}

// cyrillicDoc is the fixture for AC2, and every offset below is a byte offset
// into it.
//
// It is Cyrillic on purpose and the requirement is not decorative: in ASCII a
// byte index and a character index are the same number, so a test on ASCII text
// is green whether the units are right or wrong. Only two-byte characters make
// the two disagree, and only a disagreement can catch the defect — which is the
// exact way it survived into production in #616.
const cyrillicDoc = "# Документ\n\n" +
	"Первый абзац: обычный текст перед цитатой.\n\n" +
	"Второй абзац содержит фразу про якорь комментария, и она встречается один раз.\n\n" +
	"Третий абзац: повторяющаяся фраза.\n\n" +
	"Четвёртый абзац: повторяющаяся фраза.\n"

// AC1 + AC2: a quote creates a comment anchored to that text, and the proof is a
// BYTE SLICE of the document at the stored offsets — not a 201.
func TestCommentDoc_CyrillicQuoteAnchorsToTheRightBytes(t *testing.T) {
	st := newCommentStore(t, cyrillicDoc)
	const quote = "фразу про якорь комментария"

	text, isErr := callTool(t, st.newServer().handleCommentDoc, map[string]any{
		"doc":   st.docID,
		"body":  "Здесь нужен пример.",
		"quote": quote,
	})
	if isErr {
		t.Fatalf("comment_doc failed: %s", text)
	}

	anchor := st.storedAnchor(0)
	if anchor == nil {
		t.Fatal("the comment was stored with no anchor at all")
	}
	start, end := int(anchor["start"].(float64)), int(anchor["end"].(float64))

	// The assertion that matters: slice the document at the offsets that were
	// actually written down and read what is there.
	got := cyrillicDoc[start:end]
	if got != quote {
		t.Fatalf("anchor points at the wrong text.\n stored offsets: [%d,%d)\n they contain: %q\n expected:     %q",
			start, end, got, quote)
	}

	// The fixture has to be able to TELL the two units apart, or this test would be
	// green under either. Cyrillic is what makes the byte offset and the character
	// offset disagree; assert that they do.
	charStart := len([]rune(cyrillicDoc[:start]))
	if charStart == start {
		t.Fatalf("fixture is not stressing the units: byte offset %d equals character offset %d, "+
			"so this test would pass with either. Use text with multi-byte characters before the quote",
			start, charStart)
	}
}

// AC1, other half: the tool never sends an offset of its own — it sends the one
// the server just computed.
func TestCommentDoc_HasNoOffsetParameter(t *testing.T) {
	st := newCommentStore(t, cyrillicDoc)

	// Offsets passed by a caller must not reach the anchor: they are not arguments
	// this tool has, so they are ignored rather than honoured.
	text, isErr := callTool(t, st.newServer().handleCommentDoc, map[string]any{
		"doc": st.docID, "body": "тест", "quote": "фразу про якорь комментария",
		"start": 1, "end": 2, "offset": 3,
	})
	if isErr {
		t.Fatalf("comment_doc failed: %s", text)
	}
	anchor := st.storedAnchor(0)
	if int(anchor["start"].(float64)) == 1 {
		t.Fatal("a caller-supplied start reached the anchor — the tool must only carry the server's own")
	}
}

// AC3: an ambiguous quote is refused, and the refusal carries the match count.
func TestCommentDoc_AmbiguousQuoteRefusedWithCount(t *testing.T) {
	st := newCommentStore(t, cyrillicDoc)

	text, isErr := callTool(t, st.newServer().handleCommentDoc, map[string]any{
		"doc": st.docID, "body": "какая из двух?", "quote": "повторяющаяся фраза",
	})
	if !isErr {
		t.Fatalf("an ambiguous quote was accepted: %s", text)
	}
	if !strings.Contains(text, "2") {
		t.Errorf("the refusal does not say how many matches there are, which is the one number the "+
			"caller needs: %s", text)
	}
	if !strings.Contains(text, "quote_context") {
		t.Errorf("the refusal does not name the way out: %s", text)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.comments) != 0 {
		t.Errorf("a comment was created despite the refusal — %d stored", len(st.comments))
	}
}

// AC3, the way out: quote_context narrows it, and to the RIGHT one.
func TestCommentDoc_QuoteContextDisambiguates(t *testing.T) {
	st := newCommentStore(t, cyrillicDoc)
	const quote = "повторяющаяся фраза"

	text, isErr := callTool(t, st.newServer().handleCommentDoc, map[string]any{
		"doc": st.docID, "body": "про четвёртый абзац", "quote": quote,
		"quote_context": "Четвёртый абзац: повторяющаяся фраза.\n",
	})
	if isErr {
		t.Fatalf("quote_context did not disambiguate: %s", text)
	}

	anchor := st.storedAnchor(0)
	start := int(anchor["start"].(float64))
	if got := cyrillicDoc[start : start+len(quote)]; got != quote {
		t.Fatalf("offsets do not contain the quote: %q", got)
	}
	// It has to be the SECOND occurrence — landing on the first would be exactly
	// the silent mis-anchor this design exists to prevent.
	if first := strings.Index(cyrillicDoc, quote); start == first {
		t.Fatalf("quote_context pointed at the FIRST occurrence (byte %d); the context names the "+
			"fourth paragraph", start)
	}
}

// A context that does not contain the quote is refused rather than quietly
// dropped: dropping it would leave the caller believing they had disambiguated.
func TestCommentDoc_QuoteContextMustContainTheQuote(t *testing.T) {
	st := newCommentStore(t, cyrillicDoc)

	text, isErr := callTool(t, st.newServer().handleCommentDoc, map[string]any{
		"doc": st.docID, "body": "x", "quote": "повторяющаяся фраза",
		"quote_context": "какой-то другой текст",
	})
	if !isErr {
		t.Fatalf("a context that does not contain the quote was accepted: %s", text)
	}
	if !strings.Contains(text, "quote_context") {
		t.Errorf("refusal does not name the offending argument: %s", text)
	}
}

// AC4: a quote that is not in the document is refused, and nothing is written.
func TestCommentDoc_MissingQuoteCreatesNothing(t *testing.T) {
	st := newCommentStore(t, cyrillicDoc)

	text, isErr := callTool(t, st.newServer().handleCommentDoc, map[string]any{
		"doc": st.docID, "body": "комментарий", "quote": "такой фразы в документе нет",
	})
	if !isErr {
		t.Fatalf("a missing quote was accepted: %s", text)
	}
	if !strings.Contains(text, "NOTHING WAS POSTED") {
		t.Errorf("the refusal does not say that nothing was written, which is the fact a caller acts "+
			"on: %s", text)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.comments) != 0 {
		t.Fatalf("a comment was created for a quote that does not exist — %d stored", len(st.comments))
	}
}

// A comment with no quote is a comment on the whole document, not an error.
func TestCommentDoc_WithoutQuoteIsUnanchored(t *testing.T) {
	st := newCommentStore(t, cyrillicDoc)

	text, isErr := callTool(t, st.newServer().handleCommentDoc, map[string]any{
		"doc": st.docID, "body": "общий комментарий к странице",
	})
	if isErr {
		t.Fatalf("an unanchored comment was refused: %s", text)
	}
	if anchor := st.storedAnchor(0); anchor != nil {
		t.Fatalf("an unanchored comment was given an anchor: %v", anchor)
	}
}

// A reply carries no quote — refused here, with the two ways out named, rather
// than as a 400 from the far end.
func TestCommentDoc_ReplyWithQuoteRefusedLocally(t *testing.T) {
	st := newCommentStore(t, cyrillicDoc)

	text, isErr := callTool(t, st.newServer().handleCommentDoc, map[string]any{
		"doc": st.docID, "body": "ответ", "quote": "фразу про якорь комментария",
		"reply_to": uuid.New().String(),
	})
	if !isErr {
		t.Fatalf("a reply carrying a quote was accepted: %s", text)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.comments) != 0 {
		t.Fatal("the refused reply reached the server")
	}
}

// AC5: replies nest under the comment they answer, and resolved threads are out
// of the way by default.
func TestListDocComments_ThreadsAndResolvedFilter(t *testing.T) {
	st := newCommentStore(t, cyrillicDoc)
	server := st.newServer()

	rootText, _ := callTool(t, server.handleCommentDoc, map[string]any{
		"doc": st.docID, "body": "корневой", "quote": "фразу про якорь комментария",
	})
	var root map[string]any
	if err := json.Unmarshal([]byte(rootText), &root); err != nil {
		t.Fatalf("comment_doc did not return json: %v", err)
	}
	rootID, _ := root["id"].(string)

	if _, isErr := callTool(t, server.handleCommentDoc, map[string]any{
		"doc": st.docID, "body": "ответ", "reply_to": rootID,
	}); isErr {
		t.Fatal("reply was refused")
	}
	if _, isErr := callTool(t, server.handleCommentDoc, map[string]any{
		"doc": st.docID, "body": "второй тред",
	}); isErr {
		t.Fatal("second thread was refused")
	}

	listText, isErr := callTool(t, server.handleListDocComments, map[string]any{"doc": st.docID})
	if isErr {
		t.Fatalf("list_doc_comments failed: %s", listText)
	}
	var listed struct {
		Threads []struct {
			ID      string           `json:"id"`
			Body    string           `json:"body"`
			Replies []map[string]any `json:"replies"`
		} `json:"threads"`
	}
	if err := json.Unmarshal([]byte(listText), &listed); err != nil {
		t.Fatalf("list_doc_comments did not return json: %v", err)
	}

	if len(listed.Threads) != 2 {
		t.Fatalf("expected 2 threads (the reply nested, not top-level), got %d: %s",
			len(listed.Threads), listText)
	}
	if len(listed.Threads[0].Replies) != 1 {
		t.Fatalf("the reply is not nested under its parent: %s", listText)
	}
	if len(listed.Threads[1].Replies) != 0 {
		t.Fatalf("the second thread picked up a reply that is not its own: %s", listText)
	}

	// Resolving the first thread hides it and its reply; the other stays.
	st.resolveComment(t, 0)
	listText, _ = callTool(t, server.handleListDocComments, map[string]any{"doc": st.docID})
	if err := json.Unmarshal([]byte(listText), &listed); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(listed.Threads) != 1 || listed.Threads[0].Body != "второй тред" {
		t.Fatalf("a resolved thread is still in the default listing: %s", listText)
	}
	if !strings.Contains(listText, "include_resolved") {
		t.Error("the listing hides resolved threads without saying so — an empty result then reads " +
			"as 'no comments'")
	}

	listText, _ = callTool(t, server.handleListDocComments,
		map[string]any{"doc": st.docID, "include_resolved": true})
	if err := json.Unmarshal([]byte(listText), &listed); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(listed.Threads) != 2 {
		t.Fatalf("include_resolved did not bring the resolved thread back: %s", listText)
	}
}

// AC5, the orphan half: a comment whose text is gone is SHOWN and MARKED, not
// dropped and not presented as if it still pointed somewhere.
func TestListDocComments_OrphanedAnchorIsShownAndMarked(t *testing.T) {
	st := newCommentStore(t, cyrillicDoc)

	st.mu.Lock()
	st.comments = append(st.comments, map[string]any{
		"id": uuid.New().String(), "document_id": st.docID, "parent_comment_id": nil,
		"author_name": "Ann", "author_type": "user", "body": "про удалённый абзац",
		"resolved_at": nil,
		"anchor": map[string]any{
			"exact": "абзац, которого больше нет", "prefix": "", "suffix": "",
			"start": nil, "end": nil, "orphaned": true,
		},
	})
	st.mu.Unlock()

	text, isErr := callTool(t, st.newServer().handleListDocComments, map[string]any{"doc": st.docID})
	if isErr {
		t.Fatalf("list_doc_comments failed: %s", text)
	}

	var listed struct {
		Threads []struct {
			Anchor map[string]any `json:"anchor"`
		} `json:"threads"`
	}
	if err := json.Unmarshal([]byte(text), &listed); err != nil {
		t.Fatalf("bad json: %v", err)
	}
	if len(listed.Threads) != 1 {
		t.Fatalf("the orphaned comment was dropped from the listing: %s", text)
	}
	if orphaned, _ := listed.Threads[0].Anchor["orphaned"].(bool); !orphaned {
		t.Fatalf("the orphaned comment is presented as if it were still anchored: %s", text)
	}
	if listed.Threads[0].Anchor["start"] != nil {
		t.Fatalf("an orphaned anchor carries offsets, which a client would happily highlight: %s", text)
	}
	if exact, _ := listed.Threads[0].Anchor["exact"].(string); exact == "" {
		t.Error("the orphan lost the quote it was about, which is all that is left of where it pointed")
	}
}

// A thread split across pages must still come back as one thread. The listing is
// ordered by time, so this is the ordinary case on a busy document, not an edge
// one.
func TestListDocComments_WalksEveryPage(t *testing.T) {
	st := newCommentStore(t, cyrillicDoc)
	server := st.newServer()

	rootText, _ := callTool(t, server.handleCommentDoc, map[string]any{
		"doc": st.docID, "body": "корень",
	})
	var root map[string]any
	_ = json.Unmarshal([]byte(rootText), &root)
	rootID, _ := root["id"].(string)

	for i := 0; i < 3; i++ {
		if _, isErr := callTool(t, server.handleCommentDoc, map[string]any{
			"doc": st.docID, "body": "ответ", "reply_to": rootID,
		}); isErr {
			t.Fatal("reply refused")
		}
	}

	// Two comments per page, so the four of them span three pages and the root and
	// its last reply are provably not on the same one. Without this the fake serves
	// everything in one page and the walk is never exercised — the test would pass
	// on a tool that only ever read page 1.
	st.mu.Lock()
	st.pageCap = 2
	st.mu.Unlock()

	text, isErr := callTool(t, server.handleListDocComments, map[string]any{"doc": st.docID})
	if isErr {
		t.Fatalf("list failed: %s", text)
	}
	var listed struct {
		Threads []struct {
			Replies []map[string]any `json:"replies"`
		} `json:"threads"`
	}
	_ = json.Unmarshal([]byte(text), &listed)
	if len(listed.Threads) != 1 || len(listed.Threads[0].Replies) != 3 {
		t.Fatalf("threading lost a reply: %s", text)
	}

	st.mu.Lock()
	asked := st.pageSizeSeen
	st.mu.Unlock()
	if asked != strconv.Itoa(docCommentPageSize) {
		t.Errorf("the walk asked for page_size=%q, not the server maximum %d — more round trips than "+
			"necessary", asked, docCommentPageSize)
	}
}

// AC6, the negative control on the rights boundary.
//
// Asserted against the REGISTERED TOOL SET, not against a handler's behaviour:
// the requirement is that no such tool exists to be called, which a runtime
// refusal would not satisfy — a refusal can be removed by a one-line change and
// a missing tool cannot be invoked at all.
func TestDocCommentTools_NoResolveOrDeleteInTheSurface(t *testing.T) {
	registered := registeredToolNames(t)

	if len(registered) == 0 {
		t.Fatal("no tools were enumerated — this test would pass vacuously")
	}
	// Positive control: the enumeration really sees this file's tools, so an empty
	// or wrongly-filtered set cannot masquerade as "nothing forbidden found".
	for _, want := range []string{"comment_doc", "list_doc_comments"} {
		if !registered[want] {
			t.Fatalf("%s is not registered — the enumeration is not seeing document tools, so the "+
				"absence check below proves nothing", want)
		}
	}

	forbidden := []string{
		"resolve_doc_comment", "unresolve_doc_comment", "resolve_comment",
		"unresolve_comment", "delete_doc_comment", "update_doc_comment",
		"edit_doc_comment", "resolve_thread", "unresolve_thread",
	}
	for _, name := range forbidden {
		if registered[name] {
			t.Errorf("%q is in the tool surface. Marking a discussion resolved is a claim about what "+
				"people agreed, and an agent doing it at scale does damage indistinguishable from "+
				"ordinary activity afterwards. It is meant to be absent, not refused at runtime", name)
		}
	}

	// Nor may it hide as an argument on the tools that do exist.
	for _, name := range []string{"comment_doc", "list_doc_comments"} {
		for _, arg := range toolArgNames(t, name) {
			switch arg {
			case "resolve", "resolved", "unresolve", "set_resolved":
				t.Errorf("%s takes %q — the same power through a different door", name, arg)
			case "start", "end", "offset", "anchor":
				t.Errorf("%s takes %q — an offset an agent computes is the defect this unit exists to "+
					"prevent", name, arg)
			}
		}
	}
}

// registeredToolNames builds a real Server and reads back what it registered.
func registeredToolNames(t *testing.T) map[string]bool {
	t.Helper()
	names := make(map[string]bool)
	for _, tool := range newTestServerTools(t) {
		names[tool.Name] = true
	}
	return names
}

func toolArgNames(t *testing.T, toolName string) []string {
	t.Helper()
	for _, tool := range newTestServerTools(t) {
		if tool.Name != toolName {
			continue
		}
		var args []string
		for name := range tool.InputSchema.Properties {
			args = append(args, name)
		}
		return args
	}
	t.Fatalf("tool %q not registered", toolName)
	return nil
}

var testToolsOnce struct {
	sync.Once
	tools []mcpsdk.Tool
}

// newTestServerTools builds a real full-profile Server and reads back what it
// registered, so the assertion is about the shipped surface rather than about a
// list written down beside it.
func newTestServerTools(t *testing.T) []mcpsdk.Tool {
	t.Helper()
	testToolsOnce.Do(func() {
		s := NewServer(ServerConfig{
			RESTClient: NewRESTClient("http://127.0.0.1:1", "test-key"),
			Session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
			Profile:    ProfileFull,
		})
		for _, st := range s.MCPServer().ListTools() {
			testToolsOnce.tools = append(testToolsOnce.tools, st.Tool)
		}
	})
	return testToolsOnce.tools
}
