package mcp

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// uploadMesh is a Mesh API that answers the two-step upload REST contract:
// declare → presigned PUT → confirm, plus the attachment list and delete.
// Its refusals carry the real error shape {code,error_code,message,context}.
type uploadMesh struct {
	mu          sync.Mutex
	srv         *httptest.Server
	uploads     map[string]*fakeUpload
	attachments map[string]map[string]any // id → row (with storage_key, as the API serves it)
	seen        []fenceSeen               // every POST/DELETE to the API
	puts        []http.Header             // headers of every PUT to the storage URL
	plainErr    string                    // answer every confirm with a 409 of this error_code
}

type fakeUpload struct {
	kind, target, name, mime string
	size                     int64
	sha                      string
	expires                  time.Time
	data                     []byte
	received                 bool
	result                   map[string]any
}

const uploadDocLimit = 25 << 20

func newUploadMesh(t *testing.T) *uploadMesh {
	t.Helper()
	m := &uploadMesh{uploads: map[string]*fakeUpload{}, attachments: map[string]map[string]any{}}
	m.srv = httptest.NewServer(m)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *uploadMesh) refuse(w http.ResponseWriter, status int, code, msg string, ctx map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"code": status, "error_code": code, "message": msg, "context": ctx})
}

func (m *uploadMesh) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	m.mu.Lock()
	defer m.mu.Unlock()
	p := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if r.Method == http.MethodPost || r.Method == http.MethodDelete {
		m.seen = append(m.seen, fenceSeen{path: r.URL.Path, header: r.Header.Clone()})
	}
	switch {
	// presigned storage PUT: /s3/uploads/<id>
	case r.Method == http.MethodPut && len(p) == 3 && p[0] == "s3":
		m.puts = append(m.puts, r.Header.Clone())
		if r.Header.Get("Authorization") != "" || r.Header.Get("X-Agent-Key") != "" {
			w.WriteHeader(http.StatusBadRequest) // an extra credential breaks the signature
			return
		}
		u := m.uploads[p[2]]
		if u == nil || r.URL.Query().Get("X-Amz-Signature") == "" {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		u.data, u.received = body, true
		w.WriteHeader(http.StatusOK)

	case r.Method == http.MethodPost && len(p) == 6 && p[2] == "documents" && p[4] == "attachments" && p[5] == "uploads":
		m.declare(w, body, "document_attachment", p[3])
	case r.Method == http.MethodPost && len(p) == 6 && p[2] == "tasks" && p[4] == "artifacts" && p[5] == "uploads":
		m.declare(w, body, "artifact", p[3])

	case r.Method == http.MethodPost && len(p) == 5 && p[2] == "uploads" && p[4] == "confirm":
		m.confirm(w, p[3])

	case r.Method == http.MethodGet && len(p) == 5 && p[2] == "documents" && p[4] == "attachments":
		items := []any{}
		for _, a := range m.attachments {
			if a["document_id"] == p[3] {
				items = append(items, a)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items, "total_count": len(items), "page": 1, "page_size": 50})

	case r.Method == http.MethodDelete && len(p) == 4 && p[2] == "document-attachments":
		if _, ok := m.attachments[p[3]]; !ok {
			m.refuse(w, http.StatusNotFound, "not_found", "Attachment not found", nil)
			return
		}
		delete(m.attachments, p[3])
		w.WriteHeader(http.StatusNoContent)
	default:
		m.refuse(w, http.StatusNotFound, "not_found", "no route "+r.Method+" "+r.URL.Path, nil)
	}
}

func (m *uploadMesh) declare(w http.ResponseWriter, body []byte, kind, target string) {
	var req struct {
		Name, MimeType, SHA256, ArtifactType string
		SizeBytes                            int64
	}
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	req.Name, _ = raw["name"].(string)
	req.MimeType, _ = raw["mime_type"].(string)
	req.SHA256, _ = raw["sha256"].(string)
	if f, ok := raw["size_bytes"].(float64); ok {
		req.SizeBytes = int64(f)
	}
	if req.SizeBytes > uploadDocLimit {
		m.refuse(w, http.StatusUnprocessableEntity, "file_too_large", "the file exceeds the limit", map[string]any{"limit_bytes": uploadDocLimit})
		return
	}
	id := uuid.New().String()
	u := &fakeUpload{kind: kind, target: target, name: req.Name, mime: req.MimeType, size: req.SizeBytes,
		sha: req.SHA256, expires: time.Now().Add(15 * time.Minute).UTC()}
	m.uploads[id] = u
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"upload_id": id, "upload_url": m.srv.URL + "/s3/uploads/" + id + "?X-Amz-Signature=sig",
		"upload_method": "PUT", "upload_headers": map[string]string{}, "expires_at": u.expires, "max_bytes": uploadDocLimit,
	})
}

