package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func runtimeStateFixture() map[string]any {
	return map[string]any{"kind": "waiting", "since": "2026-10-10T10:00:00Z", "ref": "pipeline:g/p#7", "lane": "linus", "source": "explicit", "stale": false}
}

func TestListTasks_RuntimeKind_ForwardedToREST(t *testing.T) {
	projectID := uuid.New().String()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("runtime_kind")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer srv.Close()
	s := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	out, err := s.handleListTasks(context.Background(), requestWith(map[string]any{"project_id": projectID, "runtime_kind": "working"}))
	if err != nil || out.IsError {
		t.Fatalf("list failed: %v %v", err, out)
	}
	if got != "working" {
		t.Fatalf("runtime_kind not forwarded: %q", got)
	}
}

func TestListTasks_RuntimeKind_InvalidRejectedBeforeREST(t *testing.T) {
	for _, bad := range []any{"dozing", "", 7} {
		reached := false
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			reached = true
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
		}))
		s := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
		out, err := s.handleListTasks(context.Background(), requestWith(map[string]any{"project_id": "p", "runtime_kind": bad}))
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !out.IsError || reached {
			t.Errorf("runtime_kind=%v: IsError=%v reachedREST=%v", bad, out.IsError, reached)
		}
	}
}

func TestListTasks_Compact_CarriesRuntimeState(t *testing.T) {
	projectID := uuid.New().String()
	fixture := listTasksEnvelopeFixture()
	fixture["items"].([]any)[0].(map[string]any)["runtime_state"] = runtimeStateFixture()
	server := listTasksHarness(t, "/api/v1/projects/"+projectID+"/tasks", fixture)
	item := listTasksItem(t, callListTasks(t, server, map[string]any{"project_id": projectID}))
	if !reflect.DeepEqual(item["runtime_state"], runtimeStateFixture()) {
		t.Fatalf("compact row dropped runtime_state: %v", item)
	}
}

func TestGetTask_Compact_CarriesRuntimeState(t *testing.T) {
	id := uuid.New().String()
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "title": "t", "updated_at": "2026-10-10T12:00:00Z", "runtime_state": runtimeStateFixture()})
	})
	task, _ := callGetTaskCompact(t, s, map[string]any{"task_id": id})["task"].(map[string]any)
	if !reflect.DeepEqual(task["runtime_state"], runtimeStateFixture()) {
		t.Fatalf("get_task dropped runtime_state: %v", task)
	}
}

func TestTaskDeltaStub_CarriesRuntimeState(t *testing.T) {
	stub := taskDeltaStub(map[string]any{"id": "x", "updated_at": "2026-10-10T12:00:00Z", "runtime_state": runtimeStateFixture()})
	if !reflect.DeepEqual(stub["runtime_state"], runtimeStateFixture()) {
		t.Fatalf("unchanged-since stub dropped runtime_state: %v", stub)
	}
}
