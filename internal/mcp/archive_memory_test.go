package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

type archiveCall struct {
	path   string
	reason string
}

// archiveTestServer answers by-key with rows and records archive/unarchive calls.
func archiveTestServer(t *testing.T, rows []any) (*Server, *[]archiveCall, func()) {
	t.Helper()
	calls := &[]archiveCall{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/api/v1/memories/by-key" {
			if r.URL.Query().Get("include_archived") != "true" {
				t.Errorf("archive_memory key lookup must include archived rows, got %q", r.URL.RawQuery)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"items": rows, "total": len(rows)})
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(raw, &body)
		*calls = append(*calls, archiveCall{path: r.URL.Path, reason: body.Reason})
		archived := strings.HasSuffix(r.URL.Path, "/archive")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"memory":      map[string]any{"id": "m-1", "key": "k", "archived": archived, "content": strings.Repeat("x", 5000)},
			"archived":    archived,
			"changed":     true,
			"revision_id": "r-1",
		})
	}))
	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
	return server, calls, srv.Close
}

func callArchive(t *testing.T, s *Server, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	result, err := s.handleArchiveMemory(context.Background(), req)
	if err != nil {
		t.Fatalf("handler error: %v", err)
	}
	return result
}

func TestArchiveMemory_ByIDArchivesAndRestores(t *testing.T) {
	s, calls, closeFn := archiveTestServer(t, nil)
	defer closeFn()

	result := callArchive(t, s, map[string]any{"memory_id": "m-1", "reason": "  merged long ago  "})
	if result.IsError {
		t.Fatalf("archive: %v", result.Content)
	}
	out := decodeRecallResult(t, result)
	if out["archived"] != true || out["revision_id"] != "r-1" {
		t.Errorf("result must carry archived and revision_id, got %v", out)
	}
	if m, _ := out["memory"].(map[string]any); m["content"] != nil {
		t.Error("the result names the entry; it must not echo its whole text")
	}

	result = callArchive(t, s, map[string]any{"memory_id": "m-1", "archived": false, "reason": "still needed"})
	if result.IsError {
		t.Fatalf("unarchive: %v", result.Content)
	}
	want := []archiveCall{{"/api/v1/memories/m-1/archive", "merged long ago"}, {"/api/v1/memories/m-1/unarchive", "still needed"}}
	if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Errorf("calls = %+v, want %+v", *calls, want)
	}
}

func TestArchiveMemory_ByKeyResolvesOneEntry(t *testing.T) {
	rows := []any{
		map[string]any{"id": "m-7", "key": "stale-directive", "scope": "project", "archived": false},
		map[string]any{"id": "m-8", "key": "stale-directive-other", "scope": "project"},
	}
	s, calls, closeFn := archiveTestServer(t, rows)
	defer closeFn()
	result := callArchive(t, s, map[string]any{"key": "stale-directive", "reason": "done"})
	if result.IsError {
		t.Fatalf("archive by key: %v", result.Content)
	}
	if len(*calls) != 1 || (*calls)[0].path != "/api/v1/memories/m-7/archive" {
		t.Errorf("calls = %+v", *calls)
	}
}

func TestArchiveMemory_Refusals(t *testing.T) {
	rows := []any{
		map[string]any{"id": "m-1", "key": "dup", "scope": "project", "project_id": "p-1"},
		map[string]any{"id": "m-2", "key": "dup", "scope": "workspace"},
	}
	s, calls, closeFn := archiveTestServer(t, rows)
	defer closeFn()
	for name, tc := range map[string]struct {
		args map[string]any
		want string
	}{
		"no reason":     {map[string]any{"memory_id": "m-1", "reason": "  "}, "reason is required"},
		"no address":    {map[string]any{"reason": "x"}, "memory_id or key is required"},
		"ambiguous key": {map[string]any{"key": "dup", "reason": "x"}, "memory_id=m-2"},
		"unknown key":   {map[string]any{"key": "nope", "reason": "x"}, "no memory with key"},
	} {
		t.Run(name, func(t *testing.T) {
			result := callArchive(t, s, tc.args)
			if !result.IsError {
				t.Fatal("must be refused")
			}
			if text := result.Content[0].(mcpsdk.TextContent).Text; !strings.Contains(text, tc.want) {
				t.Errorf("got %q, want it to mention %q", text, tc.want)
			}
		})
	}
	if len(*calls) != 0 {
		t.Errorf("a refused call must not reach archive/unarchive, got %+v", *calls)
	}
}

func TestArchiveMemory_RegisteredInCoreProfile(t *testing.T) {
	tools := NewServer(ServerConfig{Profile: ProfileCore}).MCPServer().ListTools()
	tool, ok := tools["archive_memory"]
	if !ok {
		t.Fatal("archive_memory is not registered in the core profile")
	}
	if !strings.Contains(strings.Join(tool.Tool.InputSchema.Required, ","), "reason") {
		t.Error("reason must be a required parameter")
	}
}

// The expiry descriptions must tell an agent what expiry really does: hidden at
// once, deleted by the 6-hourly cleanup 7 days later, archive_memory to undo.
func TestExpiryDescriptions_StateCleanupAndArchive(t *testing.T) {
	tools := NewServer(ServerConfig{Profile: ProfileCore}).MCPServer().ListTools()
	for _, ref := range []struct{ tool, param string }{
		{"remember", "expires_at"}, {"recall", "include_expired"}, {"get_memory", "include_expired"},
	} {
		prop, _ := tools[ref.tool].Tool.InputSchema.Properties[ref.param].(map[string]any)
		desc, _ := prop["description"].(string)
		for _, want := range []string{"moment it expires", "every 6 h", "7 days after expiry", "archive_memory"} {
			if !strings.Contains(desc, want) {
				t.Errorf("%s.%s description lacks %q: %q", ref.tool, ref.param, want, desc)
			}
		}
	}
}
