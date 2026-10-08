package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestListTasksFilters_RejectsUnknownAndInvalidCategory(t *testing.T) {
	for _, args := range []map[string]any{
		{"project_id": "p", "unexpected": "x"},
		{"project_id": "p", "status_category": "active"},
		{"workspace_id": "w", "search": "none", "status_category": "active"},
		{"project_id": "p", "assignee_id": "bart"},
	} {
		t.Run(strings.Join(mapKeys(args), ","), func(t *testing.T) {
			reached := false
			httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				reached = true
				_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
			}))
			defer httpServer.Close()
			s := &Server{restClient: NewRESTClient(httpServer.URL, "k"), tracker: NewSessionTracker()}
			out, err := s.handleListTasks(context.Background(), requestWith(args))
			if err != nil {
				t.Fatal(err)
			}
			if !out.IsError {
				t.Fatalf("invalid filters succeeded: %s", resultText(t, out))
			}
			if reached {
				t.Fatal("invalid filters reached REST")
			}
			text := resultText(t, out)
			if _, ok := args["unexpected"]; ok && (!strings.Contains(text, "unexpected") || !strings.Contains(text, "project_id") || !strings.Contains(text, "status_category")) {
				t.Fatalf("error lacks supported parameters: %s", text)
			}
			if args["status_category"] == "active" {
				for _, cat := range []string{"backlog", "todo", "in_progress", "review", "done", "cancelled"} {
					if !strings.Contains(text, cat) {
						t.Errorf("missing %s in %s", cat, text)
					}
				}
			}
		})
	}
}

func mapKeys(m map[string]any) []string {
	out := []string{}
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestListTasksFilters_AssigneeAndWorkspaceForwarding(t *testing.T) {
	agent := uuid.New()
	workspace := uuid.New()
	for _, tc := range []struct {
		name string
		args map[string]any
		fail bool
	}{
		{"name", map[string]any{"project_id": "p", "assignee": "bArT"}, false},
		{"uuid", map[string]any{"project_id": "p", "assignee": agent.String()}, false},
		{"me", map[string]any{"project_id": "p", "assignee": "me"}, false},
		{"alias", map[string]any{"project_id": "p", "assignee_id": agent.String()}, false},
		{"human", map[string]any{"project_id": "p", "assignee": "member"}, false},
		{"ambiguous", map[string]any{"project_id": "p", "assignee": "shared"}, true},
		{"unknown", map[string]any{"project_id": "p", "assignee": "missing"}, true},
		{"conflict", map[string]any{"project_id": "p", "assignee": "me", "assignee_id": uuid.NewString()}, true},
		{"workspace", map[string]any{"workspace_id": workspace.String(), "search": "none", "assignee": "Bart", "status_category": "todo", "page": 2}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listed := false
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if strings.HasSuffix(r.URL.Path, "/team") {
					_ = json.NewEncoder(w).Encode(map[string]any{"agents": []any{map[string]any{"id": agent.String(), "name": "Bart"}, map[string]any{"id": agent.String(), "name": "shared"}}, "humans": []any{map[string]any{"id": agent.String(), "name": "Member"}, map[string]any{"id": uuid.NewString(), "name": "shared"}}})
					return
				}
				listed = true
				if r.URL.Query().Get("assignee_id") != agent.String() {
					t.Errorf("assignee filter lost: %s", r.URL)
				}
				if tc.name == "workspace" && (r.URL.Query().Get("status_category") != "todo" || r.URL.Query().Get("page") != "2") {
					t.Errorf("workspace filters lost: %s", r.URL)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}, "total_count": 0})
			}))
			defer srv.Close()
			s := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker(), session: &AgentSession{AgentID: agent, WorkspaceID: workspace}}
			out, err := s.handleListTasks(context.Background(), requestWith(tc.args))
			if err != nil {
				t.Fatal(err)
			}
			if out.IsError != tc.fail {
				t.Fatalf("error=%v expected %v: %s", out.IsError, tc.fail, resultText(t, out))
			}
			if tc.fail && listed {
				t.Fatal("unknown/conflicting assignee reached task list")
			}
			if !tc.fail && !listed {
				t.Fatal("positive filter control never listed")
			}
		})
	}
}

