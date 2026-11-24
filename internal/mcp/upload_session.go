package mcp

// Two-step uploads: an agent over SSE has no file_path (the server cannot read
// its disk) and inline base64 of a binary breaks in its context. The API
// therefore hands out a presigned PUT URL; the agent PUTs the bytes itself and
// confirms. These tools are thin wrappers over that REST contract:
//
//	POST   /api/v1/documents/:doc_id/attachments/uploads
//	POST   /api/v1/tasks/:task_id/artifacts/uploads
//	POST   /api/v1/uploads/:upload_id/confirm
//	GET    /api/v1/documents/:doc_id/attachments
//	DELETE /api/v1/document-attachments/:att_id
//
// What they add is the text an agent acts on: the exact step 2, what to put in
// a document body, and a refusal that names the next step instead of raw JSON.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

const (
	toolCreateDocAttachmentUpload = "create_doc_attachment_upload"
	toolCreateArtifactUpload      = "create_artifact_upload"
	toolConfirmUpload             = "confirm_upload"
	toolListDocAttachments        = "list_doc_attachments"
	toolDeleteDocAttachment       = "delete_doc_attachment"
)

// uploadStep2 is the one line of step 2. The agent key never appears in it:
// the presigned URL is the only credential the PUT needs.
const uploadStep2 = "PUT the raw file bytes to upload_url (no Authorization, no headers to set by hand; " +
	"Content-Length must equal size_bytes), e.g. `curl -sS -T <file> '%s'`, then call confirm_upload(upload_id=\"%s\")."

const (
	uploadBodyHint = "Put `markdown` into the document body as is; never paste upload_url or a download URL — they expire."
	uploadNoInline = "This type is always downloaded, not shown inline."
)

const uploadFlowHowTo = "Two-step upload for any binary, over stdio and SSE alike: " +
	"(1) this call declares name, mime_type, size_bytes and sha256 and returns upload_url; " +
	"(2) PUT the raw bytes to upload_url with no Authorization and no hand-set headers, e.g. `curl -sS -T <file> '<upload_url>'`; " +
	"(3) confirm_upload(upload_id). upload_url expires (expires_at, 15 minutes)."

var sha256HexRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// uploadTaskMap remembers the task of an artifact upload until it expires.
// The binding outlives a successful confirm: a repeated confirm (the API
// replays it) still goes out under the same card's fence. put prunes expired
// entries.
type uploadTaskMap struct {
	mu sync.Mutex
	m  map[string]uploadTask
}

type uploadTask struct {
	taskID  string
	expires time.Time
}

func (u *uploadTaskMap) put(uploadID, taskID string, expires time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	now := time.Now()
	if u.m == nil {
		u.m = map[string]uploadTask{}
	}
	for id, t := range u.m {
		if now.After(t.expires) {
			delete(u.m, id)
		}
	}
	u.m[uploadID] = uploadTask{taskID: taskID, expires: expires}
}

func (u *uploadTaskMap) get(uploadID string) string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.m[uploadID].taskID
}

