package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// AC1: a phrase that only appears in a document's body is found by search_docs,
// and the result carries a path usable directly with get_doc — not just an id.
func TestSearchDocs_FindsDocumentByBodyPhrase(t *testing.T) {
	f, projID, childID := standardFixture(t)

	out := callDocsTool(t, f.newServer().handleSearchDocs, map[string]any{
		"project_id": projID,
		"query":      "Resolution order",
	})

	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 hit, got %d: %v", len(items), out["__raw"])
	}
	hit := items[0].(map[string]any)
	if hit["id"] != childID {
		t.Errorf("expected hit id %q, got %v", childID, hit["id"])
	}
	if hit["path"] != "architecture/adr-004" {
		t.Errorf("expected path %q, got %v", "architecture/adr-004", hit["path"])
	}
	if _, present := hit["body"]; present {
		t.Errorf("search hit carries a body key — a hit is not a document: %v", hit)
	}
}

// AC2: negative control — a query that matches nothing returns an empty list,
// never an error. A tool that errors on "no results" is unusable for the
// exploratory searches this tool exists for.
func TestSearchDocs_NoMatchReturnsEmptyNotAnError(t *testing.T) {
	f, projID, _ := standardFixture(t)

	out := callDocsTool(t, f.newServer().handleSearchDocs, map[string]any{
		"project_id": projID,
		"query":      "zzz-nonexistent-phrase-not-in-any-document-qqq",
	})

	items, _ := out["items"].([]any)
	if len(items) != 0 {
		t.Errorf("expected 0 hits, got %d: %v", len(items), out["__raw"])
	}
	if count, ok := out["count"].(float64); !ok || count != 0 {
		t.Errorf("expected count=0, got %v", out["count"])
	}
}

// AC3: negative control — cross-project isolation. A phrase that genuinely
// exists, but only in a DIFFERENT project's document, must not be found when
// searching this project. This is the property the tool's own description
// promises ("results never cross project_id") and it is demonstrated here
// against two real projects on one fixture server, not assumed from the
// same-project no-match case above.
func TestSearchDocs_ScopedToProjectOnly(t *testing.T) {
	projA := uuid.New().String()
	projB := uuid.New().String()
	docA := uuid.New().String()
	docB := uuid.New().String()

	byProject := map[string][]map[string]any{
		projA: {{"id": docA, "project_id": projA, "parent_id": nil, "slug": "doc-a", "title": "Doc A", "version": 1}},
		projB: {{"id": docB, "project_id": projB, "parent_id": nil, "slug": "doc-b", "title": "Doc B", "version": 1}},
	}
	bodies := map[string]string{
		docA: "# Doc A\n\nNothing special here.\n",
		docB: "# Doc B\n\nThe unique-marker-only-in-project-b lives here.\n",
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		for proj, docs := range byProject {
			prefix := "/api/v1/projects/" + proj
			switch {
			case r.URL.Path == prefix+"/documents/search":
				q := strings.ToLower(r.URL.Query().Get("q"))
				hits := []map[string]any{}
				for _, d := range docs {
					id, _ := d["id"].(string)
					if q != "" && strings.Contains(strings.ToLower(bodies[id]), q) {
						hits = append(hits, map[string]any{
							"id": id, "project_id": proj, "title": d["title"], "slug": d["slug"],
							"snippet": "...", "snippet_is_match": true, "rank": 0.5,
						})
					}
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"items": hits})
				return
			case r.URL.Path == prefix+"/documents":
				_ = json.NewEncoder(w).Encode(map[string]any{
					"items": docs, "total_count": len(docs), "page": 1,
					"page_size": 200, "total_pages": 1, "has_more": false,
				})
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(server.Close)

	srv := &Server{
		restClient: NewRESTClient(server.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}

	// Sanity: the phrase IS findable in the project it actually belongs to.
	inB := callDocsTool(t, srv.handleSearchDocs, map[string]any{
		"project_id": projB, "query": "unique-marker-only-in-project-b",
	})
	if items, _ := inB["items"].([]any); len(items) != 1 {
		t.Fatalf("sanity check failed: expected 1 hit in project B, got %v", inB["__raw"])
	}

	// The actual assertion: searching project A for project B's phrase finds
	// nothing, even though the phrase is real and indexed.
	inA := callDocsTool(t, srv.handleSearchDocs, map[string]any{
		"project_id": projA, "query": "unique-marker-only-in-project-b",
	})
	if items, _ := inA["items"].([]any); len(items) != 0 {
		t.Errorf("cross-project leak: searching project A found project B's document: %v", inA["__raw"])
	}
}

func TestSearchDocs_RequiresProjectID(t *testing.T) {
	f, _, _ := standardFixture(t)
	srv := f.newServer()

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"query": "anything"}
	result, err := srv.handleSearchDocs(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !result.IsError {
		t.Error("expected an error result when project_id is missing")
	}
}

func TestSearchDocs_RequiresQuery(t *testing.T) {
	f, projID, _ := standardFixture(t)
	srv := f.newServer()

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"project_id": projID}
	result, err := srv.handleSearchDocs(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected transport error: %v", err)
	}
	if !result.IsError {
		t.Error("expected an error result when query is missing")
	}
}
