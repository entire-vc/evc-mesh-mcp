package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// docsFixture is a fake Mesh API serving one project of documents. It records
// which routes were hit, so a test can assert not just what came back but what
// the tool asked for — the difference between "the body was not returned" and
// "the body was never fetched".
type docsFixture struct {
	docs    []map[string]any
	bodies  map[string]string
	hits    []string
	server  *httptest.Server
	projID  string
	handler http.HandlerFunc
}

func newDocsFixture(t *testing.T, projID string, docs []map[string]any, bodies map[string]string) *docsFixture {
	t.Helper()
	f := &docsFixture{docs: docs, bodies: bodies, projID: projID}

	f.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits = append(f.hits, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.URL.Path == "/api/v1/projects/"+projID+"/documents":
			// Metadata only — matching the real API, where a document's body is
			// populated for single-document reads and never for a list.
			items := make([]map[string]any, 0, len(f.docs))
			items = append(items, f.docs...)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"items": items, "total_count": len(items), "page": 1,
				"page_size": 200, "total_pages": 1, "has_more": false,
			})

		case strings.HasSuffix(r.URL.Path, "/outline"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/documents/"), "/outline")
			doc := f.find(id)
			if doc == nil {
				http.NotFound(w, r)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"document_id": id,
				"title":       doc["title"],
				"version":     doc["version"],
				"outline":     outlineOf(f.bodies[id]),
			})

		case strings.HasSuffix(r.URL.Path, "/section"):
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/v1/documents/"), "/section")
			doc := f.find(id)
			if doc == nil {
				http.NotFound(w, r)
				return
			}
			heading := r.URL.Query().Get("heading")
			content, ok := sectionOf(f.bodies[id], heading)
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				_ = json.NewEncoder(w).Encode(map[string]any{"message": "no such heading"})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"document_id": id,
				"version":     doc["version"],
				"heading":     map[string]any{"level": 2, "text": heading, "anchor": heading, "start": 10, "end": 20, "line": 3},
				"content":     content,
			})

		case strings.HasPrefix(r.URL.Path, "/api/v1/documents/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/documents/")
			doc := f.find(id)
			if doc == nil {
				http.NotFound(w, r)
				return
			}
			full := map[string]any{}
			for k, v := range doc {
				full[k] = v
			}
			full["body"] = f.bodies[id]
			full["storage_key"] = "s3://bucket/" + id
			_ = json.NewEncoder(w).Encode(full)

		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(f.server.Close)
	return f
}

func (f *docsFixture) find(id string) map[string]any {
	for _, d := range f.docs {
		if d["id"] == id {
			return d
		}
	}
	return nil
}

// fetchedBody reports whether any request in this fixture's history would have
// caused the API to download a markdown page.
func (f *docsFixture) fetchedBody() bool {
	for _, path := range f.hits {
		if strings.HasPrefix(path, "/api/v1/documents/") &&
			!strings.HasSuffix(path, "/outline") && !strings.HasSuffix(path, "/section") {
			return true
		}
	}
	return false
}

func (f *docsFixture) newServer() *Server {
	return &Server{
		restClient: NewRESTClient(f.server.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
}

// outlineOf produces the heading list the real /outline route would, including
// the byte offsets the tool is expected to drop.
func outlineOf(body string) []map[string]any {
	out := []map[string]any{}
	offset := 0
	for i, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "#") {
			level := len(line) - len(strings.TrimLeft(line, "#"))
			text := strings.TrimSpace(strings.TrimLeft(line, "#"))
			out = append(out, map[string]any{
				"level": level, "text": text,
				"anchor": strings.ToLower(strings.ReplaceAll(text, " ", "-")),
				"line":   i + 1, "start": offset, "end": offset + 400,
			})
		}
		offset += len(line) + 1
	}
	return out
}

func sectionOf(body, heading string) (string, bool) {
	for _, block := range strings.Split(body, "\n## ") {
		if strings.HasPrefix(block, heading+"\n") {
			return "## " + block, true
		}
	}
	return "", false
}