func (s *Server) registerUploadTools() {
	s.addTool(mcpsdk.NewTool(toolCreateDocAttachmentUpload,
		mcpsdk.WithDescription("Attach a file (image, pdf, any binary) to a document. "+uploadFlowHowTo+
			" confirm_upload returns `markdown` (an image or a link on the stable download_path) to put into the document body with update_doc."),
		mcpsdk.WithString("document_id", mcpsdk.Required(), mcpsdk.Description("Document UUID the file is attached to.")),
		mcpsdk.WithString("name", mcpsdk.Required(), mcpsdk.Description("File name shown in the document, e.g. screenshot.png.")),
		mcpsdk.WithString("mime_type", mcpsdk.Required(), mcpsdk.Description("MIME type of the file, e.g. image/png. png/jpeg/gif/webp/pdf are checked against the file's first bytes on confirm.")),
		mcpsdk.WithNumber("size_bytes", mcpsdk.Required(), mcpsdk.Description("Exact file size in bytes (max 25 MiB). The PUT must carry exactly this many bytes.")),
		mcpsdk.WithString("sha256", mcpsdk.Required(), mcpsdk.Description("Lowercase hex sha256 of the file (64 chars), e.g. from `shasum -a 256 <file>`. Checked on confirm.")),
	), s.tracked(toolCreateDocAttachmentUpload, s.handleCreateDocAttachmentUpload))

	s.addTool(mcpsdk.NewTool(toolCreateArtifactUpload,
		mcpsdk.WithDescription("Attach a file (screenshot, log, archive, any binary) to a task as an artifact. "+uploadFlowHowTo+
			" Use this instead of upload_artifact for any binary over SSE. Sends the card's checkout fence like upload_artifact."),
		mcpsdk.WithString("task_id", mcpsdk.Required(), mcpsdk.Description("Task ID (UUID or short id) the artifact belongs to.")),
		mcpsdk.WithString("name", mcpsdk.Required(), mcpsdk.Description("Artifact filename, e.g. report.pdf.")),
		mcpsdk.WithString("mime_type", mcpsdk.Required(), mcpsdk.Description("MIME type of the file, e.g. image/png. png/jpeg/gif/webp/pdf are checked against the file's first bytes on confirm.")),
		mcpsdk.WithNumber("size_bytes", mcpsdk.Required(), mcpsdk.Description("Exact file size in bytes, within the artifact limit and the task's quota. The PUT must carry exactly this many bytes.")),
		mcpsdk.WithString("sha256", mcpsdk.Required(), mcpsdk.Description("Lowercase hex sha256 of the file (64 chars), e.g. from `shasum -a 256 <file>`. Checked on confirm.")),
		mcpsdk.WithString("artifact_type", mcpsdk.Description("Category used to label and filter the artifact: file (default), code, log, report, link, image or data."), mcpsdk.DefaultString("file")),
	), s.tracked(toolCreateArtifactUpload, s.handleCreateArtifactUpload))

	s.addTool(mcpsdk.NewTool(toolConfirmUpload,
		mcpsdk.WithDescription("Step 3 of a two-step upload: after the bytes were PUT to upload_url, the server checks size, sha256 and type and stores the file as a document attachment or a task artifact. "+
			"Returns its id, download_path, renders_inline and `markdown` to put into a document body as is. Repeating it on a confirmed upload returns the same result. "+
			"A refusal names the cause (upload_not_received, size_mismatch, checksum_mismatch, mime_mismatch, upload_expired) and the next step."),
		mcpsdk.WithString("upload_id", mcpsdk.Required(), mcpsdk.Description("upload_id returned by create_doc_attachment_upload or create_artifact_upload.")),
		mcpsdk.WithString("task_id", mcpsdk.Description("For an artifact upload only: its task, to send the card's checkout fence when this server did not see the declaration (e.g. after a restart). Ignored when the upload's own task is known.")),
	), s.tracked(toolConfirmUpload, s.handleConfirmUpload))

	s.addTool(mcpsdk.NewTool(toolListDocAttachments,
		mcpsdk.WithDescription("List the files attached to a document: id, name, mime_type, size_bytes, created_at, plus download_path and the `markdown` to embed each one in the document body. The bytes are not inlined."),
		mcpsdk.WithString("document_id", mcpsdk.Required(), mcpsdk.Description("Document UUID whose attachments to list.")),
	), s.tracked(toolListDocAttachments, s.handleListDocAttachments))

	s.addTool(mcpsdk.NewTool(toolDeleteDocAttachment,
		mcpsdk.WithDescription("Delete a file attached to a document (soft delete: it disappears from list_doc_attachments and its download_path stops serving). Remove its markdown from the document body with update_doc as well."),
		mcpsdk.WithString("attachment_id", mcpsdk.Required(), mcpsdk.Description("Attachment UUID, from list_doc_attachments or confirm_upload.")),
	), s.tracked(toolDeleteDocAttachment, s.handleDeleteDocAttachment))
}

// uploadDeclaration reads and pre-checks the fields every create shares.
func uploadDeclaration(request mcpsdk.CallToolRequest) (map[string]any, error) {
	name := strings.TrimSpace(mcpsdk.ParseString(request, "name", ""))
	if name == "" {
		return nil, errors.New("name is required")
	}
	mimeType := strings.TrimSpace(mcpsdk.ParseString(request, "mime_type", ""))
	if mimeType == "" {
		return nil, errors.New("mime_type is required")
	}
	size := mcpsdk.ParseFloat64(request, "size_bytes", 0)
	if size <= 0 || size != float64(int64(size)) {
		return nil, errors.New("size_bytes is required: the exact file size in bytes, a positive integer")
	}
	sum := strings.ToLower(strings.TrimSpace(mcpsdk.ParseString(request, "sha256", "")))
	if !sha256HexRe.MatchString(sum) {
		return nil, errors.New("sha256 is required: 64 lowercase hex chars of the file's sha256 (e.g. `shasum -a 256 <file>`)")
	}
	return map[string]any{"name": name, "mime_type": mimeType, "size_bytes": int64(size), "sha256": sum}, nil
}