func (m *uploadMesh) confirm(w http.ResponseWriter, id string) {
	if m.plainErr != "" {
		m.refuse(w, http.StatusConflict, m.plainErr, "another confirm of this upload is in progress; retry when it finishes", nil)
		return
	}
	u := m.uploads[id]
	if u == nil {
		m.refuse(w, http.StatusNotFound, "upload_not_found", "Upload not found", nil)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if u.result != nil {
		_ = json.NewEncoder(w).Encode(u.result)
		return
	}
	if !time.Now().Before(u.expires) {
		m.refuse(w, http.StatusGone, "upload_expired", "the upload expired; declare it again", map[string]any{"expires_at": u.expires})
		return
	}
	if !u.received {
		m.refuse(w, http.StatusConflict, "upload_not_received", "nothing was uploaded to upload_url yet; PUT the file there, then confirm", nil)
		return
	}
	drop := func() { u.data, u.received = nil, false }
	if int64(len(u.data)) != u.size {
		n := len(u.data)
		drop()
		m.refuse(w, http.StatusUnprocessableEntity, "size_mismatch", "the uploaded file is not the declared size", map[string]any{"declared": u.size, "received": n})
		return
	}
	sum := sha256.Sum256(u.data)
	if got := hex.EncodeToString(sum[:]); got != u.sha {
		drop()
		m.refuse(w, http.StatusUnprocessableEntity, "checksum_mismatch", "the uploaded file does not match the declared sha256; PUT it again and confirm", map[string]any{"declared": u.sha, "received": got})
		return
	}
	if sniffed := map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "application/pdf": true}; sniffed[u.mime] {
		if detected := strings.SplitN(http.DetectContentType(u.data), ";", 2)[0]; detected != u.mime {
			drop()
			m.refuse(w, http.StatusUnprocessableEntity, "mime_mismatch", "the uploaded file is not of the declared type", map[string]any{"declared": u.mime, "detected": detected})
			return
		}
	}
	rowID := uuid.New().String()
	inline := map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true, "application/pdf": true}[u.mime]
	res := map[string]any{"kind": u.kind, "name": u.name, "mime_type": u.mime, "size_bytes": u.size, "sha256": u.sha, "renders_inline": inline}
	if u.kind == "document_attachment" {
		path := "/api/v1/document-attachments/" + rowID + "/download?disposition=inline"
		res["attachment_id"], res["document_id"], res["download_path"] = rowID, u.target, path
		res["markdown"] = attachmentMarkdown(u.name, u.mime, path)
		m.attachments[rowID] = map[string]any{"id": rowID, "document_id": u.target, "name": u.name, "mime_type": u.mime,
			"size_bytes": u.size, "storage_key": "documents/x/" + rowID, "created_at": time.Now().UTC()}
	} else {
		path := "/api/v1/artifacts/" + rowID + "/download?disposition=inline"
		res["artifact_id"], res["task_id"], res["download_path"] = rowID, u.target, path
		res["markdown"] = attachmentMarkdown(u.name, u.mime, path)
	}
	u.result = res
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(res)
}

func (m *uploadMesh) expire(t *testing.T) {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.uploads {
		u.expires = time.Now().Add(-time.Minute)
	}
}

const uploadTestKey = "agk_upload_secret_key"

func (m *uploadMesh) server() *Server {
	return &Server{
		restClient: NewRESTClient(m.srv.URL, uploadTestKey),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
}

// pngBytes is a PNG signature followed by filler: enough for content sniffing.
func pngBytes(n int) []byte {
	b := bytes.Repeat([]byte{0x42}, n)
	copy(b, []byte("\x89PNG\r\n\x1a\n"))
	return b
}

func sumHex(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }

func decodeResult(t *testing.T, text string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("result is not JSON: %v\n%s", err, text)
	}
	return out
}

