package mcp

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// These tests assert what left the process — the query string, the request body, the
// multipart part header — rather than the tool's result. Every parameter fixed here
// was one the schema advertised and the code discarded, and in each case the call
// still returned success. A result-only assertion cannot tell "forwarded" from
// "dropped", which is exactly why these went unnoticed.

// --- get_context: `since` ---------------------------------------------------

// TestGetContext_ForwardsSinceAsDateFrom pins the mapping. The tool advertises
// `since` (RFC3339); the API spells the same filter `date_from`. The handler read
// neither, so a caller narrowing the window silently got the default one back.
func TestGetContext_ForwardsSinceAsDateFrom(t *testing.T) {
	projectID := uuid.New().String()
	var gotQuery string

	// Capture ONLY the events request: handleGetContext also fetches project
	// knowledge afterwards, and a capture that does not discriminate by path records
	// the wrong call.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			gotQuery = r.URL.RawQuery
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{
		"project_id": projectID,
		"since":      "2026-08-01T00:00:00Z",
	}

	if _, err := server.handleGetContext(context.Background(), req); err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}
	if !strings.Contains(gotQuery, "date_from=2026-08-01T00%3A00%3A00Z") {
		t.Errorf("since was not forwarded as date_from; query was %q", gotQuery)
	}
}

// TestGetContext_RejectsMalformedSince keeps the validation at the tool, so a bad
// timestamp fails loudly instead of being silently ignored by the API's filter.
func TestGetContext_RejectsMalformedSince(t *testing.T) {
	reached := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/events") {
			reached = true
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"project_id": uuid.New().String(), "since": "last tuesday"}

	result, err := server.handleGetContext(context.Background(), req)
	if err != nil {
		t.Fatalf("handleGetContext: %v", err)
	}
	if !result.IsError {
		t.Error("a malformed since must be rejected by the tool")
	}
	if reached {
		t.Error("no request should have been sent for a malformed since")
	}
}

// --- update_task: delegation_level / completion_signal ----------------------

func captureUpdateBody(t *testing.T, args map[string]any) map[string]any {
	t.Helper()
	taskID, _ := args["task_id"].(string)
	var body map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPatch && r.URL.Path == "/api/v1/tasks/"+taskID {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": taskID})
	}))
	t.Cleanup(srv.Close)

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	if _, err := server.handleUpdateTask(context.Background(), req); err != nil {
		t.Fatalf("handleUpdateTask: %v", err)
	}
	return body
}

// TestUpdateTask_ForwardsDelegationLevel closes an asymmetry: delegation_level could
// be set at creation but never changed afterwards, so a task's routing was fixed for
// life at the moment it was filed.
func TestUpdateTask_ForwardsDelegationLevel(t *testing.T) {
	body := captureUpdateBody(t, map[string]any{
		"task_id": uuid.New().String(), "delegation_level": "review",
	})
	if got := body["delegation_level"]; got != "review" {
		t.Errorf("delegation_level not forwarded: %v (body %v)", got, body)
	}
}

// TestUpdateTask_ForwardsCompletionSignalFalse uses false deliberately. A presence
// check that only ever tests `true` passes just as well against a handler that
// hardcodes true, and false is the value that actually retracts the signal.
func TestUpdateTask_ForwardsCompletionSignalFalse(t *testing.T) {
	body := captureUpdateBody(t, map[string]any{
		"task_id": uuid.New().String(), "completion_signal": false,
	})
	got, present := body["completion_signal"]
	if !present {
		t.Fatalf("completion_signal absent from body %v — false must be sent, not skipped", body)
	}
	if got != false {
		t.Errorf("completion_signal forwarded as %v, want false", got)
	}
}

// TestUpdateTask_OmittedFieldsAreNotSent guards the inverse: PATCH semantics mean a
// field we invent here would overwrite real data. Only what the caller passed may go.
func TestUpdateTask_OmittedFieldsAreNotSent(t *testing.T) {
	body := captureUpdateBody(t, map[string]any{
		"task_id": uuid.New().String(), "title": "only the title",
	})
	for _, k := range []string{"delegation_level", "completion_signal", "estimated_hours", "labels"} {
		if _, present := body[k]; present {
			t.Errorf("%q was sent although the caller never supplied it: %v", k, body)
		}
	}
}