func (s *Server) handleCreateDocAttachmentUpload(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	docID := strings.TrimSpace(mcpsdk.ParseString(request, "document_id", ""))
	if docID == "" {
		return errResult("document_id is required")
	}
	body, err := uploadDeclaration(request)
	if err != nil {
		return errResult("%v", err)
	}
	var result map[string]any
	if err := s.getRESTClient(ctx).doJSON(ctx, http.MethodPost, "/api/v1/documents/"+docID+"/attachments/uploads", body, &result); err != nil {
		return uploadErrResult(toolCreateDocAttachmentUpload, err)
	}
	return jsonResult(withUploadStep2(result))
}

func (s *Server) handleCreateArtifactUpload(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	if s.getSession(ctx) == nil {
		return errResult("not authenticated: no agent session")
	}
	taskID := strings.TrimSpace(mcpsdk.ParseString(request, "task_id", ""))
	if taskID == "" {
		return errResult("task_id is required")
	}
	body, err := uploadDeclaration(request)
	if err != nil {
		return errResult("%v", err)
	}
	body["artifact_type"] = mcpsdk.ParseString(request, "artifact_type", "file")
	var result map[string]any
	err = s.fencedWrite(ctx, toolCreateArtifactUpload, taskID, false, func(ctx context.Context) error {
		return s.getRESTClient(ctx).doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+taskID+"/artifacts/uploads", body, &result)
	})
	if err != nil {
		return uploadErrResult(toolCreateArtifactUpload, err)
	}
	if id, _ := result["upload_id"].(string); id != "" {
		expires := time.Now().Add(time.Hour)
		if at, ok := result["expires_at"].(string); ok {
			if t, perr := time.Parse(time.RFC3339Nano, at); perr == nil {
				expires = t
			}
		}
		s.uploadTasks.put(id, taskID, expires)
	}
	return jsonResult(withUploadStep2(result))
}

func withUploadStep2(result map[string]any) map[string]any {
	if result == nil {
		result = map[string]any{}
	}
	url, _ := result["upload_url"].(string)
	id, _ := result["upload_id"].(string)
	result["next_step"] = fmt.Sprintf(uploadStep2, url, id)
	return result
}

func (s *Server) handleConfirmUpload(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	uploadID := strings.TrimSpace(mcpsdk.ParseString(request, "upload_id", ""))
	if uploadID == "" {
		return errResult("upload_id is required")
	}
	// The task the upload was declared for wins: a task_id argument naming
	// another card must not move the write under that card's fence (or none).
	// task_id only fills in when this server never saw the declaration.
	taskID := s.uploadTasks.get(uploadID)
	if taskID == "" {
		taskID = strings.TrimSpace(mcpsdk.ParseString(request, "task_id", ""))
	}
	var result map[string]any
	confirm := func(ctx context.Context) error {
		return s.getRESTClient(ctx).doJSON(ctx, http.MethodPost, "/api/v1/uploads/"+uploadID+"/confirm", nil, &result)
	}
	var err error
	if taskID != "" {
		err = s.fencedWrite(ctx, toolConfirmUpload, taskID, false, confirm)
	} else {
		err = confirm(ctx)
	}
	if err != nil {
		return uploadErrResult(toolConfirmUpload, err)
	}
	if result == nil {
		result = map[string]any{}
	}
	result["how_to_use"] = uploadBodyHint
	if inline, ok := result["renders_inline"].(bool); ok && !inline {
		result["how_to_use"] = uploadBodyHint + " " + uploadNoInline
	}
	return jsonResult(result)
}

func (s *Server) handleListDocAttachments(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	docID := strings.TrimSpace(mcpsdk.ParseString(request, "document_id", ""))
	if docID == "" {
		return errResult("document_id is required")
	}
	var result map[string]any
	if err := s.getRESTClient(ctx).doJSON(ctx, http.MethodGet, "/api/v1/documents/"+docID+"/attachments", nil, &result); err != nil {
		return errResult("failed to list document attachments: %v", err)
	}
	items, _ := result["items"].([]any)
	if items == nil {
		items = []any{}
	}
	for _, it := range items {
		att, ok := it.(map[string]any)
		if !ok {
			continue
		}
		// storage_key is the bucket path: internal, and useless to an agent.
		delete(att, "storage_key")
		id, _ := att["id"].(string)
		if id == "" {
			continue
		}
		path := "/api/v1/document-attachments/" + id + "/download?disposition=inline"
		name, _ := att["name"].(string)
		mime, _ := att["mime_type"].(string)
		att["download_path"] = path
		att["markdown"] = attachmentMarkdown(name, mime, path)
	}
	result["items"] = items
	return jsonResult(result)
}