// putBytes is step 2 as an agent runs it: a bare PUT, no Authorization.
func putBytes(t *testing.T, url string, data []byte) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPut, url, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	return resp.StatusCode
}

func declareDoc(t *testing.T, s *Server, docID, name, mime string, data []byte) map[string]any {
	t.Helper()
	text, isErr := callTool(t, s.handleCreateDocAttachmentUpload, map[string]any{
		"document_id": docID, "name": name, "mime_type": mime, "size_bytes": float64(len(data)), "sha256": sumHex(data)})
	if isErr {
		t.Fatalf("create_doc_attachment_upload refused: %s", text)
	}
	return decodeResult(t, text)
}

// AC1: create_doc_attachment_upload → PUT → confirm → markdown with the
// stable download_path; list shows it; delete removes it; list is empty again.
func TestUpload_DocumentAttachmentFullCycle(t *testing.T) {
	m := newUploadMesh(t)
	s := m.server()
	docID := uuid.New().String()
	data := pngBytes(500_000)

	text, isErr := callTool(t, s.handleCreateDocAttachmentUpload, map[string]any{
		"document_id": docID, "name": "shot.png", "mime_type": "image/png", "size_bytes": float64(len(data)), "sha256": sumHex(data)})
	if isErr {
		t.Fatalf("create refused: %s", text)
	}
	if strings.Contains(text, uploadTestKey) {
		t.Fatalf("the agent key leaked into the create answer: %s", text)
	}
	created := decodeResult(t, text)
	url, _ := created["upload_url"].(string)
	id, _ := created["upload_id"].(string)
	step, _ := created["next_step"].(string)
	if url == "" || id == "" || created["expires_at"] == nil {
		t.Fatalf("create lacks upload_url/upload_id/expires_at: %v", created)
	}
	for _, want := range []string{"PUT the raw file bytes to upload_url", "no Authorization", "Content-Length must equal size_bytes",
		"curl -sS -T <file> '" + url + "'", "confirm_upload(upload_id=\"" + id + "\")"} {
		if !strings.Contains(step, want) {
			t.Errorf("next_step lacks %q: %s", want, step)
		}
	}
	if got := putBytes(t, url, data); got != http.StatusOK {
		t.Fatalf("bare PUT to upload_url = %d", got)
	}

	text, isErr = callTool(t, s.handleConfirmUpload, map[string]any{"upload_id": id})
	if isErr {
		t.Fatalf("confirm refused: %s", text)
	}
	conf := decodeResult(t, text)
	attID, _ := conf["attachment_id"].(string)
	path := "/api/v1/document-attachments/" + attID + "/download?disposition=inline"
	if attID == "" || conf["download_path"] != path || conf["markdown"] != "![shot.png]("+path+")" || conf["renders_inline"] != true {
		t.Fatalf("confirm answer: %v", conf)
	}
	hint, _ := conf["how_to_use"].(string)
	if !strings.Contains(hint, "Put `markdown` into the document body as is") || !strings.Contains(hint, "never paste upload_url or a download URL") {
		t.Errorf("how_to_use: %q", hint)
	}
	if strings.Contains(hint, "always downloaded") {
		t.Errorf("a png renders inline, but how_to_use says otherwise: %q", hint)
	}

	// A repeated confirm answers the same row.
	text, isErr = callTool(t, s.handleConfirmUpload, map[string]any{"upload_id": id})
	if isErr || decodeResult(t, text)["attachment_id"] != attID {
		t.Fatalf("repeat confirm: isErr=%v %s", isErr, text)
	}

	text, isErr = callTool(t, s.handleListDocAttachments, map[string]any{"document_id": docID})
	if isErr {
		t.Fatalf("list refused: %s", text)
	}
	items, _ := decodeResult(t, text)["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("list after confirm: %s", text)
	}
	row := items[0].(map[string]any)
	if row["id"] != attID || row["markdown"] != "![shot.png]("+path+")" || row["download_path"] != path {
		t.Errorf("list row: %v", row)
	}
	if _, leaked := row["storage_key"]; leaked {
		t.Errorf("list row carries the internal storage_key: %v", row)
	}

	text, isErr = callTool(t, s.handleDeleteDocAttachment, map[string]any{"attachment_id": attID})
	if isErr || decodeResult(t, text)["deleted"] != true {
		t.Fatalf("delete: isErr=%v %s", isErr, text)
	}
	text, _ = callTool(t, s.handleListDocAttachments, map[string]any{"document_id": docID})
	if items, _ := decodeResult(t, text)["items"].([]any); len(items) != 0 {
		t.Fatalf("list after delete is not empty: %s", text)
	}
	// Deleting it again is a plain refusal, not a crash.
	if text, isErr = callTool(t, s.handleDeleteDocAttachment, map[string]any{"attachment_id": attID}); !isErr || !strings.Contains(text, "not found") {
		t.Fatalf("second delete: isErr=%v %s", isErr, text)
	}

	// The PUT the agent ran carried no credential of its own.
	for _, h := range m.puts {
		if h.Get("Authorization") != "" || h.Get("X-Agent-Key") != "" {
			t.Errorf("PUT carried a credential: %v", h)
		}
	}
}