func callDocsTool(t *testing.T, fn func(context.Context, mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error), args map[string]any) map[string]any {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	result, err := fn(context.Background(), req)
	if err != nil {
		t.Fatalf("tool returned error: %v", err)
	}
	text, ok := result.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("content[0] is not TextContent, got %T", result.Content[0])
	}
	if result.IsError {
		t.Fatalf("tool reported an error: %s", text.Text)
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, text.Text)
	}
	out["__raw"] = text.Text
	return out
}

// proseDoc builds an ordinary markdown page: a heading every few paragraphs,
// the shape of real documentation.
func proseDoc(sizeBytes int) string {
	var b strings.Builder
	b.WriteString("# Big Document\n\n")
	para := strings.Repeat("Прод-документ с кириллицей и достаточным объёмом текста. ", 12) + "\n\n"
	for i := 0; b.Len() < sizeBytes; i++ {
		b.WriteString(fmt.Sprintf("## Section %d\n\n", i))
		for j := 0; j < 8 && b.Len() < sizeBytes; j++ {
			b.WriteString(para)
		}
	}
	return b.String()
}

// denseDoc builds the same volume of text cut into many short sections, with
// long headings.
//
// This is deliberately the worst shape for an outline rather than a typical one:
// the cost of a default read scales with the number of headings, so a page of
// many small sections is where an unbounded default stops being cheap. A
// document that is merely long is the easy case.
func denseDoc(sizeBytes int) string {
	var b strings.Builder
	b.WriteString("# Big Document\n\n")
	para := strings.Repeat("Прод-документ с кириллицей и достаточным объёмом текста. ", 6) + "\n\n"
	for i := 0; b.Len() < sizeBytes; i++ {
		b.WriteString(fmt.Sprintf("## Section %d with a reasonably descriptive heading\n\n", i))
		b.WriteString(para)
	}
	return b.String()
}

func standardFixture(t *testing.T) (*docsFixture, string, string) {
	t.Helper()
	projID := uuid.New().String()
	parentID := uuid.New().String()
	childID := uuid.New().String()

	docs := []map[string]any{
		{"id": parentID, "project_id": projID, "parent_id": nil, "slug": "architecture",
			"title": "Architecture", "version": 4, "updated_at": "2026-08-19T21:00:00Z"},
		{"id": childID, "project_id": projID, "parent_id": parentID, "slug": "adr-004",
			"title": "ADR 004", "version": 7, "updated_at": "2026-08-19T22:00:00Z"},
	}
	bodies := map[string]string{
		parentID: "# Architecture\n\n## Overview\n\nSome text.\n",
		childID:  "# ADR 004\n\n## Context\n\nWhy we did it.\n\n## Resolution order\n\nThe answer lives here.\n",
	}
	return newDocsFixture(t, projID, docs, bodies), projID, childID
}

// AC1: a listing carries no document bodies at all.
func TestListDocs_CarriesNoBodies(t *testing.T) {
	f, projID, _ := standardFixture(t)
	// The strongest form of the assertion: the fixture is made to misbehave and
	// return a body on the list route, the way a future server regression would.
	// The tool must strip it regardless, because "the API does not send bodies
	// here" is an assumption that can stop being true silently.
	f.docs[0]["body"] = "# Architecture\n\nA body that should never survive."

	out := callDocsTool(t, f.newServer().handleListDocs, map[string]any{"project_id": projID})

	raw := out["__raw"].(string)
	if strings.Contains(raw, "A body that should never survive") {
		t.Errorf("a document body reached the agent through list_docs:\n%s", raw)
	}
	for _, item := range out["items"].([]any) {
		m := item.(map[string]any)
		if _, present := m["body"]; present {
			t.Errorf("list item carries a body key: %v", m)
		}
		if _, present := m["storage_key"]; present {
			t.Errorf("list item leaks storage_key: %v", m)
		}
	}
	if f.fetchedBody() {
		t.Errorf("list_docs fetched a document body; routes hit: %v", f.hits)
	}
}