func (s *Server) handleDeleteDocAttachment(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	attID := strings.TrimSpace(mcpsdk.ParseString(request, "attachment_id", ""))
	if attID == "" {
		return errResult("attachment_id is required")
	}
	if err := s.getRESTClient(ctx).doJSON(ctx, http.MethodDelete, "/api/v1/document-attachments/"+attID, nil, nil); err != nil {
		return errResult("failed to delete document attachment: %v", err)
	}
	return jsonResult(map[string]any{"deleted": true, "attachment_id": attID})
}

var (
	markdownImageTypes   = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}
	markdownLabelEscaper = strings.NewReplacer(`\`, `\\`, `[`, `\[`, `]`, `\]`)
)

// attachmentMarkdown matches the API's confirm answer: an image for the raster
// types a document body renders, a link for everything else.
func attachmentMarkdown(name, mimeType, path string) string {
	label := markdownLabelEscaper.Replace(name)
	base := strings.ToLower(strings.TrimSpace(strings.SplitN(mimeType, ";", 2)[0]))
	if markdownImageTypes[base] {
		return "![" + label + "](" + path + ")"
	}
	return "[" + label + "](" + path + ")"
}

// uploadErrResult turns an upload refusal into a tool error that names the
// code and what to do next. It keys on the API's error_code only: a plain
// conflict or any other failure keeps the generic text, and a checkout-fence
// refusal keeps the fence's own wording.
func uploadErrResult(tool string, err error) (*mcpsdk.CallToolResult, error) {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Body == nil {
		return errResult("%s failed: %v", tool, err)
	}
	code, _ := apiErr.Body["error_code"].(string)
	detail, _ := apiErr.Body["context"].(map[string]any)
	field := func(k string) string {
		if detail == nil || detail[k] == nil {
			return "?"
		}
		// JSON numbers decode as float64; a byte count must not read 2.62144e+07.
		if f, ok := detail[k].(float64); ok && f == float64(int64(f)) {
			return strconv.FormatInt(int64(f), 10)
		}
		return fmt.Sprint(detail[k])
	}
	var next string
	switch code {
	case "file_too_large":
		next = fmt.Sprintf("the file is larger than the limit of %s bytes; nothing was stored. Upload a smaller or compressed file, or split it.", field("limit_bytes"))
	case "size_mismatch":
		next = fmt.Sprintf("the server received %s bytes, the upload declared %s. PUT the exact file again to the same upload_url (valid until its expires_at) and call confirm_upload again; if the file itself is different, declare a new upload with its real size_bytes and sha256.", field("received"), field("declared"))
	case "checksum_mismatch":
		next = fmt.Sprintf("the received bytes hash to %s, the upload declared %s; the received copy was discarded. PUT the file again to the same upload_url (valid until its expires_at), byte for byte, e.g. `curl -sS -T <file> '<upload_url>'`, then confirm_upload again; if the file changed, declare a new upload with its real sha256.", field("received"), field("declared"))
	case "upload_not_received":
		next = "nothing has been PUT to upload_url yet. PUT the raw bytes first, e.g. `curl -sS -T <file> '<upload_url>'` (no Authorization header), then call confirm_upload again."
	case "upload_expired":
		next = fmt.Sprintf("this upload expired at %s. Declare it again (create_doc_attachment_upload or create_artifact_upload) for a fresh upload_url, PUT the file there and confirm the new upload_id.", field("expires_at"))
	case "mime_mismatch":
		next = fmt.Sprintf("the file's first bytes are %s, not the declared %s; the received copy was discarded. Declare a new upload with the real mime_type (or upload the right file).", field("detected"), field("declared"))
	case "upload_not_found":
		next = "no such upload_id for you in this workspace (unknown, someone else's, or swept after expiry). Use the upload_id your own create_*_upload call returned, or declare a new upload."
	default:
		return errResult("%s failed: %v", tool, err)
	}
	return errResult("%s: %s", code, next)
}
