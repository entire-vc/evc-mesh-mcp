package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

func buildCheckoutRequest(taskID string, ttl int) mcpsdk.CallToolRequest {
	args := map[string]any{"task_id": taskID}
	if ttl > 0 {
		args["ttl_minutes"] = float64(ttl)
	}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

func buildReleaseRequest(taskID string) mcpsdk.CallToolRequest {
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": taskID}
	return req
}

func buildExtendCheckoutRequest(taskID string, ttl int) mcpsdk.CallToolRequest {
	args := map[string]any{"task_id": taskID}
	if ttl > 0 {
		args["ttl_minutes"] = float64(ttl)
	}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

// TestCheckoutRelease_TokenForwardedToAPI verifies that after checkout_task the
// cached token is forwarded in the DELETE body of release_task.
func TestCheckoutRelease_TokenForwardedToAPI(t *testing.T) {
	taskID := uuid.New().String()
	token := uuid.New().String()

	var releasedBody map[string]string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/"+taskID+"/checkout":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"task_id":        taskID,
				"checkout_token": token,
			})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/tasks/"+taskID+"/checkout":
			if err := json.NewDecoder(r.Body).Decode(&releasedBody); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
	}
	ctx := context.Background()

	// checkout
	checkoutResult, err := server.handleCheckoutTask(ctx, buildCheckoutRequest(taskID, 120))
	if err != nil {
		t.Fatalf("handleCheckoutTask returned error: %v", err)
	}
	out := decodeToolResultJSON(t, checkoutResult)
	if out["checkout_token"] != token {
		t.Errorf("checkout result checkout_token = %v, want %s", out["checkout_token"], token)
	}

	// release — must forward the token
	releaseResult, err := server.handleReleaseTask(ctx, buildReleaseRequest(taskID))
	if err != nil {
		t.Fatalf("handleReleaseTask returned error: %v", err)
	}
	releaseOut := decodeToolResultJSON(t, releaseResult)
	if releaseOut["released"] != true {
		t.Errorf("expected released=true, got %v", releaseOut["released"])
	}
	if releasedBody["checkout_token"] != token {
		t.Errorf("API received checkout_token = %q, want %q", releasedBody["checkout_token"], token)
	}

	// token must be cleared from the cache after release
	if _, ok := server.checkouts.Load(taskID); ok {
		t.Error("checkout_token not cleared from cache after release")
	}
}

// TestCheckoutRelease_NoTokenError verifies release_task returns an error when
// there is no cached token (e.g., checkout was in a different session).
func TestCheckoutRelease_NoTokenError(t *testing.T) {
	server := &Server{
		restClient: NewRESTClient("http://unused", "test-key"),
		tracker:    NewSessionTracker(),
	}
	result, err := server.handleReleaseTask(context.Background(), buildReleaseRequest(uuid.New().String()))
	if err != nil {
		t.Fatalf("handleReleaseTask returned Go error: %v", err)
	}
	// The result should be an error result (IsError flag), not a success.
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.IsError {
		t.Errorf("expected IsError=true when no cached token, got false; content: %v", result.Content)
	}
}

// TestExtendCheckout_NoTokenError verifies extend_checkout returns an error
// result (not a panic or a Go error) when there is no cached token for the
// task — e.g. checkout was acquired in a different session, already
// released, or already expired.
func TestExtendCheckout_NoTokenError(t *testing.T) {
	server := &Server{
		restClient: NewRESTClient("http://unused", "test-key"),
		tracker:    NewSessionTracker(),
	}
	result, err := server.handleExtendCheckout(context.Background(), buildExtendCheckoutRequest(uuid.New().String(), 0))
	if err != nil {
		t.Fatalf("handleExtendCheckout returned Go error: %v", err)
	}
	if result == nil {
		t.Fatal("expected non-nil result")
	}
	if !result.IsError {
		t.Errorf("expected IsError=true when no cached token, got false; content: %v", result.Content)
	}
}

// TestExtendCheckout_TokenForwardedAndPreserved verifies that after
// checkout_task, extend_checkout forwards the cached token in the PATCH
// body, and that the token remains cached afterward (the server does not
// rotate it — release_task must still be able to use it).
func TestExtendCheckout_TokenForwardedAndPreserved(t *testing.T) {
	taskID := uuid.New().String()
	token := uuid.New().String()

	var extendBody map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks/"+taskID+"/checkout":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"task_id":        taskID,
				"checkout_token": token,
			})
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/tasks/"+taskID+"/checkout":
			if err := json.NewDecoder(r.Body).Decode(&extendBody); err != nil {
				http.Error(w, "bad body", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"task_id":        taskID,
				"checkout_token": token, // server echoes the same token back, does not rotate it
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
	}
	ctx := context.Background()

	if _, err := server.handleCheckoutTask(ctx, buildCheckoutRequest(taskID, 120)); err != nil {
		t.Fatalf("handleCheckoutTask returned error: %v", err)
	}

	extendResult, err := server.handleExtendCheckout(ctx, buildExtendCheckoutRequest(taskID, 60))
	if err != nil {
		t.Fatalf("handleExtendCheckout returned error: %v", err)
	}
	if extendResult.IsError {
		t.Fatalf("expected success, got error result: %v", extendResult.Content)
	}
	if extendBody["checkout_token"] != token {
		t.Errorf("API received checkout_token = %v, want %q", extendBody["checkout_token"], token)
	}
	if extendBody["ttl_minutes"] != float64(60) {
		t.Errorf("API received ttl_minutes = %v, want 60", extendBody["ttl_minutes"])
	}

	// Token must still be cached — release_task should still work afterward.
	cached, ok := server.checkouts.Load(taskID)
	if !ok || cached.(string) != token {
		t.Errorf("expected checkout_token %q to remain cached after extend, got %v (ok=%v)", token, cached, ok)
	}
}