// list_docs is a map: the path and the tree shape are what make it navigable.
func TestListDocs_ComputesPathAndHasChildren(t *testing.T) {
	f, projID, childID := standardFixture(t)

	out := callDocsTool(t, f.newServer().handleListDocs, map[string]any{"project_id": projID})

	byID := map[string]map[string]any{}
	for _, item := range out["items"].([]any) {
		m := item.(map[string]any)
		byID[m["id"].(string)] = m
	}
	if got := byID[childID]["path"]; got != "architecture/adr-004" {
		t.Errorf("child path = %q, want architecture/adr-004", got)
	}
	parent := byID[f.docs[0]["id"].(string)]
	if parent["has_children"] != true {
		t.Errorf("parent has_children = %v, want true", parent["has_children"])
	}
	if byID[childID]["has_children"] != false {
		t.Errorf("leaf has_children = %v, want false", byID[childID]["has_children"])
	}
}

// AC2: the default read returns metadata and an outline, and no body.
func TestGetDoc_DefaultReturnsOutlineWithoutBody(t *testing.T) {
	f, _, childID := standardFixture(t)

	out := callDocsTool(t, f.newServer().handleGetDoc, map[string]any{"doc": childID})

	if _, present := out["body"]; present {
		t.Errorf("default get_doc returned a body key: %v", out)
	}
	if strings.Contains(out["__raw"].(string), "The answer lives here") {
		t.Errorf("default get_doc leaked body text:\n%s", out["__raw"])
	}
	if f.fetchedBody() {
		t.Errorf("default get_doc hit the body-fetching route; routes: %v", f.hits)
	}
	headings := out["outline"].([]any)
	if len(headings) != 3 {
		t.Fatalf("outline has %d headings, want 3: %v", len(headings), headings)
	}
	// AC5: version is present, and is the same value the document API reports —
	// it is what update_doc will take as base_version, so a wrong one here is a
	// lost write later.
	if v, ok := out["version"].(float64); !ok || int(v) != 7 {
		t.Errorf("version = %v, want 7", out["version"])
	}
}

// The byte offsets an agent must never act on do not reach it.
func TestGetDoc_OutlineDropsByteOffsets(t *testing.T) {
	f, _, childID := standardFixture(t)

	out := callDocsTool(t, f.newServer().handleGetDoc, map[string]any{"doc": childID})

	for _, h := range out["outline"].([]any) {
		m := h.(map[string]any)
		for _, banned := range []string{"start", "end", "line"} {
			if _, present := m[banned]; present {
				t.Errorf("heading carries %q, which an agent must not compute against: %v", banned, m)
			}
		}
		if _, present := m["size_bytes"]; !present {
			t.Errorf("heading lost its size hint: %v", m)
		}
		if m["anchor"] == nil || m["text"] == nil {
			t.Errorf("heading lost the fields it is addressed by: %v", m)
		}
	}
}

// AC3: asking for a section returns that section and nothing around it.
func TestGetDoc_SectionReturnsOnlyThatSection(t *testing.T) {
	f, _, childID := standardFixture(t)

	out := callDocsTool(t, f.newServer().handleGetDoc,
		map[string]any{"doc": childID, "section": "Resolution order"})

	content, _ := out["content"].(string)
	if !strings.Contains(content, "The answer lives here") {
		t.Errorf("section content missing its own text: %q", content)
	}
	if strings.Contains(content, "Why we did it") {
		t.Errorf("section bled into the neighbouring section: %q", content)
	}
	if v, ok := out["version"].(float64); !ok || int(v) != 7 {
		t.Errorf("section response lost version: %v", out["version"])
	}
}

// AC4: asking for the body by name returns the whole page.
func TestGetDoc_BodyTrueReturnsWholeDocument(t *testing.T) {
	f, _, childID := standardFixture(t)

	out := callDocsTool(t, f.newServer().handleGetDoc,
		map[string]any{"doc": childID, "body": true})

	body, _ := out["body"].(string)
	if !strings.Contains(body, "Why we did it") || !strings.Contains(body, "The answer lives here") {
		t.Errorf("body=true did not return the whole document: %q", body)
	}
	if _, present := out["storage_key"]; present {
		t.Errorf("body=true leaked storage_key: %v", out)
	}
}

