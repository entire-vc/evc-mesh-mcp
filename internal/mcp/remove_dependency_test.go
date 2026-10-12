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

func removeDepRequest(args map[string]any) mcpsdk.CallToolRequest {
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

// The edge lives on the blocked task; removing it must hit
// DELETE /tasks/<blocked>/dependencies/<edge id> and nothing else.
func TestHandleRemoveDependency_DeletesOutgoingEdge(t *testing.T) {
	blocked, blocker, edge := uuid.NewString(), uuid.NewString(), uuid.NewString()
	var deleted string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+blocked:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": blocked})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+blocked+"/dependencies":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"outgoing": []map[string]any{{"id": edge, "task_id": blocked, "depends_on_task_id": blocker}},
				"incoming": []map[string]any{},
			})
		case r.Method == http.MethodDelete:
			deleted = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	res, err := s.handleRemoveDependency(context.Background(), removeDepRequest(map[string]any{
		"task_id": blocked, "depends_on_task_id": blocker, "reason": "stray link holds closing",
	}))
	if err != nil || res.IsError {
		t.Fatalf("remove failed: err=%v res=%+v", err, res)
	}
	if want := "/api/v1/tasks/" + blocked + "/dependencies/" + edge; deleted != want {
		t.Errorf("DELETE path = %q, want %q", deleted, want)
	}
}

// Negative control: the reversed pair has no outgoing edge, so nothing is deleted.
func TestHandleRemoveDependency_ReversedPairDeletesNothing(t *testing.T) {
	blocked, blocker := uuid.NewString(), uuid.NewString()
	deletes := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+blocker:
			_ = json.NewEncoder(w).Encode(map[string]any{"id": blocker})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/"+blocker+"/dependencies":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"outgoing": []map[string]any{},
				"incoming": []map[string]any{{"id": uuid.NewString(), "task_id": blocked, "depends_on_task_id": blocker}},
			})
		case r.Method == http.MethodDelete:
			deletes++
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	s := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	res, _ := s.handleRemoveDependency(context.Background(), removeDepRequest(map[string]any{
		"task_id": blocker, "depends_on_task_id": blocked, "reason": "wrong direction test",
	}))
	if !res.IsError || deletes != 0 {
		t.Fatalf("reversed pair must be refused without DELETE: isError=%v deletes=%d", res.IsError, deletes)
	}
}

func TestHandleRemoveDependency_RequiresReason(t *testing.T) {
	s := &Server{restClient: NewRESTClient("http://127.0.0.1:1", "k"), tracker: NewSessionTracker()}
	res, _ := s.handleRemoveDependency(context.Background(), removeDepRequest(map[string]any{
		"task_id": uuid.NewString(), "depends_on_task_id": uuid.NewString(), "reason": "x",
	}))
	if !res.IsError || !strings.Contains(resultText(t, res), "reason") {
		t.Fatalf("short reason must be refused, got %+v", res)
	}
}

// Ten bytes, five characters: a byte-length check would let this through.
func TestHandleRemoveDependency_ShortMultibyteReasonRefused(t *testing.T) {
	s := &Server{restClient: NewRESTClient("http://127.0.0.1:1", "k"), tracker: NewSessionTracker()}
	res, _ := s.handleRemoveDependency(context.Background(), removeDepRequest(map[string]any{
		"task_id": uuid.NewString(), "depends_on_task_id": uuid.NewString(), "reason": "связь",
	}))
	if !res.IsError || !strings.Contains(resultText(t, res), "reason") {
		t.Fatalf("5-character reason must be refused, got %+v", res)
	}
}
