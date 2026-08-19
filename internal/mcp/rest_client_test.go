package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestAPIErrorMessage(t *testing.T) {
	tests := []struct {
		name       string
		errBody    map[string]any
		statusCode int
		want       string
	}{
		{
			name: "validation field surfaced",
			errBody: map[string]any{
				"message": "Validation failed",
				"validation": map[string]any{
					"key": "key must match pattern ^[a-z0-9][a-z0-9-]*[a-z0-9]$ (lowercase alphanumeric with hyphens)",
				},
			},
			statusCode: http.StatusBadRequest,
			want:       "Validation failed (key: key must match pattern ^[a-z0-9][a-z0-9-]*[a-z0-9]$ (lowercase alphanumeric with hyphens))",
		},
		{
			name: "multiple validation fields sorted",
			errBody: map[string]any{
				"message": "Validation failed",
				"validation": map[string]any{
					"content": "content is required",
					"key":     "key is required",
				},
			},
			statusCode: http.StatusBadRequest,
			want:       "Validation failed (content: content is required; key: key is required)",
		},
		{
			name: "message only, no validation map",
			errBody: map[string]any{
				"message": "not found",
			},
			statusCode: http.StatusNotFound,
			want:       "not found",
		},
		{
			name: "error field fallback",
			errBody: map[string]any{
				"error": "rule_violation",
			},
			statusCode: http.StatusUnprocessableEntity,
			want:       "rule_violation",
		},
		{
			name:       "empty body falls back to generic message",
			errBody:    map[string]any{},
			statusCode: http.StatusInternalServerError,
			want:       "API error 500",
		},
		{
			name: "empty validation map ignored",
			errBody: map[string]any{
				"message":    "Validation failed",
				"validation": map[string]any{},
			},
			statusCode: http.StatusBadRequest,
			want:       "Validation failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := apiErrorMessage(tt.errBody, tt.statusCode)
			if got != tt.want {
				t.Errorf("apiErrorMessage() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDoJSON_SurfacesValidationDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"code":    400,
			"message": "Validation failed",
			"validation": map[string]any{
				"key": "key must match pattern ^[a-z0-9][a-z0-9-]*[a-z0-9]$ (lowercase alphanumeric with hyphens)",
			},
		})
	}))
	defer srv.Close()

	c := NewRESTClient(srv.URL, "test-key")
	err := c.doJSON(context.Background(), http.MethodPost, "/memories", map[string]string{"key": "foo:bar"}, nil)

	if err == nil {
		t.Fatal("expected an error, got nil")
	}
	if !strings.Contains(err.Error(), "key must match pattern") {
		t.Errorf("error message dropped validation detail: %q", err.Error())
	}
}

// TestRecallWithGraph_SendsScopeAndTagsOnTheWire is the regression test for
// task #37e9344c: RecallWithGraphParams had no Scope/Tags/TagsAny fields at
// all, so the graph-boost arm of recall() ran unscoped no matter what the
// caller asked for — the backend has enforced these since #2c087b2a, but
// nothing on this path sent them.
//
// It inspects the ACTUAL request the client puts on the wire, not just that
// the call returns without error — a field existing on the struct proves
// nothing about what reaches the server (the exact gap this task's own
// diagnosis names: "не 'поле добавлено', а фактически доехавшее значение").
//
// Two tags in both Tags and TagsAny, specifically, to catch the sibling
// class of bug #2c087b2a found on the C4 channel: the recall_graph endpoint
// binds tags/tags_any as plain strings via echo's c.Bind, which keeps only
// the FIRST occurrence of a repeated query param. Sending repeated
// tags_any=a&tags_any=b (what params.Add would produce) silently narrows a
// two-tag filter to one tag with no error — a passing single-tag assertion
// would not have caught that class at all.
func TestRecallWithGraph_SendsScopeAndTagsOnTheWire(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer srv.Close()

	c := NewRESTClient(srv.URL, "test-key")
	_, err := c.RecallWithGraph(context.Background(), RecallWithGraphParams{
		Query:       "test query",
		WorkspaceID: "ws-1",
		Scope:       "workspace",
		Tags:        []string{"kind:decision", "owner:linus"},
		TagsAny:     []string{"pavel-decision", "canonical"},
	})
	if err != nil {
		t.Fatalf("RecallWithGraph returned error: %v", err)
	}

	if got := gotQuery.Get("scope"); got != "workspace" {
		t.Errorf("scope did not reach the server: got %q, want %q", got, "workspace")
	}

	// url.Values.Get returns only the first value for a key — using it here
	// (rather than iterating) is itself part of the assertion: if the client
	// regressed to repeated params, Get would still return "kind:decision"
	// and hide the dropped second tag. The explicit len==1 check below is
	// what actually catches that.
	tagsRaw := gotQuery["tags"]
	if len(tagsRaw) != 1 {
		t.Fatalf("tags sent as %d separate query values (want 1 comma-joined value): %v", len(tagsRaw), tagsRaw)
	}
	if tagsRaw[0] != "kind:decision,owner:linus" {
		t.Errorf("tags value = %q, want %q", tagsRaw[0], "kind:decision,owner:linus")
	}

	tagsAnyRaw := gotQuery["tags_any"]
	if len(tagsAnyRaw) != 1 {
		t.Fatalf("tags_any sent as %d separate query values (want 1 comma-joined value): %v", len(tagsAnyRaw), tagsAnyRaw)
	}
	if tagsAnyRaw[0] != "pavel-decision,canonical" {
		t.Errorf("tags_any value = %q, want %q", tagsAnyRaw[0], "pavel-decision,canonical")
	}
}

