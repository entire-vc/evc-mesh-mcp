package mcp

import (
	"context"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
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
	// fileBytes is what the server actually received as the file body. Asserting
	// on it is the only way to tell a decoded PNG from the base64 text of one —
	// the tool result is identical either way.
	fileBytes []byte
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
					cap.fileBytes, _ = io.ReadAll(p)
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

// captureUploadResult is captureUpload without the success assertion: it returns
// the tool result so a caller can assert that an upload was REFUSED and that
// nothing reached the server.
func captureUploadResult(t *testing.T, args map[string]any) (uploadCapture, *mcpsdk.CallToolResult) {
	t.Helper()
	cap := uploadCapture{fields: map[string]string{}}
	reached := false

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !strings.HasSuffix(r.URL.Path, "/artifacts") || r.Method != http.MethodPost {
			_ = json.NewEncoder(w).Encode(map[string]any{})
			return
		}
		reached = true
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
					cap.fileBytes, _ = io.ReadAll(p)
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
	ctx := withTestSession(context.Background(), server, uuid.New())
	result, err := server.handleUploadArtifact(ctx, req)
	if err != nil {
		t.Fatalf("handleUploadArtifact: %v", err)
	}
	cap.fields["__reached"] = map[bool]string{true: "yes", false: "no"}[reached]
	return cap, result
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

// TestUploadArtifact_NoMimeTypeKeepsDefaultPart pins what happens when the caller
// omits mime_type.
//
// Note which branch actually runs: handleUploadArtifact fills mimeType with
// detectMIMEType(name), whose default arm returns "application/octet-stream", so
// fileMime is NEVER empty and the CreatePart path is the only one production takes.
// An earlier version of this comment claimed the opposite, and the test passed anyway
// because it asserted nothing that could tell the two branches apart — which is how a
// header-injection bug lived in the "untaken" branch through a green suite.
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


// TestUploadArtifact_FilenameCannotInjectHeaders is the regression guard for a defect
// introduced while wiring mime_type through: the Content-Disposition was built with
// Sprintf and a hand-written quote escaper that copied two of the standard library's
// four replacements, dropping \r -> %0D and \n -> %0A.
//
// A filename containing CRLF therefore closed the header line early and injected
// arbitrary headers into the part, including a second Content-Type — and Header.Get
// returns the injected one in preference to ours, so an attacker-chosen filename
// controlled the MIME type the server stored.
//
// The assertions are on the PARSED part, not on the raw bytes: what matters is what a
// multipart reader (the server uses the same standard library) makes of it.
func TestUploadArtifact_FilenameCannotInjectHeaders(t *testing.T) {
	hostile := "evil.txt\r\nX-Injected: pwned\r\nContent-Type: text/html"

	got := captureUploadParts(t, map[string]any{
		"task_id":   uuid.New().String(),
		"name":      hostile,
		"content":   "x",
		"mime_type": "image/svg+xml",
	})

	if v := got.header.Get("X-Injected"); v != "" {
		t.Errorf("filename injected a header into the part: X-Injected=%q", v)
	}
	if cts := got.header.Values("Content-Type"); len(cts) != 1 {
		t.Errorf("part carries %d Content-Type values %v — exactly one must survive", len(cts), cts)
	}
	if ct := got.header.Get("Content-Type"); ct != "image/svg+xml" {
		t.Errorf("Content-Type = %q, want the caller's image/svg+xml, not a filename-supplied one", ct)
	}
	// The header must be ONE line — the injected content has to live inside the
	// encoded parameter value, not as separate header lines.
	cd := got.header.Get("Content-Disposition")
	if strings.ContainsAny(cd, "\r\n") {
		t.Errorf("Content-Disposition contains a raw CR/LF, so the value escaped its header: %q", cd)
	}
	if !strings.Contains(cd, "%0D%0A") {
		t.Errorf("expected the CRLF to survive percent-encoded (RFC 2231), got %q", cd)
	}

	// Deliberately NOT asserting an exact filename round-trip here: Part.FileName()
	// applies filepath.Base per RFC 7578 4.2, and this hostile name contains "/" in
	// "text/html", so the reader legitimately returns "html". That is a path-traversal
	// guard doing its job, not truncation by the injection — the exact round-trip is
	// covered by TestUploadArtifact_UnicodeFilenameRoundTrips on a name without a
	// separator. What matters here is that nothing became a header.
}

// TestUploadArtifact_UnicodeFilenameRoundTrips guards the encoding the fix relies on:
// FormatMediaType switches to RFC 2231 (filename*=utf-8'') for non-ASCII, and the
// reader must decode it back.
func TestUploadArtifact_UnicodeFilenameRoundTrips(t *testing.T) {
	name := "отчёт-\"quoted\".md"
	got := captureUploadParts(t, map[string]any{
		"task_id": uuid.New().String(), "name": name, "content": "x", "mime_type": "text/markdown",
	})
	if got.filename != name {
		t.Errorf("filename did not round-trip: got %q, want %q", got.filename, name)
	}
}

type partCapture struct {
	header   textproto.MIMEHeader
	filename string
}

// captureUploadParts returns the parsed file part: its full header set and the
// filename a multipart reader recovers from it.
func captureUploadParts(t *testing.T, args map[string]any) partCapture {
	t.Helper()
	var out partCapture

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/artifacts") && r.Method == http.MethodPost {
			_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
			if err == nil {
				mr := multipart.NewReader(r.Body, params["boundary"])
				for {
					p, perr := mr.NextPart()
					if perr != nil {
						break
					}
					if p.FileName() != "" || p.FormName() == "file" {
						out.header = p.Header
						out.filename = p.FileName()
					}
					_, _ = io.ReadAll(p)
				}
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": uuid.New().String()})
	}))
	t.Cleanup(srv.Close)

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	ctx := withTestSession(context.Background(), server, uuid.New())
	result, err := server.handleUploadArtifact(ctx, req)
	if err != nil {
		t.Fatalf("handleUploadArtifact: %v", err)
	}
	if result.IsError {
		t.Fatalf("handleUploadArtifact error result: %+v", result.Content)
	}
	return out
}

