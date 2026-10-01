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

// Regression coverage for the silent-ignore class: create_task/update_task read
// only status_slug, but a caller that types `status` (the shorter, more natural
// name) used to get a success result while the field was dropped — the card was
// created in the project's default status / updated with the status change
// missing, so the call looked executed and wasn't. move_task already refuses the
// same mistake loudly ("status_slug is required"); these tests pin that
// create_task and update_task do too, that the refusal happens before any REST
// request leaves the process, and that legitimate status_slug calls are
// untouched.

// TestCreateTask_RejectsUnknownStatusParam is a table over the guard's whole
// boundary: the rejected shape, the legacy accepted shape, an empty `status`
// (which counts as omitted, like every other optional string here), and the
// both-parameters corner where status_slug wins and the stray `status` is
// ignored rather than fatal.
func TestCreateTask_RejectsUnknownStatusParam(t *testing.T) {
	projectID := uuid.New().String()
	todoID, doneID := uuid.New().String(), uuid.New().String()

	cases := []struct {
		name    string
		args    map[string]any
		wantErr string // non-empty → expect an error result containing this substring
	}{
		{
			name:    "status without status_slug",
			args:    map[string]any{"project_id": projectID, "title": "A task", "status": "backlog"},
			wantErr: `unknown parameter "status": use status_slug`,
		},
		{
			name: "status_slug as before",
			args: map[string]any{"project_id": projectID, "title": "A task", "status_slug": "todo"},
		},
		{
			name: "empty status counts as omitted",
			args: map[string]any{"project_id": projectID, "title": "A task", "status": ""},
		},
		{
			name: "status_slug wins when both are passed",
			args: map[string]any{"project_id": projectID, "title": "A task", "status": "backlog", "status_slug": "todo"},
		},
		{
			// Pins statusArgPresent's documented claim that a non-string
			// value counts as present — the caller clearly meant something.
			name:    "non-string status counts as passed",
			args:    map[string]any{"project_id": projectID, "title": "A task", "status": 3},
			wantErr: `unknown parameter "status": use status_slug`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &routeRecorder{}
			var createBody map[string]any

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record(r.Method + " " + r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodGet && r.URL.Path == "/api/v1/projects/"+projectID+"/statuses":
					_ = json.NewEncoder(w).Encode(statusFixture(todoID, doneID))
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/projects/"+projectID+"/tasks":
					_ = json.NewDecoder(r.Body).Decode(&createBody)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": uuid.New().String(), "project_id": projectID})
				default:
					http.NotFound(w, r)
				}
			}))
			defer srv.Close()

			server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}
			ctx := withTestSession(context.Background(), server, uuid.New())

			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = tc.args

			result, err := server.handleCreateTask(ctx, req)
			if err != nil {
				t.Fatalf("handleCreateTask returned error: %v", err)
			}

			if tc.wantErr != "" {
				if !result.IsError {
					t.Fatalf("expected an error result, got success: %+v", result.Content)
				}
				if msg := resultText(t, result); !strings.Contains(msg, tc.wantErr) {
					t.Errorf("error message = %q, want it to contain %q", msg, tc.wantErr)
				}
				if len(rec.paths) != 0 {
					t.Errorf("a rejected call must not reach REST at all; paths hit: %v", rec.paths)
				}
				return
			}

			if result.IsError {
				t.Fatalf("unexpected error result: %+v", result.Content)
			}
			if !rec.hit("POST /api/v1/projects/" + projectID + "/tasks") {
				t.Errorf("an accepted call must still create the task; paths hit: %v", rec.paths)
			}
			// The guard must not disturb slug resolution: status_slug still
			// resolves to status_id, and no slug still means API default.
			if _, hasSlug := tc.args["status_slug"]; hasSlug {
				if got := createBody["status_id"]; got != todoID {
					t.Errorf("create body carried status_id %v, want resolved todo id %v", got, todoID)
				}
			} else if _, hasStatusID := createBody["status_id"]; hasStatusID {
				t.Errorf("create body carried status_id %v with no status_slug passed; want the API default", createBody["status_id"])
			}
		})
	}
}

// TestUpdateTask_RejectsUnknownStatusParam covers the second call site. update_task
// has no status_slug parameter at all (status transitions are move_task's job), so
// `status` here is doubly unknown — and doubly tempting for a caller that wants
// todo → in_progress in what looks like the generic field-update tool. The old
// behavior patched the other fields and dropped the status change silently.
func TestUpdateTask_RejectsUnknownStatusParam(t *testing.T) {
	taskID := uuid.New().String()

	cases := []struct {
		name    string
		args    map[string]any
		wantErr string
	}{
		{
			name:    "status alongside a real field",
			args:    map[string]any{"task_id": taskID, "title": "New title", "status": "in_progress"},
			wantErr: `unknown parameter "status"`,
		},
		{
			// The both-spellings hedge an LLM caller plausibly sends. Unlike
			// create_task there is no status_slug here for the stray `status`
			// to lose to — both are unknown, and both were silently dropped.
			name:    "status and status_slug both passed",
			args:    map[string]any{"task_id": taskID, "title": "New title", "status": "in_progress", "status_slug": "in_progress"},
			wantErr: `unknown parameter "status"`,
		},
		{
			name: "plain field update as before",
			args: map[string]any{"task_id": taskID, "title": "New title"},
		},
		{
			name: "empty status counts as omitted",
			args: map[string]any{"task_id": taskID, "title": "New title", "status": ""},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &routeRecorder{}
			var patchBody map[string]any

			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				rec.record(r.Method + " " + r.URL.Path)
				w.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/tasks/"+taskID {
					_ = json.NewDecoder(r.Body).Decode(&patchBody)
					_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID})
					return
				}
				http.NotFound(w, r)
			}))
			defer srv.Close()

			server := &Server{restClient: NewRESTClient(srv.URL, "test-key"), tracker: NewSessionTracker()}

			req := mcpsdk.CallToolRequest{}
			req.Params.Arguments = tc.args

			result, err := server.handleUpdateTask(context.Background(), req)
			if err != nil {
				t.Fatalf("handleUpdateTask returned error: %v", err)
			}

			if tc.wantErr != "" {
				if !result.IsError {
					t.Fatalf("expected an error result, got success: %+v", result.Content)
				}
				if msg := resultText(t, result); !strings.Contains(msg, tc.wantErr) {
					t.Errorf("error message = %q, want it to contain %q", msg, tc.wantErr)
				}
				if len(rec.paths) != 0 {
					t.Errorf("a rejected call must not reach REST at all; paths hit: %v", rec.paths)
				}
				return
			}

			if result.IsError {
				t.Fatalf("unexpected error result: %+v", result.Content)
			}
			if !rec.hit("PATCH /api/v1/tasks/" + taskID) {
				t.Errorf("an accepted call must still patch the task; paths hit: %v", rec.paths)
			}
			if got := patchBody["title"]; got != "New title" {
				t.Errorf("patch body carried title %v, want %q", got, "New title")
			}
		})
	}
}