// A type that is never shown inline says so.
func TestUpload_NonInlineTypeSaysDownloaded(t *testing.T) {
	m := newUploadMesh(t)
	s := m.server()
	data := []byte("PK\x03\x04 archive bytes")
	created := declareDoc(t, s, uuid.New().String(), "logs.zip", "application/zip", data)
	putBytes(t, created["upload_url"].(string), data)
	text, isErr := callTool(t, s.handleConfirmUpload, map[string]any{"upload_id": created["upload_id"]})
	if isErr {
		t.Fatalf("confirm refused: %s", text)
	}
	conf := decodeResult(t, text)
	if conf["renders_inline"] != false || !strings.HasPrefix(conf["markdown"].(string), "[logs.zip](") {
		t.Fatalf("zip answer: %v", conf)
	}
	if hint, _ := conf["how_to_use"].(string); !strings.Contains(hint, "This type is always downloaded, not shown inline.") {
		t.Fatalf("how_to_use lacks the not-inline line: %q", hint)
	}
}

// AC1: create_artifact_upload and its confirm send the card's checkout fence,
// like upload_artifact; a document upload sends none.
func TestUpload_ArtifactCarriesFenceOnCreateAndConfirm(t *testing.T) {
	e := newFenceEnv(t)
	m := newUploadMesh(t)
	e.server.restClient = NewRESTClient(m.srv.URL, uploadTestKey)
	token := uuid.New().String()
	e.publishToken(t, token, 3)
	data := pngBytes(3_000_000)

	text, isErr := e.server.handleCreateArtifactUpload(e.ctx, call(map[string]any{
		"task_id": e.taskID, "name": "shot.png", "mime_type": "image/png", "size_bytes": float64(len(data)), "sha256": sumHex(data), "artifact_type": "image"}))
	if isErr != nil || text.IsError {
		t.Fatalf("create_artifact_upload: %v %v", isErr, text.Content)
	}
	created := decodeResult(t, text.Content[0].(mcpsdk.TextContent).Text)
	if got := putBytes(t, created["upload_url"].(string), data); got != http.StatusOK {
		t.Fatalf("PUT = %d", got)
	}
	// No task_id on confirm: the server remembers which card the upload is for.
	res, _ := e.server.handleConfirmUpload(e.ctx, call(map[string]any{"upload_id": created["upload_id"]}))
	if res.IsError {
		t.Fatalf("confirm refused: %v", res.Content)
	}
	conf := decodeResult(t, res.Content[0].(mcpsdk.TextContent).Text)
	if conf["task_id"] != e.taskID || !strings.HasPrefix(conf["download_path"].(string), "/api/v1/artifacts/") {
		t.Fatalf("artifact confirm answer: %v", conf)
	}

	// The document upload of the same agent goes unfenced.
	doc := pngBytes(1000)
	dc, _ := e.server.handleCreateDocAttachmentUpload(e.ctx, call(map[string]any{
		"document_id": uuid.New().String(), "name": "d.png", "mime_type": "image/png", "size_bytes": float64(len(doc)), "sha256": sumHex(doc)}))
	dcm := decodeResult(t, dc.Content[0].(mcpsdk.TextContent).Text)
	putBytes(t, dcm["upload_url"].(string), doc)
	if r, _ := e.server.handleConfirmUpload(e.ctx, call(map[string]any{"upload_id": dcm["upload_id"]})); r.IsError {
		t.Fatalf("doc confirm refused: %v", r.Content)
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.seen) != 4 {
		t.Fatalf("want 4 API writes, got %d: %v", len(m.seen), m.seen)
	}
	for i, w := range m.seen {
		fenced := i < 2 // artifact create + confirm
		got := w.header.Get(fenceTokenHeader)
		if fenced && got != token {
			t.Errorf("%s: %s=%q, want the card's token", w.path, fenceTokenHeader, got)
		}
		if !fenced && got != "" {
			t.Errorf("%s: a document upload sent the fence %q", w.path, got)
		}
		if w.header.Get(fenceRevisionHeader) != "" {
			t.Errorf("%s sent %s; the revision goes only on finish", w.path, fenceRevisionHeader)
		}
	}
}

