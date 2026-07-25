package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