// --- list_tasks: `order` and `page` ------------------------------------------

// These two differ from every other case in this file in one way worth stating:
// `order` and `page` were not advertised-and-discarded, they were absent from the
// tool's schema outright, while the REST layer had honoured both all along. The
// caller-visible damage was the same or worse. A project larger than `limit`
// answered "what changed in the last day" with its OLDEST tasks inside a
// well-formed envelope, so the walk reported "nothing changed" and looked clean;
// and the envelope kept reporting total_pages, advertising pages no caller could
// reach through this tool.
//
// The assertion is on the query string that LEAVES the process, for the reason
// this file's header gives: a result-only check cannot tell forwarded from
// dropped, and both spellings return a perfectly good page either way.

func listTasksQueryCapture(t *testing.T, args map[string]any) string {
	t.Helper()
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/tasks") {
			gotQuery = r.URL.RawQuery
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{}})
	}))
	defer srv.Close()

	server := &Server{restClient: NewRESTClient(srv.URL, "k"), tracker: NewSessionTracker()}
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	if _, err := server.handleListTasks(context.Background(), req); err != nil {
		t.Fatalf("handleListTasks: %v", err)
	}
	return gotQuery
}

func TestListTasks_ForwardsOrder(t *testing.T) {
	q := listTasksQueryCapture(t, map[string]any{
		"project_id": uuid.New().String(),
		"sort":       "updated_at",
		"order":      "desc",
	})
	if !strings.Contains(q, "order=desc") {
		t.Errorf("order was not forwarded; query was %q", q)
	}
	if !strings.Contains(q, "sort_by=updated_at") {
		t.Errorf("sort must still map to sort_by; query was %q", q)
	}
}

// The asc direction is not a redundant twin of the test above: it pins that the
// value is passed through rather than a fixed "desc" being stamped on any
// non-empty order. Without it, a handler that hardcoded desc would pass.
func TestListTasks_ForwardsOrderAsc(t *testing.T) {
	q := listTasksQueryCapture(t, map[string]any{
		"project_id": uuid.New().String(),
		"order":      "asc",
	})
	if !strings.Contains(q, "order=asc") {
		t.Errorf("order=asc was not forwarded verbatim; query was %q", q)
	}
}

func TestListTasks_ForwardsPage(t *testing.T) {
	q := listTasksQueryCapture(t, map[string]any{
		"project_id": uuid.New().String(),
		"page":       2,
	})
	if !strings.Contains(q, "page=2") {
		t.Errorf("page was not forwarded; query was %q", q)
	}
}

// Omission must stay omission. Sending page=1 or order="" unconditionally would
// overwrite a server-side default with a client-side guess, which is how a
// "harmless" default silently becomes policy.
func TestListTasks_OmittedOrderAndPageAreNotSent(t *testing.T) {
	q := listTasksQueryCapture(t, map[string]any{
		"project_id": uuid.New().String(),
	})
	if strings.Contains(q, "order=") {
		t.Errorf("order must not be sent when the caller omitted it; query was %q", q)
	}
	if strings.Contains(q, "page=") {
		t.Errorf("page must not be sent when the caller omitted it; query was %q", q)
	}
}

// The schema is the contract a caller reads before choosing a spelling. A handler
// that forwards a parameter the schema never advertises is still unusable, so pin
// both halves.
func TestListTasks_SchemaAdvertisesOrderAndPage(t *testing.T) {
	server := NewServer(ServerConfig{})
	tool, ok := server.MCPServer().ListTools()["list_tasks"]
	if !ok {
		t.Fatal("list_tasks tool is not registered")
	}
	props := tool.Tool.InputSchema.Properties
	for _, name := range []string{"order", "page"} {
		if _, present := props[name]; !present {
			t.Errorf("list_tasks schema must advertise %q, otherwise no caller can send it", name)
		}
	}
}