// AC1: every API refusal code is a tool error (isError) that names the code
// and the next step — never a dropped call and never the raw JSON body.
func TestUpload_RefusalCodesAreToolErrorsWithNextStep(t *testing.T) {
	data := pngBytes(2048)
	cases := []struct {
		code  string
		run   func(t *testing.T, m *uploadMesh, s *Server) (string, bool)
		wants []string
	}{
		{"file_too_large", func(t *testing.T, _ *uploadMesh, s *Server) (string, bool) {
			return callTool(t, s.handleCreateDocAttachmentUpload, map[string]any{
				"document_id": uuid.New().String(), "name": "big.bin", "mime_type": "application/octet-stream",
				"size_bytes": float64(uploadDocLimit + 1), "sha256": strings.Repeat("a", 64)})
		}, []string{"limit of 26214400 bytes", "nothing was stored"}},
		{"size_mismatch", func(t *testing.T, _ *uploadMesh, s *Server) (string, bool) {
			c := declareDoc(t, s, uuid.New().String(), "a.png", "image/png", data)
			putBytes(t, c["upload_url"].(string), data[:1000])
			return callTool(t, s.handleConfirmUpload, map[string]any{"upload_id": c["upload_id"]})
		}, []string{"received 1000 bytes", "declared 2048", "PUT the exact file again"}},
		{"checksum_mismatch", func(t *testing.T, _ *uploadMesh, s *Server) (string, bool) {
			c := declareDoc(t, s, uuid.New().String(), "a.png", "image/png", data)
			bad := append([]byte(nil), data...)
			bad[100] ^= 0xff
			putBytes(t, c["upload_url"].(string), bad)
			return callTool(t, s.handleConfirmUpload, map[string]any{"upload_id": c["upload_id"]})
		}, []string{"declared " + sumHex(data), "PUT the file again to the same upload_url", "confirm_upload again"}},
		{"upload_not_received", func(t *testing.T, _ *uploadMesh, s *Server) (string, bool) {
			c := declareDoc(t, s, uuid.New().String(), "a.png", "image/png", data)
			return callTool(t, s.handleConfirmUpload, map[string]any{"upload_id": c["upload_id"]})
		}, []string{"nothing has been PUT to upload_url yet", "curl -sS -T <file>"}},
		{"upload_expired", func(t *testing.T, m *uploadMesh, s *Server) (string, bool) {
			c := declareDoc(t, s, uuid.New().String(), "a.png", "image/png", data)
			putBytes(t, c["upload_url"].(string), data)
			m.expire(t)
			return callTool(t, s.handleConfirmUpload, map[string]any{"upload_id": c["upload_id"]})
		}, []string{"expired at", "Declare it again"}},
		{"mime_mismatch", func(t *testing.T, _ *uploadMesh, s *Server) (string, bool) {
			txt := []byte("plain text, not a png at all")
			c := declareDoc(t, s, uuid.New().String(), "a.png", "image/png", txt)
			putBytes(t, c["upload_url"].(string), txt)
			return callTool(t, s.handleConfirmUpload, map[string]any{"upload_id": c["upload_id"]})
		}, []string{"first bytes are text/plain", "not the declared image/png", "real mime_type"}},
		{"upload_not_found", func(t *testing.T, _ *uploadMesh, s *Server) (string, bool) {
			return callTool(t, s.handleConfirmUpload, map[string]any{"upload_id": uuid.New().String()})
		}, []string{"no such upload_id"}},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			m := newUploadMesh(t)
			text, isErr := tc.run(t, m, m.server())
			if !isErr {
				t.Fatalf("not a tool error: %s", text)
			}
			if !strings.HasPrefix(text, tc.code+": ") {
				t.Errorf("does not lead with the code %q: %s", tc.code, text)
			}
			for _, w := range tc.wants {
				if !strings.Contains(text, w) {
					t.Errorf("lacks %q: %s", w, text)
				}
			}
			if strings.Contains(text, `{"`) || strings.Contains(text, "error_code") {
				t.Errorf("raw JSON in the tool error: %s", text)
			}
		})
	}
}

