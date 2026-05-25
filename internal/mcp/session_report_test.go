package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// buildSessionReportRequest builds a CallToolRequest for session_report with the given params.
func buildSessionReportRequest(model string, tokensIn, tokensOut float64, cost float64) mcpsdk.CallToolRequest {
	args := map[string]any{}
	if model != "" {
		args["model"] = model
	}
	if tokensIn != 0 {
		args["tokens_in"] = tokensIn
	}
	if tokensOut != 0 {
		args["tokens_out"] = tokensOut
	}
	if cost != 0 {
		args["estimated_cost"] = cost
	}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

// decodeToolResultJSON decodes the text content of a CallToolResult as JSON.
func decodeToolResultJSON(t *testing.T, result *mcpsdk.CallToolResult) map[string]any {
	t.Helper()
	if result == nil {
		t.Fatal("result is nil")
	}
	if len(result.Content) == 0 {
		t.Fatal("result has no content")
	}
	text, ok := result.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("content[0] is not TextContent, got %T", result.Content[0])
	}
	var out map[string]any
	if err := json.Unmarshal([]byte(text.Text), &out); err != nil {
		t.Fatalf("unmarshal result JSON: %v (text: %s)", err, text.Text)
	}
	return out
}

func TestHandleSessionReport_PersistsViaRESTClient(t *testing.T) {
	var capturedBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agents/me/sessions/report" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&capturedBody); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": "sess-abc-123",
			"totals": map[string]any{
				"tokens_in":      float64(100),
				"tokens_out":     float64(200),
				"estimated_cost": 0.05,
				"model_used":     "claude-sonnet-4-6",
			},
		})
	}))
	defer srv.Close()

	rc := NewRESTClient(srv.URL, "test-key")
	server := &Server{
		restClient: rc,
		tracker:    NewSessionTracker(),
	}

	req := buildSessionReportRequest("claude-sonnet-4-6", 100, 200, 0.05)
	result, err := server.handleSessionReport(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSessionReport returned error: %v", err)
	}

	out := decodeToolResultJSON(t, result)

	// Verify persistence was signaled.
	if out["persisted"] != true {
		t.Errorf("expected persisted=true, got %v", out["persisted"])
	}
	if out["session_id"] != "sess-abc-123" {
		t.Errorf("expected session_id=sess-abc-123, got %v", out["session_id"])
	}
	if _, ok := out["session_totals"]; !ok {
		t.Error("expected session_totals in response")
	}
	if _, ok := out["persist_error"]; ok {
		t.Errorf("unexpected persist_error: %v", out["persist_error"])
	}

	// Verify the body sent to the REST endpoint.
	if capturedBody == nil {
		t.Fatal("REST endpoint was not called")
	}
	if capturedBody["model"] != "claude-sonnet-4-6" {
		t.Errorf("expected model=claude-sonnet-4-6 in request body, got %v", capturedBody["model"])
	}
	if capturedBody["tokens_in"] != float64(100) {
		t.Errorf("expected tokens_in=100 in request body, got %v", capturedBody["tokens_in"])
	}
	if capturedBody["tokens_out"] != float64(200) {
		t.Errorf("expected tokens_out=200 in request body, got %v", capturedBody["tokens_out"])
	}
	if capturedBody["estimated_cost"] != 0.05 {
		t.Errorf("expected estimated_cost=0.05 in request body, got %v", capturedBody["estimated_cost"])
	}
}

func TestHandleSessionReport_PersistError_NonFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"internal server error"}`, http.StatusInternalServerError)
	}))
	defer srv.Close()

	rc := NewRESTClient(srv.URL, "test-key")
	server := &Server{
		restClient: rc,
		tracker:    NewSessionTracker(),
	}

	req := buildSessionReportRequest("claude-sonnet-4-6", 50, 75, 0.01)
	result, err := server.handleSessionReport(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSessionReport returned error: %v", err)
	}

	out := decodeToolResultJSON(t, result)

	// Stats must still be returned even when persistence fails.
	if _, ok := out["tool_calls"]; !ok {
		t.Error("expected tool_calls in response stats even on persist error")
	}
	if out["persisted"] == true {
		t.Error("persisted should not be true on error")
	}
	if _, ok := out["persist_error"]; !ok {
		t.Error("expected persist_error in response when REST call fails")
	}
}

func TestHandleSessionReport_NotImplemented_NonFatal(t *testing.T) {
	// Simulates a server where sessionRepo is not configured (returns 501).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"session tracking not configured"}`, http.StatusNotImplemented)
	}))
	defer srv.Close()

	rc := NewRESTClient(srv.URL, "test-key")
	server := &Server{
		restClient: rc,
		tracker:    NewSessionTracker(),
	}

	req := buildSessionReportRequest("", 10, 20, 0)
	result, err := server.handleSessionReport(context.Background(), req)
	if err != nil {
		t.Fatalf("handleSessionReport returned error: %v", err)
	}

	out := decodeToolResultJSON(t, result)

	// Tool call must succeed — only persist_error is set.
	if out["persisted"] == true {
		t.Error("persisted should not be true on 501")
	}
	// persist_error is set (not nil).
	if _, ok := out["persist_error"]; !ok {
		t.Error("expected persist_error to be set when server returns 501")
	}
}

func TestRESTClient_ReportSession_SendsCorrectPayload(t *testing.T) {
	var captured reportSessionBody

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"session_id": "s1",
			"totals":     map[string]any{},
		})
	}))
	defer srv.Close()

	rc := NewRESTClient(srv.URL, "key")
	_, err := rc.ReportSession(context.Background(), 111, 222, "opus-4", 1.23)
	if err != nil {
		t.Fatalf("ReportSession error: %v", err)
	}

	if captured.TokensIn != 111 {
		t.Errorf("tokens_in: want 111, got %d", captured.TokensIn)
	}
	if captured.TokensOut != 222 {
		t.Errorf("tokens_out: want 222, got %d", captured.TokensOut)
	}
	if captured.Model != "opus-4" {
		t.Errorf("model: want opus-4, got %s", captured.Model)
	}
	if captured.EstimatedCost != 1.23 {
		t.Errorf("estimated_cost: want 1.23, got %f", captured.EstimatedCost)
	}
}