func TestListTasksFilters_CompactRoutingAndDefaultLimit(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/statuses") {
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": "st", "slug": "todo"}})
			return
		}
		if r.URL.Query().Get("page_size") != "20" {
			t.Errorf("default page_size=%s, want20", r.URL.Query().Get("page_size"))
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "id", "title": "T", "project_id": "p", "status_id": "st", "description": "long body", "has_description": true, "description_truncated": true, "priority": "high", "labels": []any{}, "assignee_name": "Bart", "updated_at": "2026-10-07T00:00:00Z", "parent_task_id": "parent", "start_after": "2026-10-08T00:00:00Z"}}, "total_count": 1})
	}))
	defer srv.Close()
	s := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	out := callListTasks(t, s, map[string]any{"project_id": "p"})
	item := listTasksItem(t, out)
	for _, key := range []string{"description", "has_description", "description_truncated", "status_id", "project_id"} {
		if _, ok := item[key]; ok {
			t.Errorf("compact includes %s", key)
		}
	}
	for _, key := range []string{"parent_task_id", "start_after"} {
		if _, ok := item[key]; !ok {
			t.Errorf("routing field lost %s", key)
		}
	}
	if item["status"] != "todo" {
		t.Errorf("status slug=%v", item["status"])
	}
}

func TestListTasksFilters_SchemaParity(t *testing.T) {
	tool := NewServer(ServerConfig{}).MCPServer().ListTools()["list_tasks"]
	props := tool.Tool.InputSchema.Properties
	if len(props) != len(listTasksParameters) {
		t.Fatalf("schema/handler parameter count differs: %d vs %d", len(props), len(listTasksParameters))
	}
	for _, name := range listTasksParameters {
		if _, ok := props[name]; !ok {
			t.Errorf("missing schema parameter %s", name)
		}
	}
}

func TestListTasksFilters_UnresolvedStatusRequiresFull(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/statuses") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "task", "project_id": "p", "status_id": "status"}}})
	}))
	defer srv.Close()
	s := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	result, err := s.handleListTasks(context.Background(), requestWith(map[string]any{"project_id": "p"}))
	if err != nil || !result.IsError || !strings.Contains(resultText(t, result), "full=true") {
		t.Fatalf("unresolved slug should be explicit error: %v %v", result, err)
	}
	item := listTasksItem(t, callListTasks(t, s, map[string]any{"project_id": "p", "full": true}))
	if item["status_id"] != "status" {
		t.Fatalf("full fallback lost status: %v", item)
	}
}

func TestListTasksFilters_WorkspaceStatusUsesTaskAccess(t *testing.T) {
	projectCalls, taskCalls := 0, 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/projects/p/statuses":
			projectCalls++
			http.Error(w, "project access denied", http.StatusForbidden)
		case "/api/v1/tasks/first/statuses":
			taskCalls++
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": "st", "slug": "todo"}})
		case "/api/v1/tasks/second/statuses":
			t.Fatal("status lookup was not cached per project")
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{
				map[string]any{"id": "first", "project_id": "p", "status_id": "st"},
				map[string]any{"id": "second", "project_id": "p", "status_id": "st"},
			}})
		}
	}))
	defer srv.Close()
	s := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	response := callListTasks(t, s, map[string]any{"workspace_id": "w", "search": "none"})
	items := response["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items=%d, want 2", len(items))
	}
	for _, raw := range items {
		item := raw.(map[string]any)
		if item["status"] != "todo" || item["project_id"] != "p" {
			t.Fatalf("workspace status/routing lost: %v", item)
		}
	}
	if projectCalls != 1 || taskCalls != 1 {
		t.Fatalf("project/task status calls=%d/%d, want 1/1", projectCalls, taskCalls)
	}
}
