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

func compactTestServer(t *testing.T, handler http.HandlerFunc) *Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
}

func callGetTaskCompact(t *testing.T, s *Server, args map[string]any) map[string]any {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	res, err := s.handleGetTask(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetTask: %v", err)
	}
	var resp map[string]any
	if err := json.Unmarshal([]byte(resultText(t, res)), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

func TestGetTask_DescriptionCappedByDefault_FullVerbatim(t *testing.T) {
	id := uuid.New().String()
	long := strings.Repeat("я", 9000) // multi-byte: cut must be rune-safe
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": "t", "description": long, "updated_at": "2026-10-03T12:00:00Z"})
	})

	task, _ := callGetTaskCompact(t, s, map[string]any{"task_id": id})["task"].(map[string]any)
	d, _ := task["description"].(string)
	if n := len([]rune(d)); n != 2000 {
		t.Errorf("default description = %d runes, want 2000 (wave 2, #dc719c63)", n)
	}
	if task["description_truncated"] != true {
		t.Errorf("description_truncated = %v, want true", task["description_truncated"])
	}
	if c, _ := task["description_chars"].(float64); c != 9000 {
		t.Errorf("description_chars = %v, want 9000", task["description_chars"])
	}
	if h, _ := task["description_hint"].(string); !strings.Contains(h, "full=true") {
		t.Errorf("hint missing full=true: %q", h)
	}

	// positive control: full=true is the verbatim old shape, no markers.
	task, _ = callGetTaskCompact(t, s, map[string]any{"task_id": id, "full": true})["task"].(map[string]any)
	if task["description"] != long {
		t.Error("full=true must return the description verbatim")
	}
	for _, k := range []string{"description_truncated", "description_chars", "description_hint"} {
		if _, has := task[k]; has {
			t.Errorf("full=true must not carry %s", k)
		}
	}
}

func TestGetTask_ShortDescriptionUntouched(t *testing.T) {
	id := uuid.New().String()
	exact := strings.Repeat("a", 2000)
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": "t", "description": exact})
	})
	task, _ := callGetTaskCompact(t, s, map[string]any{"task_id": id})["task"].(map[string]any)
	if task["description"] != exact {
		t.Error("a description of exactly 2000 chars must pass whole")
	}
	if _, has := task["description_truncated"]; has {
		t.Error("no truncation marker on an untouched description")
	}
}

func TestListComments_DefaultLimitTenBodyCutCompact(t *testing.T) {
	var q string
	body := strings.Repeat("x", 20000)
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		q = r.URL.RawQuery
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "c", "body": body}}})
	})
	taskID := uuid.New().String()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID}
	res, err := s.handleListComments(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(q, "page_size=10") || !strings.Contains(q, "sort_dir=desc") {
		t.Errorf("default query = %q, want page_size=10 and sort_dir=desc", q)
	}
	// Wave 2 (#dc719c63): the default view cuts a 20k-char body to 500 + mark;
	// full=true returns it whole.
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal([]byte(resultText(t, res)), &page); err != nil {
		t.Fatal(err)
	}
	got := page.Items[0]["body"].(string)
	if n := len([]rune(got)); n != 500+1 {
		t.Errorf("compact body = %d runes, want 500 + ellipsis", n)
	}
	if page.Items[0]["body_truncated"] != true {
		t.Error("a cut body must carry body_truncated=true")
	}

	reqFull := mcpsdk.CallToolRequest{}
	reqFull.Params.Arguments = map[string]any{"task_id": taskID, "full": true}
	res, err = s.handleListComments(context.Background(), reqFull)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(resultText(t, res), body) {
		t.Error("full=true must return the comment body whole")
	}
}

func TestPollTasks_LeanByDefault_FullRestoresOld(t *testing.T) {
	desc := "first line\n" + strings.Repeat("body ", 500)
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"changed": true, "count": 1, "tasks": []any{
			map[string]any{"id": "t1", "title": "T", "description": desc, "url": "u", "created_by": "x", "position": 1.0},
		}})
	})
	call := func(args map[string]any) map[string]any {
		req := mcpsdk.CallToolRequest{}
		req.Params.Arguments = args
		res, err := s.handlePollTasks(ContextWithSession(context.Background(), &AgentSession{WorkspaceID: uuid.New()}), req)
		if err != nil {
			t.Fatal(err)
		}
		var resp map[string]any
		if err := json.Unmarshal([]byte(resultText(t, res)), &resp); err != nil {
			t.Fatalf("decode %q: %v", resultText(t, res), err)
		}
		return resp
	}
	lean := call(map[string]any{"timeout": 1})["tasks"].([]any)[0].(map[string]any)
	if lean["description"] != "first line" || lean["description_truncated"] != true {
		t.Errorf("lean description = %q trunc=%v", lean["description"], lean["description_truncated"])
	}
	if _, has := lean["url"]; has {
		t.Error("lean view must drop envelope field url")
	}
	full := call(map[string]any{"timeout": 1, "full": true})["tasks"].([]any)[0].(map[string]any)
	if full["description"] != desc || full["url"] != "u" {
		t.Error("full=true must be the verbatim old shape")
	}
}

func TestTeamDirectory_ProjectColumnCappedToOneLine(t *testing.T) {
	zone := "first line zone\n" + strings.Repeat("z", 3000)
	long := strings.Repeat("я", 900)
	full := map[string]any{"workspace": "w", "agents": []any{
		map[string]any{"id": "a1", "name": "A", "role": "dev", "responsibility_zone": zone, "computed_status": "online"},
		map[string]any{"id": "a2", "name": "B", "role": "dev", "responsibility_zone": long, "computed_status": "online"},
	}}
	rows := compactTeamDirectory(full)["agents"].([][]any)
	if rows[0][3] != "first line zone" {
		t.Errorf("zone row0 = %q", rows[0][3])
	}
	if n := len([]rune(rows[1][3].(string))); n != 60 {
		t.Errorf("zone row1 = %d runes, want 60 (wave 2, one line)", n)
	}
}