// After checksum_mismatch the session stays open: a correct PUT and a second
// confirm succeed, as the refusal text promises.
func TestUpload_ChecksumMismatchThenRetrySucceeds(t *testing.T) {
	m := newUploadMesh(t)
	s := m.server()
	data := pngBytes(4096)
	c := declareDoc(t, s, uuid.New().String(), "a.png", "image/png", data)
	bad := append([]byte(nil), data...)
	bad[10] ^= 0x01
	putBytes(t, c["upload_url"].(string), bad)
	if text, isErr := callTool(t, s.handleConfirmUpload, map[string]any{"upload_id": c["upload_id"]}); !isErr || !strings.HasPrefix(text, "checksum_mismatch: ") {
		t.Fatalf("corrupted PUT confirmed: %s", text)
	}
	putBytes(t, c["upload_url"].(string), data)
	if text, isErr := callTool(t, s.handleConfirmUpload, map[string]any{"upload_id": c["upload_id"]}); isErr {
		t.Fatalf("retry refused: %s", text)
	}
}

// A refusal without an upload code (a plain conflict) keeps the API's own text
// and is not dressed up as one of the upload codes.
func TestUpload_PlainConflictKeepsGenericText(t *testing.T) {
	m := newUploadMesh(t)
	m.plainErr = "conflict"
	text, isErr := callTool(t, m.server().handleConfirmUpload, map[string]any{"upload_id": uuid.New().String()})
	if !isErr || !strings.HasPrefix(text, "confirm_upload failed: Conflict: another confirm of this upload is in progress") {
		t.Fatalf("plain conflict: isErr=%v %s", isErr, text)
	}
}

// Malformed declarations are refused before any request leaves the server.
func TestUpload_DeclarationCheckedLocally(t *testing.T) {
	m := newUploadMesh(t)
	s := m.server()
	base := func() map[string]any {
		return map[string]any{"document_id": uuid.New().String(), "name": "a.png", "mime_type": "image/png", "size_bytes": float64(10), "sha256": strings.Repeat("0", 64)}
	}
	for field, bad := range map[string]any{"sha256": "abc", "size_bytes": float64(0), "name": "", "mime_type": "", "document_id": ""} {
		args := base()
		args[field] = bad
		if text, isErr := callTool(t, s.handleCreateDocAttachmentUpload, args); !isErr || !strings.Contains(text, field) {
			t.Errorf("%s=%v: isErr=%v %s", field, bad, isErr, text)
		}
	}
	if text, isErr := callTool(t, s.handleCreateArtifactUpload, map[string]any{"name": "a", "mime_type": "image/png", "size_bytes": float64(1), "sha256": strings.Repeat("0", 64)}); !isErr || !strings.Contains(text, "task_id") {
		t.Errorf("artifact without task_id: %s", text)
	}
	if len(m.seen) != 0 {
		t.Fatalf("a malformed declaration reached the API: %v", m.seen)
	}
}