// --- upload_artifact: metadata / mime_type ----------------------------------

type uploadCapture struct {
	fields   map[string]string
	filePart textproto0
}

// textproto0 is the small slice of the file part we care about.
type textproto0 struct {
	contentType string
	filename    string
}

func captureUpload(t *testing.T, args map[string]any) uploadCapture {
	t.Helper()
	cap := uploadCapture{fields: map[string]string{}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/artifacts") || r.Method != http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err == nil {
			mr := multipart.NewReader(r.Body, params["boundary"])
			for {
				p, perr := mr.NextPart()
				if perr != nil {
					break
				}
				if p.FileName() != "" {
					cap.filePart = textproto0{contentType: p.Header.Get("Content-Type"), filename: p.FileName()}
					_, _ = io.ReadAll(p)
					continue
				}
				b, _ := io.ReadAll(p)
				cap.fields[p.FormName()] = string(b)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": uuid.New().String()})
	}))
	t.Cleanup(srv.Close)

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	// upload_artifact requires an authenticated session; without one the handler
	// returns an error result before any request is made and the capture stays empty.
	ctx := withTestSession(context.Background(), server, uuid.New())
	result, err := server.handleUploadArtifact(ctx, req)
	if err != nil {
		t.Fatalf("handleUploadArtifact: %v", err)
	}
	if result.IsError {
		t.Fatalf("handleUploadArtifact returned an error result: %+v", result.Content)
	}
	return cap
}

// TestUploadArtifact_ForwardsMetadata — the tool declared `metadata` and the handler
// never read it, so every caller's metadata was discarded with a success response.
func TestUploadArtifact_ForwardsMetadata(t *testing.T) {
	got := captureUpload(t, map[string]any{
		"task_id":  uuid.New().String(),
		"name":     "report.md",
		"content":  "hello",
		"metadata": map[string]any{"source": "verifier", "run": "42"},
	})

	raw, present := got.fields["metadata"]
	if !present {
		t.Fatalf("metadata form field absent; fields sent: %v", got.fields)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("metadata is not a JSON object the API can store: %q", raw)
	}
	if decoded["source"] != "verifier" {
		t.Errorf("metadata content wrong: %v", decoded)
	}
}

// TestUploadArtifact_ForwardsMimeTypeOnFilePart — mime_type was a parameter of the
// client method that the client never wrote anywhere. The API takes the stored MIME
// from the file part's Content-Type header, so that header is the only channel.
func TestUploadArtifact_ForwardsMimeTypeOnFilePart(t *testing.T) {
	got := captureUpload(t, map[string]any{
		"task_id":   uuid.New().String(),
		"name":      "diagram.svg",
		"content":   "<svg/>",
		"mime_type": "image/svg+xml",
	})
	if got.filePart.contentType != "image/svg+xml" {
		t.Errorf("file part Content-Type = %q, want image/svg+xml", got.filePart.contentType)
	}
	if got.filePart.filename != "diagram.svg" {
		t.Errorf("filename lost when building the part by hand: %q", got.filePart.filename)
	}
}

// TestUploadArtifact_NoMimeTypeKeepsDefaultPart is the negative control for the part
// rewrite: with no mime_type the hand-built header path must not be taken, and the
// filename must still survive.
func TestUploadArtifact_NoMimeTypeKeepsDefaultPart(t *testing.T) {
	got := captureUpload(t, map[string]any{
		"task_id": uuid.New().String(),
		"name":    "notes.txt",
		"content": "x",
	})
	if _, present := got.fields["metadata"]; present {
		t.Errorf("metadata must not be sent when not supplied: %v", got.fields)
	}
	if got.filePart.filename != "notes.txt" {
		t.Errorf("filename = %q, want notes.txt", got.filePart.filename)
	}
	// Not application/octet-stream: with mime_type omitted the handler fills in
	// detectMIMEType(name), and that value now actually reaches the part header
	// instead of being computed and discarded. The API reads the stored MimeType from
	// this header, so a .txt upload is recorded as text/plain rather than a generic
	// blob — the improvement that forwarding the field buys.
	if ct := got.filePart.contentType; ct != "text/plain" {
		t.Errorf("Content-Type on the default part = %q, want the detected text/plain", ct)
	}
}