// AC6, the negative control that matters: the default response must not grow
// with the document. A test on a small page cannot tell a working default from
// a broken one, so this measures two documents that differ only in the way that
// would expose a broken default.
func TestGetDoc_DefaultStaysSmallOnALargeDocument(t *testing.T) {
	measure := func(t *testing.T, body string) (int, int) {
		t.Helper()
		projID := uuid.New().String()
		docID := uuid.New().String()
		f := newDocsFixture(t, projID,
			[]map[string]any{{"id": docID, "project_id": projID, "parent_id": nil,
				"slug": "big", "title": "Big Document", "version": 2}},
			map[string]string{docID: body})

		out := callDocsTool(t, f.newServer().handleGetDoc, map[string]any{"doc": docID})
		if strings.Contains(out["__raw"].(string), "кириллицей") {
			t.Errorf("default response contains prose from the body")
		}
		return len(out["__raw"].(string)), len(out["outline"].([]any))
	}

	// A 50KB page of ordinary prose — a heading every few paragraphs. This is the
	// case the acceptance criterion names, and it is the one agents will meet.
	t.Run("ordinary 50KB document costs under a kilobyte", func(t *testing.T) {
		body := proseDoc(50 * 1024)
		size, headings := measure(t, body)
		t.Logf("body %d bytes, %d headings -> default response %d bytes (%.2f%% of the body)",
			len(body), headings, size, 100*float64(size)/float64(len(body)))
		if size > 1024 {
			t.Errorf("default response is %d bytes on a %d-byte document, want under 1024", size, len(body))
		}
	})

	// The hard case, and the one that decides whether the default is safe: the
	// same 50KB cut into many short sections. An outline costs per heading, so
	// this is where an unbounded default stops being cheap — it measured 11KB
	// before maxDefaultHeadings existed.
	t.Run("heading-dense document stays bounded", func(t *testing.T) {
		body := denseDoc(50 * 1024)
		size, headings := measure(t, body)
		t.Logf("body %d bytes, %d headings returned -> default response %d bytes (%.1f%% of the body)",
			len(body), headings, size, 100*float64(size)/float64(len(body)))
		if headings > maxDefaultHeadings {
			t.Errorf("returned %d headings, above the %d cap", headings, maxDefaultHeadings)
		}
		if size > 8*1024 {
			t.Errorf("default response is %d bytes; the cap is not bounding it", size)
		}
	})

	// The property the criterion is really protecting, stated directly: the cost
	// of looking must not track the size of the page.
	//
	// Not asserted as exact equality, and the reason is worth stating: the
	// response carries the true heading count, so quadrupling the document moves
	// it by the decimal width of that number — a couple of bytes. The bound below
	// is far under any proportional growth (a tracking default would add ~150KB
	// here) while leaving room for that.
	t.Run("response does not grow with body length", func(t *testing.T) {
		small, _ := measure(t, denseDoc(50*1024))
		large, _ := measure(t, denseDoc(200*1024))
		if growth := large - small; growth > 64 {
			t.Errorf("response grew %d bytes (from %d to %d) when the body quadrupled — the default tracks body size",
				growth, small, large)
		}
	})
}

// version_only is the cheap "did it change?" check before a write.
func TestGetDoc_VersionOnlyOmitsOutline(t *testing.T) {
	f, _, childID := standardFixture(t)

	out := callDocsTool(t, f.newServer().handleGetDoc,
		map[string]any{"doc": childID, "version_only": true})

	if _, present := out["outline"]; present {
		t.Errorf("version_only returned an outline: %v", out)
	}
	if v, ok := out["version"].(float64); !ok || int(v) != 7 {
		t.Errorf("version_only lost the version: %v", out["version"])
	}
}

// A path is how an agent thinks about a document; resolving one must not cost a
// body download.
func TestGetDoc_ResolvesSlugPathWithoutFetchingBodies(t *testing.T) {
	f, projID, childID := standardFixture(t)

	out := callDocsTool(t, f.newServer().handleGetDoc,
		map[string]any{"doc": "architecture/adr-004", "project_id": projID})

	if out["document_id"] != childID {
		t.Errorf("path resolved to %v, want %s", out["document_id"], childID)
	}
	if f.fetchedBody() {
		t.Errorf("path resolution downloaded a body; routes: %v", f.hits)
	}
}