// TestRecallWithGraph_OmitsEmptyScopeAndTags confirms the new fields are
// opt-in: a caller with no filters must not start sending empty scope/tags
// params that the server would then have to special-case.
func TestRecallWithGraph_OmitsEmptyScopeAndTags(t *testing.T) {
	var gotQuery url.Values
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer srv.Close()

	c := NewRESTClient(srv.URL, "test-key")
	_, err := c.RecallWithGraph(context.Background(), RecallWithGraphParams{
		Query:       "test query",
		WorkspaceID: "ws-1",
	})
	if err != nil {
		t.Fatalf("RecallWithGraph returned error: %v", err)
	}

	for _, key := range []string{"scope", "tags", "tags_any"} {
		if gotQuery.Has(key) {
			t.Errorf("unset %s sent as an empty query param: %v", key, gotQuery[key])
		}
	}
}

// TestGetTaskDependencies_DecodesOutgoingIncomingShape is the regression
// test for #cf78d1f9: the /tasks/{id}/dependencies endpoint moved from a
// bare JSON array to {"outgoing":[...],"incoming":[...]}, and the old
// []map[string]any decode target failed on the new shape with "cannot
// unmarshal object into Go value of type []map[string]interface {}".
func TestGetTaskDependencies_DecodesOutgoingIncomingShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"outgoing": []map[string]any{
				{"task_id": "t1", "depends_on_task_id": "blocker-1", "dependency_type": "blocks"},
			},
			"incoming": []map[string]any{
				{"task_id": "downstream-1", "depends_on_task_id": "t1", "dependency_type": "blocks"},
			},
		})
	}))
	defer srv.Close()

	c := NewRESTClient(srv.URL, "test-key")
	deps, err := c.GetTaskDependencies(context.Background(), "t1")
	if err != nil {
		t.Fatalf("GetTaskDependencies returned error: %v", err)
	}

	if len(deps.Outgoing) != 1 || deps.Outgoing[0]["depends_on_task_id"] != "blocker-1" {
		t.Errorf("Outgoing not decoded correctly: %+v", deps.Outgoing)
	}
	// Negative control: incoming edges must never leak into Outgoing — mixing
	// them silently changes "this task's own blockers" into "blockers plus
	// downstream work", which would wrongly gate on the wrong direction.
	if len(deps.Incoming) != 1 || deps.Incoming[0]["task_id"] != "downstream-1" {
		t.Errorf("Incoming not decoded correctly: %+v", deps.Incoming)
	}
	for _, edge := range deps.Outgoing {
		if edge["task_id"] == "downstream-1" {
			t.Errorf("incoming edge leaked into Outgoing: %+v", deps.Outgoing)
		}
	}
}