// Over SSE upload_artifact cannot read a file; its description and its
// file_path refusal both point at the two-step upload.
func TestUploadArtifact_PointsSSEBinariesAtTwoStepUpload(t *testing.T) {
	if !strings.Contains(errFilePathNotLocal.Error(), "create_artifact_upload") {
		t.Errorf("file_path refusal does not name create_artifact_upload: %v", errFilePathNotLocal)
	}
	srv := NewServer(ServerConfig{Profile: ProfileFull, SetupHint: "x"}).MCPServer()
	tool := srv.GetTool("upload_artifact")
	if tool == nil || !strings.Contains(tool.Tool.Description, "over SSE use create_artifact_upload") {
		t.Fatalf("upload_artifact description does not point SSE binaries at create_artifact_upload")
	}
	for _, name := range []string{toolCreateDocAttachmentUpload, toolCreateArtifactUpload, toolConfirmUpload, toolListDocAttachments, toolDeleteDocAttachment} {
		if srv.GetTool(name) == nil {
			t.Errorf("%s is not registered in the full profile", name)
		}
	}
}

// confirmFenceEnv declares and PUTs an artifact upload for the fenced card and
// returns its upload_id with the card's token.
func confirmFenceEnv(t *testing.T) (*fenceEnv, *uploadMesh, string, string) {
	t.Helper()
	e := newFenceEnv(t)
	m := newUploadMesh(t)
	e.server.restClient = NewRESTClient(m.srv.URL, uploadTestKey)
	token := uuid.New().String()
	e.publishToken(t, token, 3)
	data := pngBytes(4096)
	res, _ := e.server.handleCreateArtifactUpload(e.ctx, call(map[string]any{
		"task_id": e.taskID, "name": "a.png", "mime_type": "image/png", "size_bytes": float64(len(data)), "sha256": sumHex(data)}))
	if res.IsError {
		t.Fatalf("create refused: %v", res.Content)
	}
	created := decodeResult(t, res.Content[0].(mcpsdk.TextContent).Text)
	putBytes(t, created["upload_url"].(string), data)
	return e, m, created["upload_id"].(string), token
}

func lastConfirmFence(t *testing.T, m *uploadMesh) string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	w := m.seen[len(m.seen)-1]
	if !strings.HasSuffix(w.path, "/confirm") {
		t.Fatalf("last write went to %s, want a confirm", w.path)
	}
	return w.header.Get(fenceTokenHeader)
}

// Review regression: a repeated confirm of a confirmed upload (the API replays
// it) still carries the card's fence; the binding lives until expires_at.
func TestUpload_RepeatConfirmKeepsFence(t *testing.T) {
	e, m, id, token := confirmFenceEnv(t)
	for i := 1; i <= 2; i++ {
		if r, _ := e.server.handleConfirmUpload(e.ctx, call(map[string]any{"upload_id": id})); r.IsError {
			t.Fatalf("confirm #%d refused: %v", i, r.Content)
		}
		if got := lastConfirmFence(t, m); got != token {
			t.Fatalf("confirm #%d: %s=%q, want the card's token", i, fenceTokenHeader, got)
		}
	}
}

// Review regression: a task_id argument naming another card does not take the
// confirm out from under the fence of the card the upload was declared for.
func TestUpload_ExplicitOtherTaskDoesNotOverrideFence(t *testing.T) {
	e, m, id, token := confirmFenceEnv(t)
	other := uuid.New().String()
	if r, _ := e.server.handleConfirmUpload(e.ctx, call(map[string]any{"upload_id": id, "task_id": other})); r.IsError {
		t.Fatalf("confirm refused: %v", r.Content)
	}
	if got := lastConfirmFence(t, m); got != token {
		t.Fatalf("confirm with task_id=%s: %s=%q, want the declaring card's token", other, fenceTokenHeader, got)
	}
}

// task_id still fences a confirm this server never saw declared (a restart).
func TestUpload_TaskIDFencesUnknownUpload(t *testing.T) {
	e, m, id, token := confirmFenceEnv(t)
	e.server.uploadTasks = uploadTaskMap{} // the declaration is forgotten
	if r, _ := e.server.handleConfirmUpload(e.ctx, call(map[string]any{"upload_id": id, "task_id": e.taskID})); r.IsError {
		t.Fatalf("confirm refused: %v", r.Content)
	}
	if got := lastConfirmFence(t, m); got != token {
		t.Fatalf("%s=%q, want the token of the task_id given", fenceTokenHeader, got)
	}
}