func TestGetDoc_PathErrorsAreActionable(t *testing.T) {
	f, projID, _ := standardFixture(t)
	server := f.newServer()

	t.Run("path without project_id says so", func(t *testing.T) {
		req := mcpsdk.CallToolRequest{}
		req.Params.Arguments = map[string]any{"doc": "architecture/adr-004"}
		result, _ := server.handleGetDoc(context.Background(), req)
		text := result.Content[0].(mcpsdk.TextContent).Text
		if !result.IsError || !strings.Contains(text, "project_id") {
			t.Errorf("expected an error naming project_id, got: %s", text)
		}
	})

	t.Run("missing path is named, not silently empty", func(t *testing.T) {
		req := mcpsdk.CallToolRequest{}
		req.Params.Arguments = map[string]any{"doc": "architecture/nope", "project_id": projID}
		result, _ := server.handleGetDoc(context.Background(), req)
		text := result.Content[0].(mcpsdk.TextContent).Text
		if !result.IsError || !strings.Contains(text, "no document at path") {
			t.Errorf("expected a not-found error, got: %s", text)
		}
	})
}

// AC7: a server ahead of this binary must degrade to "extra data", never to a
// broken tool. On 19.08 a changed /dependencies shape broke get_task fleet-wide
// for exactly this reason.
func TestDocs_UnknownServerFieldsSurviveAndDoNotBreak(t *testing.T) {
	f, projID, childID := standardFixture(t)
	f.docs[1]["future_field"] = "added by a newer server"

	list := callDocsTool(t, f.newServer().handleListDocs, map[string]any{"project_id": projID})
	if !strings.Contains(list["__raw"].(string), "added by a newer server") {
		t.Errorf("list_docs dropped an unknown field instead of passing it through:\n%s", list["__raw"])
	}

	// Same on the read path: an outline gaining a field must not break get_doc.
	f2 := newDocsFixture(t, projID, f.docs, f.bodies)
	f2.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"document_id": childID, "title": "ADR 004", "version": 7,
			"tomorrows_field": "hello",
			"outline": []map[string]any{
				{"level": 2, "text": "Context", "anchor": "context",
					"start": 0, "end": 10, "line": 3, "heading_extra": "unseen"},
			},
		})
	})

	out := callDocsTool(t, f2.newServer().handleGetDoc, map[string]any{"doc": childID})
	if out["tomorrows_field"] != "hello" {
		t.Errorf("get_doc dropped an unknown top-level field: %v", out)
	}
	h := out["outline"].([]any)[0].(map[string]any)
	if h["heading_extra"] != "unseen" {
		t.Errorf("get_doc dropped an unknown heading field: %v", h)
	}
}

// outline_depth surveys a deep document top-down, the way a printed table of
// contents shows chapters before subsections.
func TestGetDoc_OutlineDepthLimitsLevels(t *testing.T) {
	projID := uuid.New().String()
	docID := uuid.New().String()
	body := "# Title\n\n## Chapter A\n\n### Detail A1\n\n#### Deeper\n\n## Chapter B\n\n### Detail B1\n"

	f := newDocsFixture(t, projID,
		[]map[string]any{{"id": docID, "project_id": projID, "parent_id": nil,
			"slug": "deep", "title": "Deep", "version": 1}},
		map[string]string{docID: body})
	server := f.newServer()

	all := callDocsTool(t, server.handleGetDoc, map[string]any{"doc": docID})
	if got := len(all["outline"].([]any)); got != 6 {
		t.Fatalf("default outline has %d headings, want all 6", got)
	}

	shallow := callDocsTool(t, server.handleGetDoc,
		map[string]any{"doc": docID, "outline_depth": "2"})
	headings := shallow["outline"].([]any)
	if len(headings) != 3 {
		t.Errorf("outline_depth=2 returned %d headings, want 3 (# and ## only)", len(headings))
	}
	for _, h := range headings {
		if level, _ := h.(map[string]any)["level"].(float64); level > 2 {
			t.Errorf("outline_depth=2 returned a level-%v heading: %v", level, h)
		}
	}
}
