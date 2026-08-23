package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// defaultHTTPTimeout is the Timeout applied to every REST call this client
// makes, unless overridden by MESH_MCP_HTTP_TIMEOUT_SECS. Unchanged from the
// long-standing default — this exists to make the value overridable, not to
// change it.
const defaultHTTPTimeout = 30 * time.Second

// RESTClient wraps HTTP calls to the Mesh REST API on behalf of an agent.
type RESTClient struct {
	baseURL    string
	agentKey   string
	httpClient *http.Client
}

// NewRESTClient creates a new RESTClient for the given API base URL and agent key.
//
// The HTTP timeout defaults to 30s, matching every deployed instance today.
// MESH_MCP_HTTP_TIMEOUT_SECS overrides it — for a caller whose own workload can
// legitimately make the server side take longer than 30s to answer, not as a
// general "make it more patient" knob. The memory-bench CI recall gate is
// exactly that caller: it writes ~45 sessions immediately before searching,
// and recall's query-embed call queues behind those same-request async
// embeds on the server's shared embedSem (memory_service.go's
// RecallWithStats) — a slow CI-only CPU embedder can make that queue alone
// exceed 30s before the response headers ever arrive, which used to fail the
// whole recall call as "could not run" although nothing was actually broken.
// An invalid or out-of-range value is ignored and falls back to the default
// silently — a malformed env var must never turn into a REST client with no
// timeout at all.
func NewRESTClient(baseURL, agentKey string) *RESTClient {
	return &RESTClient{
		baseURL:  strings.TrimRight(baseURL, "/"),
		agentKey: agentKey,
		httpClient: &http.Client{
			Timeout: httpTimeoutFromEnv(),
		},
	}
}

// httpTimeoutFromEnv resolves MESH_MCP_HTTP_TIMEOUT_SECS to a Duration,
// falling back to defaultHTTPTimeout when the variable is unset, not a
// positive integer, or outside [1, 600] seconds — a value that large would
// itself indicate a misconfiguration, not a legitimate slow caller.
func httpTimeoutFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv("MESH_MCP_HTTP_TIMEOUT_SECS"))
	if raw == "" {
		return defaultHTTPTimeout
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs < 1 || secs > 600 {
		return defaultHTTPTimeout
	}
	return time.Duration(secs) * time.Second
}

// do executes an HTTP request with the agent key auth header.
func (c *RESTClient) do(ctx context.Context, method, path string, body any) (*http.Response, error) {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal body: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}

	reqURL := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("X-Agent-Key", c.agentKey)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return c.httpClient.Do(req)
}

// doJSON executes an HTTP request and decodes the JSON response into result.
// Returns an error for HTTP 4xx/5xx responses using the API's error message.
func (c *RESTClient) doJSON(ctx context.Context, method, path string, body, result any) error {
	resp, err := c.do(ctx, method, path, body)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		var errBody map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return &APIError{
			StatusCode: resp.StatusCode,
			Message:    apiErrorMessage(errBody, resp.StatusCode),
			Body:       errBody,
		}
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// APIError is a failed API response. It carries the HTTP status alongside the
// server's message so callers can branch on the status — e.g. tell "this server
// predates the route" (404) apart from "this server refused me" (403) — without
// pattern-matching on error text. Error() renders exactly as before, so existing
// callers and their error strings are unchanged.
type APIError struct {
	StatusCode int
	Message    string

	// Body is the server's decoded error payload, kept whole.
	//
	// Message is prose for a human; some errors also carry a field the caller
	// must ACT on, and flattening those into a sentence forces the caller to
	// parse the sentence back out. The document version conflict is the case in
	// hand: it answers with current_version, and a caller that cannot read that
	// number has nothing to retry with except a blind guess.
	//
	// nil when the response body was not JSON. Error() is unchanged, so existing
	// callers and their error strings are unaffected.
	Body map[string]any
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s", http.StatusText(e.StatusCode), e.Message)
}

// apiErrorMessage builds a human-readable message from a Mesh API error body.
// The API returns {"message": "...", "validation": {"field": "reason", ...}} for
// 400s from apierror.ValidationError — without this, callers only ever saw the
// generic "Validation failed" message and lost the field-level detail entirely.
func apiErrorMessage(errBody map[string]any, statusCode int) string {
	msg := fmt.Sprintf("API error %d", statusCode)
	if m, ok := errBody["message"].(string); ok {
		msg = m
	} else if m, ok := errBody["error"].(string); ok {
		msg = m
	}

	if validation, ok := errBody["validation"].(map[string]any); ok && len(validation) > 0 {
		fields := make([]string, 0, len(validation))
		for field, reason := range validation {
			if reasonStr, ok := reason.(string); ok {
				fields = append(fields, fmt.Sprintf("%s: %s", field, reasonStr))
			}
		}
		sort.Strings(fields)
		if len(fields) > 0 {
			msg = fmt.Sprintf("%s (%s)", msg, strings.Join(fields, "; "))
		}
	}

	return msg
}

// doMultipart executes a multipart/form-data POST and decodes the JSON response into result.
func (c *RESTClient) doMultipart(ctx context.Context, path string, fields map[string]string, fileField, fileName, fileMime string, fileContent []byte, result any) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)

	// Write non-file fields.
	for k, v := range fields {
		if err := mw.WriteField(k, v); err != nil {
			return fmt.Errorf("write field %s: %w", k, err)
		}
	}

	// Write file content as a form file. When the caller supplies a MIME type we set
	// it on the part header rather than letting CreateFormFile default to
	// application/octet-stream: the API derives the stored MimeType from this header
	// (inferMimeType(part Content-Type, filename)), so it is the only channel an
	// explicit mime_type can travel through.
	var (
		fw  io.Writer
		err error
	)
	if fileMime != "" {
		// Build the Content-Disposition with mime.FormatMediaType rather than by hand.
		// An earlier version formatted it with Sprintf and a locally-written quote
		// escaper; that escaper copied two of the standard library's four
		// replacements and dropped \r -> %0D and \n -> %0A, so a filename containing
		// CRLF terminated the header early and injected arbitrary headers into the
		// part — including a second Content-Type that Header.Get returns in
		// preference to ours. FormatMediaType is the standard library's own encoder:
		// it percent-encodes such values per RFC 2231 instead of interpolating them.
		disposition := mime.FormatMediaType("form-data", map[string]string{
			"name":     fileField,
			"filename": fileName,
		})
		if disposition == "" {
			// Unreachable with the current call: FormatMediaType only returns "" when
			// the media type or an attribute KEY is not a valid token, and all three
			// ("form-data", "name", "filename") are literals here — no filename value,
			// including invalid UTF-8, produces it. Kept because it fails safe rather
			// than shipping a malformed part, but noted so nobody assumes it is tested.
			return fmt.Errorf("cannot encode Content-Disposition for file name %q", fileName)
		}
		h := make(textproto.MIMEHeader)
		h.Set("Content-Disposition", disposition)
		h.Set("Content-Type", fileMime)
		fw, err = mw.CreatePart(h)
	} else {
		fw, err = mw.CreateFormFile(fileField, fileName)
	}
	if err != nil {
		return fmt.Errorf("create form file: %w", err)
	}
	if _, err = fw.Write(fileContent); err != nil {
		return fmt.Errorf("write file content: %w", err)
	}

	if err = mw.Close(); err != nil {
		return fmt.Errorf("close multipart writer: %w", err)
	}

	reqURL := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, reqURL, &buf)
	if err != nil {
		return fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("X-Agent-Key", c.agentKey)
	req.Header.Set("Content-Type", mw.FormDataContentType())

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		var errBody map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&errBody)
		return &APIError{
			StatusCode: resp.StatusCode,
			Message:    apiErrorMessage(errBody, resp.StatusCode),
			Body:       errBody,
		}
	}

	if result != nil {
		if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// BaseURL returns the base URL used by this client.
func (c *RESTClient) BaseURL() string {
	return c.baseURL
}

// Ping checks connectivity by calling GET /health.
func (c *RESTClient) Ping(ctx context.Context) error {
	return c.doJSON(ctx, http.MethodGet, "/health", nil, nil)
}

// GetAgentMe returns the current agent's profile.
func (c *RESTClient) GetAgentMe(ctx context.Context) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/agents/me", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ListProjects lists projects in a workspace.
func (c *RESTClient) ListProjects(ctx context.Context, workspaceID string, includeArchived bool) (map[string]any, error) {
	path := fmt.Sprintf("/api/v1/workspaces/%s/projects", workspaceID)
	if !includeArchived {
		path += "?is_archived=false"
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetProject returns a project by ID.
func (c *RESTClient) GetProject(ctx context.Context, projectID string) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/projects/"+projectID, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetProjectStatuses returns statuses for a project.
func (c *RESTClient) GetProjectStatuses(ctx context.Context, projectID string) ([]map[string]any, error) {
	var result []map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/projects/"+projectID+"/statuses", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetTaskStatuses returns the statuses of the project owning the given task.
//
// This route carries the same workspace gate as POST /tasks/:task_id/move, whereas
// GET /projects/:proj_id/statuses is project-gated. Resolving a status slug is a
// precondition of the move, so it must be read through the route whose gate matches
// the move — otherwise a caller entitled to the transition is refused the lookup and
// the 403 it gets back names the project rather than the move.
func (c *RESTClient) GetTaskStatuses(ctx context.Context, taskID string) ([]map[string]any, error) {
	var result []map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/tasks/"+taskID+"/statuses", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetProjectCustomFields returns custom fields for a project.
func (c *RESTClient) GetProjectCustomFields(ctx context.Context, projectID string) ([]map[string]any, error) {
	var result []map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/projects/"+projectID+"/custom-fields", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ListTasks lists tasks with optional filters.
func (c *RESTClient) ListTasks(ctx context.Context, projectID string, params map[string]string) (map[string]any, error) {
	path := "/api/v1/projects/" + projectID + "/tasks"
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			q.Set(k, v)
		}
		path += "?" + q.Encode()
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetTask returns a task by ID. When taskID is not a valid full UUID (36 chars with dashes)
// it is resolved via GET /api/v1/tasks/by-short-id/:taskID.
func (c *RESTClient) GetTask(ctx context.Context, taskID string) (map[string]any, error) {
	path := "/api/v1/tasks/" + taskID
	if len(taskID) != 36 {
		path = "/api/v1/tasks/by-short-id/" + taskID
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// SearchTasks searches tasks across all projects in a workspace.
func (c *RESTClient) SearchTasks(ctx context.Context, workspaceID string, params map[string]string) (map[string]any, error) {
	q := url.Values{}
	for k, v := range params {
		q.Set(k, v)
	}
	path := "/api/v1/workspaces/" + workspaceID + "/tasks?" + q.Encode()
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// CreateTask creates a new task in a project.
func (c *RESTClient) CreateTask(ctx context.Context, projectID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/projects/"+projectID+"/tasks", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateTask updates a task.
func (c *RESTClient) UpdateTask(ctx context.Context, taskID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPatch, "/api/v1/tasks/"+taskID, body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// MoveTask moves a task to a new status.
func (c *RESTClient) MoveTask(ctx context.Context, taskID string, body map[string]any) error {
	return c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+taskID+"/move", body, nil)
}

// AssignTask assigns a task to an agent or user.
func (c *RESTClient) AssignTask(ctx context.Context, taskID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+taskID+"/assign", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// CreateSubtask creates a subtask.
func (c *RESTClient) CreateSubtask(ctx context.Context, parentTaskID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+parentTaskID+"/subtasks", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// AddDependency adds a dependency between tasks.
func (c *RESTClient) AddDependency(ctx context.Context, taskID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+taskID+"/dependencies", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// CreateHumanGateDecision records a human_gate decision on a task — the
// third human_gate exit (docs/human-gate-decision-recorded.md in evc-mesh).
// If the task's gate is currently live, the server releases it as a
// consequence of this write.
func (c *RESTClient) CreateHumanGateDecision(ctx context.Context, taskID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+taskID+"/human-gate-decisions", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// AddComment adds a comment to a task.
func (c *RESTClient) AddComment(ctx context.Context, taskID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+taskID+"/comments", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// AddVCSLink links a task to a pull request, commit, or branch.
func (c *RESTClient) AddVCSLink(ctx context.Context, taskID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+taskID+"/vcs-links", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetTaskVCSLinks returns the VCS links (PRs/MRs/commits/branches) attached
// to a task — provider, link_type, external_id, url, status, created_at per
// link. Unlike comments/artifacts, the underlying GET /tasks/:id/vcs-links
// endpoint (internal/handler/vcs_link_handler.go List, in evc-mesh) has no
// pagination: it returns the full set under the key "vcs_links" (plus a
// "count"), not "items"/"total_count"/"has_more" — so there is no
// truncation envelope to propagate here. Added for #5a6460b7: before this,
// get_task only ever surfaced vcs_link_count, and diagnosing a
// misclassified/stuck link required a raw REST call no MCP tool exposed.
func (c *RESTClient) GetTaskVCSLinks(ctx context.Context, taskID string) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/tasks/"+taskID+"/vcs-links", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ListComments lists comments on a task.
func (c *RESTClient) ListComments(ctx context.Context, taskID string, params map[string]string) (map[string]any, error) {
	path := "/api/v1/tasks/" + taskID + "/comments"
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			q.Set(k, v)
		}
		path += "?" + q.Encode()
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// UploadArtifact uploads an artifact to a task using multipart form.
// UploadArtifact uploads an artifact. metadata, when non-empty, must be a JSON object
// string — the API reads it from the "metadata" form field and stores it verbatim.
//
// mimeType previously arrived as a parameter and was discarded: it was in the
// signature but never written to the request, so callers setting it got the inferred
// type regardless. It now travels on the file part's Content-Type header, which is
// where the API looks for it.
func (c *RESTClient) UploadArtifact(ctx context.Context, taskID, name, artifactType, mimeType, metadata string, content []byte) (map[string]any, error) {
	fields := map[string]string{
		"name":          name,
		"artifact_type": artifactType,
	}
	if metadata != "" {
		fields["metadata"] = metadata
	}
	var result map[string]any
	if err := c.doMultipart(ctx, "/api/v1/tasks/"+taskID+"/artifacts", fields, "file", name, mimeType, content, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ListArtifacts lists artifacts for a task.
func (c *RESTClient) ListArtifacts(ctx context.Context, taskID string) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/tasks/"+taskID+"/artifacts", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetArtifact gets an artifact by ID.
func (c *RESTClient) GetArtifact(ctx context.Context, artifactID string) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/artifacts/"+artifactID, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetArtifactDownloadURL returns the download URL for an artifact.
// The REST API returns a redirect from /artifacts/:id/download — we return the URL.
func (c *RESTClient) GetArtifactDownloadURL(ctx context.Context, artifactID string) (string, error) {
	// Use the direct redirect URL as the download URL.
	return c.baseURL + "/api/v1/artifacts/" + artifactID + "/download", nil
}

// Heartbeat sends a heartbeat for the agent with optional status/message/metadata.
func (c *RESTClient) Heartbeat(ctx context.Context, body map[string]any) (map[string]any, error) {
	if body == nil {
		body = map[string]any{}
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/agents/heartbeat", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetAgentTasks returns tasks assigned to the current agent.
func (c *RESTClient) GetAgentTasks(ctx context.Context, params map[string]string) (map[string]any, error) {
	path := "/api/v1/agents/me/tasks"
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			q.Set(k, v)
		}
		path += "?" + q.Encode()
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// PollTasks long-polls for new task assignments.
// timeoutSecs controls how long the server blocks before returning (1–120).
func (c *RESTClient) PollTasks(ctx context.Context, timeoutSecs int) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/v1/agents/me/tasks/poll?timeout=%d", timeoutSecs), nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// PublishEvent publishes an event to the event bus.
func (c *RESTClient) PublishEvent(ctx context.Context, projectID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/projects/"+projectID+"/events", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetContext returns events from the event bus for a project.
func (c *RESTClient) GetContext(ctx context.Context, projectID string, params map[string]string) (map[string]any, error) {
	path := "/api/v1/projects/" + projectID + "/events"
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			q.Set(k, v)
		}
		path += "?" + q.Encode()
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetTaskContext returns full context for a task.
func (c *RESTClient) GetTaskContext(ctx context.Context, taskID string) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/tasks/"+taskID+"/context", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetTaskComments returns the most recent DefaultPageSize comments for a
// task, in chronological order (last element = newest).
//
// get_task(include_comments=true) is the call the READ-BEFORE-ACT gate tells
// every agent to trust as "the whole thread, read to the end". For a task
// with more than DefaultPageSize comments, the server's untouched default
// (sort_dir=asc) makes "the end" mean the OLDEST comments — an agent reading
// a long-running task's thread would see it stop at whenever the 50th
// comment landed and never learn the thread kept going. Requesting
// sort_dir=desc gets the newest page instead, then reversing it back to
// chronological order preserves the reading experience while fixing which N
// comments are shown. Ported from the same fix in entire-vc/evc-mesh
// (internal/mcp/rest_client.go, task 4222c17d / D1) — this repo's
// RESTClient hits the same evc-mesh REST API but is a separately maintained
// copy, so the fix does not propagate on its own.
func (c *RESTClient) GetTaskComments(ctx context.Context, taskID string) (map[string]any, error) {
	result, err := c.ListComments(ctx, taskID, map[string]string{
		"include_internal": "true",
		"sort_dir":         "desc",
	})
	if err != nil {
		return nil, err
	}
	if items, ok := result["items"].([]any); ok {
		for i, j := 0, len(items)-1; i < j; i, j = i+1, j-1 {
			items[i], items[j] = items[j], items[i]
		}
		result["items"] = items
	}
	return result, nil
}

// GetTaskArtifacts returns artifacts for a task.
func (c *RESTClient) GetTaskArtifacts(ctx context.Context, taskID string) (map[string]any, error) {
	return c.ListArtifacts(ctx, taskID)
}

// TaskDependencies holds the two directed edge sets the dependencies
// endpoint returns: Outgoing is this task's own blockers (what it depends
// on), Incoming is the reverse (other tasks that depend on this one).
type TaskDependencies struct {
	Outgoing []map[string]any `json:"outgoing"`
	Incoming []map[string]any `json:"incoming"`
}

// GetTaskDependencies returns dependencies for a task.
func (c *RESTClient) GetTaskDependencies(ctx context.Context, taskID string) (*TaskDependencies, error) {
	var result TaskDependencies
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/tasks/"+taskID+"/dependencies", nil, &result); err != nil {
		return nil, err
	}
	return &result, nil
}

// UpdateAgent updates the current agent.
func (c *RESTClient) UpdateAgent(ctx context.Context, agentID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPatch, "/api/v1/agents/"+agentID, body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateMe updates the calling agent's own profile via PATCH /agents/me (no admin RBAC required).
func (c *RESTClient) UpdateMe(ctx context.Context, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPatch, "/api/v1/agents/me", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// RegisterSubAgent creates a sub-agent under the given parent agent.
// The parentID is embedded in the request body as parent_agent_id.
func (c *RESTClient) RegisterSubAgent(ctx context.Context, workspaceID, parentID, name, agentType string, capabilities map[string]any) (map[string]any, error) {
	body := map[string]any{
		"name":            name,
		"agent_type":      agentType,
		"parent_agent_id": parentID,
	}
	if capabilities != nil {
		body["capabilities"] = capabilities
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/workspaces/"+workspaceID+"/agents", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ListSubAgents returns the sub-agents of a given agent.
// When recursive is true, all descendants (up to 10 levels) are returned.
func (c *RESTClient) ListSubAgents(ctx context.Context, agentID string, recursive bool) ([]map[string]any, error) {
	path := "/api/v1/agents/" + agentID + "/sub-agents"
	if recursive {
		path += "?recursive=true"
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	// Response is {"agents": [...], "count": N}
	agents, _ := result["agents"].([]any)
	out := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		if m, ok := a.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out, nil
}

// GetEffectiveRules calls the given rules path and returns the response.
// path should be a full API path like /api/v1/workspaces/{id}/rules/effective.
func (c *RESTClient) GetEffectiveRules(ctx context.Context, path string) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetTeamDirectory returns the team directory for a workspace (agents + humans).
func (c *RESTClient) GetTeamDirectory(ctx context.Context, workspaceID string) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/workspaces/"+workspaceID+"/team", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetAssignmentRules returns effective assignment rules for a project.
func (c *RESTClient) GetAssignmentRules(ctx context.Context, projectID string) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/projects/"+projectID+"/rules/assignment", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetWorkflowRules returns workflow rules and caller permissions for a project.
func (c *RESTClient) GetWorkflowRules(ctx context.Context, projectID string) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/projects/"+projectID+"/rules/workflow", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateAgentProfile updates the calling agent's profile fields.
func (c *RESTClient) UpdateAgentProfile(ctx context.Context, agentID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPut, "/api/v1/agents/"+agentID+"/profile", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// doRaw executes an HTTP request with a raw body and given Content-Type, returning the response body.
func (c *RESTClient) doRaw(ctx context.Context, method, path, contentType string, rawBody []byte) (body []byte, statusCode int, err error) {
	var bodyReader io.Reader
	if rawBody != nil {
		bodyReader = bytes.NewReader(rawBody)
	}

	reqURL := c.baseURL + path
	req, err := http.NewRequestWithContext(ctx, method, reqURL, bodyReader)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("X-Agent-Key", c.agentKey)
	if rawBody != nil {
		req.Header.Set("Content-Type", contentType)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	return data, resp.StatusCode, nil
}

// ImportWorkspaceConfig imports workspace configuration from YAML content.
func (c *RESTClient) ImportWorkspaceConfig(ctx context.Context, workspaceID, yamlContent string) (map[string]any, error) {
	data, statusCode, err := c.doRaw(ctx, http.MethodPost, "/api/v1/workspaces/"+workspaceID+"/config/import", "text/yaml", []byte(yamlContent))
	if err != nil {
		return nil, err
	}
	if statusCode >= 400 {
		var errBody map[string]any
		_ = json.Unmarshal(data, &errBody)
		msg := fmt.Sprintf("API error %d", statusCode)
		if m, ok := errBody["message"].(string); ok {
			msg = m
		} else if m, ok := errBody["error"].(string); ok {
			msg = m
		}
		return nil, fmt.Errorf("%s: %s", http.StatusText(statusCode), msg)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return result, nil
}

// CreateRecurringSchedule creates a recurring task schedule for a project.
func (c *RESTClient) CreateRecurringSchedule(ctx context.Context, projectID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/projects/"+projectID+"/recurring", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ListRecurringSchedules lists recurring schedules for a project.
func (c *RESTClient) ListRecurringSchedules(ctx context.Context, projectID string, params map[string]string) (map[string]any, error) {
	path := "/api/v1/projects/" + projectID + "/recurring"
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			q.Set(k, v)
		}
		path += "?" + q.Encode()
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetRecurringHistory returns the instance history for a recurring schedule.
func (c *RESTClient) GetRecurringHistory(ctx context.Context, scheduleID string, params map[string]string) (map[string]any, error) {
	path := "/api/v1/recurring/" + scheduleID + "/history"
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			q.Set(k, v)
		}
		path += "?" + q.Encode()
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// TriggerRecurringNow immediately creates the next instance for a recurring schedule.
func (c *RESTClient) TriggerRecurringNow(ctx context.Context, scheduleID string) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/recurring/"+scheduleID+"/trigger", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateRecurringSchedule updates a recurring schedule by ID.
func (c *RESTClient) UpdateRecurringSchedule(ctx context.Context, scheduleID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPatch, "/api/v1/recurring/"+scheduleID, body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// DeleteRecurringSchedule deletes a recurring schedule by ID.
func (c *RESTClient) DeleteRecurringSchedule(ctx context.Context, scheduleID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/recurring/"+scheduleID, nil, nil)
}

// Remember creates or updates a memory entry (UPSERT by key within scope).
func (c *RESTClient) Remember(ctx context.Context, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/memories", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// RecallMemoriesParams holds all optional parameters for memory search.
type RecallMemoriesParams struct {
	Query             string
	WorkspaceID       string
	ProjectID         string
	Scope             string
	Tags              []string
	TagsAny           []string
	CreatedBy         string
	Since             string
	Until             string
	RelevanceMin      float64
	ImportanceMin     float64
	ApplyRecencyDecay bool
	HalfLifeDays      int // >0 → passed as half_life_days to server (P1-D feature)
	OrderBy           string
	IncludeExpired    bool
	IncludeArchived   bool
	ExcludeSuperseded *bool // nil = server default (true); false = include superseded entries
	Limit             int
	Offset            int
}

// RecallMemories searches memories via full-text search with optional filters.
func (c *RESTClient) RecallMemories(ctx context.Context, p RecallMemoriesParams) (map[string]any, error) {
	params := make(url.Values)
	if p.Query != "" {
		params.Set("q", p.Query)
	}
	if p.WorkspaceID != "" {
		params.Set("workspace_id", p.WorkspaceID)
	}
	if p.ProjectID != "" {
		params.Set("project_id", p.ProjectID)
	}
	if p.Scope != "" {
		params.Set("scope", p.Scope)
	}
	for _, tag := range p.Tags {
		params.Add("tags", tag)
	}
	for _, tag := range p.TagsAny {
		params.Add("tags_any", tag)
	}
	if p.CreatedBy != "" {
		params.Set("created_by", p.CreatedBy)
	}
	if p.Since != "" {
		params.Set("since", p.Since)
	}
	if p.Until != "" {
		params.Set("until", p.Until)
	}
	if p.RelevanceMin > 0 {
		params.Set("relevance_min", fmt.Sprintf("%g", p.RelevanceMin))
	}
	params.Set("min_importance", fmt.Sprintf("%g", p.ImportanceMin))
	if p.ApplyRecencyDecay {
		params.Set("apply_recency_decay", "true")
	}
	if p.HalfLifeDays > 0 {
		params.Set("half_life_days", fmt.Sprintf("%d", p.HalfLifeDays))
	}
	if p.OrderBy != "" {
		params.Set("order_by", p.OrderBy)
	}
	if p.IncludeExpired {
		params.Set("include_expired", "true")
	}
	if p.IncludeArchived {
		params.Set("include_archived", "true")
	}
	if p.ExcludeSuperseded != nil {
		params.Set("exclude_superseded", fmt.Sprintf("%t", *p.ExcludeSuperseded))
	}
	if p.Limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", p.Limit))
	}
	if p.Offset > 0 {
		params.Set("offset", fmt.Sprintf("%d", p.Offset))
	}
	path := "/api/v1/memories/search"
	if encoded := params.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// RecallWithGraphParams holds parameters for KG-expanded memory recall.
//
// Scope/Tags/TagsAny restrict both the seed recall AND the BFS-expanded
// neighbours server-side (domain.RecallGraphOpts, task #2c087b2a) — graph
// expansion walks memory_edges, which carry no notion of scope, so without
// these an out-of-scope memory adjacent to an in-scope seed is returned. The
// server accepted these fields from day one; nothing on this path sent them
// until task #37e9344c, so the filter bypass was live on every call.
type RecallWithGraphParams struct {
	Query           string
	WorkspaceID     string
	ProjectID       string
	TaskID          string
	Scope           string
	Tags            []string
	TagsAny         []string
	Hops            int
	WeightThreshold float64
	Limit           int
}

// RecallWithGraph calls GET /api/v1/memories/recall_graph — multi-hop KG traversal
// seeded from hybrid recall hits. Returns memories ranked by composite score with
// hop_distance and provenance (rrf | graph) fields.
func (c *RESTClient) RecallWithGraph(ctx context.Context, p RecallWithGraphParams) (map[string]any, error) {
	params := make(url.Values)
	if p.Query != "" {
		params.Set("query", p.Query)
	}
	if p.WorkspaceID != "" {
		params.Set("workspace_id", p.WorkspaceID)
	}
	if p.ProjectID != "" {
		params.Set("project_id", p.ProjectID)
	}
	if p.TaskID != "" {
		params.Set("task_id", p.TaskID)
	}
	if p.Scope != "" {
		params.Set("scope", p.Scope)
	}
	// recall_graph's query struct binds Tags/TagsAny as plain strings via
	// echo's c.Bind, which — unlike the repeatable-param handling the regular
	// /search endpoint uses — keeps only the FIRST occurrence of a repeated
	// param. Sending repeated tags=a&tags=b here would silently narrow a
	// two-tag filter to one tag with no error (the exact C4-channel gotcha
	// from #2c087b2a). The server splits on comma (splitCSV), so a single
	// comma-joined value is the only encoding that survives intact.
	if len(p.Tags) > 0 {
		params.Set("tags", strings.Join(p.Tags, ","))
	}
	if len(p.TagsAny) > 0 {
		params.Set("tags_any", strings.Join(p.TagsAny, ","))
	}
	if p.Hops > 0 {
		params.Set("hops", fmt.Sprintf("%d", p.Hops))
	}
	if p.WeightThreshold > 0 {
		params.Set("weight_threshold", fmt.Sprintf("%g", p.WeightThreshold))
	}
	if p.Limit > 0 {
		params.Set("limit", fmt.Sprintf("%d", p.Limit))
	}
	path := "/api/v1/memories/recall_graph"
	if encoded := params.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// SetProjectKnowledge upserts a project-scoped knowledge entry by key.
func (c *RESTClient) SetProjectKnowledge(ctx context.Context, projectID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/projects/"+projectID+"/knowledge", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetProjectKnowledge returns knowledge for a project with optional workspace-tier pagination.
// limit/offset apply to workspace memories; minImportance and tagsAny filter workspace-tier.
func (c *RESTClient) GetProjectKnowledge(ctx context.Context, projectID string, limit, offset int, minImportance float64, tagsAny string) (map[string]any, error) {
	path := fmt.Sprintf("/api/v1/projects/%s/knowledge?limit=%d&offset=%d", projectID, limit, offset)
	if minImportance > 0 {
		path += fmt.Sprintf("&min_importance=%g", minImportance)
	}
	if tagsAny != "" {
		path += "&tags_any=" + tagsAny
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ForgetMemory deletes a memory entry by ID.
func (c *RESTClient) ForgetMemory(ctx context.Context, memoryID string) error {
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/memories/"+memoryID, nil, nil)
}

// CheckoutTask acquires an exclusive TTL-based lock on a task.
func (c *RESTClient) CheckoutTask(ctx context.Context, taskID string, ttlMinutes int) (map[string]any, error) {
	body := map[string]int{"ttl_minutes": ttlMinutes}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/tasks/"+taskID+"/checkout", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ReleaseTask releases the exclusive lock on a task acquired via CheckoutTask.
// checkoutToken must be the value returned by CheckoutTask; the API rejects releases
// that do not present the matching token.
func (c *RESTClient) ReleaseTask(ctx context.Context, taskID, checkoutToken string) error {
	body := map[string]string{"checkout_token": checkoutToken}
	return c.doJSON(ctx, http.MethodDelete, "/api/v1/tasks/"+taskID+"/checkout", body, nil)
}

// ExtendCheckout pushes the checkout_expires deadline forward on a task lock
// acquired via CheckoutTask. checkoutToken must match the token returned by
// CheckoutTask; the API rejects extensions that do not present it.
func (c *RESTClient) ExtendCheckout(ctx context.Context, taskID, checkoutToken string, ttlMinutes int) (map[string]any, error) {
	body := map[string]any{"checkout_token": checkoutToken, "ttl_minutes": ttlMinutes}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPatch, "/api/v1/tasks/"+taskID+"/checkout", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ExportWorkspaceConfig exports workspace configuration as YAML text.
// reportSessionBody is the JSON body for POST /api/v1/agents/me/sessions/report.
type reportSessionBody struct {
	TokensIn      int64   `json:"tokens_in"`
	TokensOut     int64   `json:"tokens_out"`
	Model         string  `json:"model,omitempty"`
	EstimatedCost float64 `json:"estimated_cost"`
}

// ReportSession accumulates token/cost usage onto the calling agent's active session.
// Returns the session totals as returned by the server, or an error.
func (c *RESTClient) ReportSession(ctx context.Context, tokensIn, tokensOut int64, model string, estimatedCost float64) (map[string]any, error) {
	body := reportSessionBody{
		TokensIn:      tokensIn,
		TokensOut:     tokensOut,
		Model:         model,
		EstimatedCost: estimatedCost,
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/agents/me/sessions/report", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *RESTClient) ExportWorkspaceConfig(ctx context.Context, workspaceID string) (string, error) {
	data, statusCode, err := c.doRaw(ctx, http.MethodGet, "/api/v1/workspaces/"+workspaceID+"/config/export", "", nil)
	if err != nil {
		return "", err
	}
	if statusCode >= 400 {
		var errBody map[string]any
		_ = json.Unmarshal(data, &errBody)
		msg := fmt.Sprintf("API error %d", statusCode)
		if m, ok := errBody["message"].(string); ok {
			msg = m
		} else if m, ok := errBody["error"].(string); ok {
			msg = m
		}
		return "", fmt.Errorf("%s: %s", http.StatusText(statusCode), msg)
	}
	return string(data), nil
}

// GetCanonicalUpdates calls GET /api/v1/canonical_updates.
// params keys: since (RFC3339), agent (slug), scope (project UUID) — all optional/empty.
func (c *RESTClient) GetCanonicalUpdates(ctx context.Context, params map[string]string) (map[string]any, error) {
	path := "/api/v1/canonical_updates"
	q := url.Values{}
	for k, v := range params {
		if v != "" {
			q.Set(k, v)
		}
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Documents
//
// Four reads, deliberately kept as four: the one that costs a body download and
// the ones that do not are different calls, so a caller cannot pay for a page by
// accident. See internal/mcp/docs.go for which tool argument reaches which.
// ---------------------------------------------------------------------------

// ListDocuments returns one page of a project's documents. The API populates a
// document's body only on single-document reads, so the items here are metadata
// and nothing else — which is what makes it safe to walk the whole tree.
func (c *RESTClient) ListDocuments(ctx context.Context, projectID string, params map[string]string) (map[string]any, error) {
	path := "/api/v1/projects/" + projectID + "/documents"
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			if v != "" {
				q.Set(k, v)
			}
		}
		if enc := q.Encode(); enc != "" {
			path += "?" + enc
		}
	}

	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// SearchDocuments full-text-searches one project's documents by title and body.
// Scope is per-project — the API joins on project_id, not workspace_id — so a
// caller wanting a different project makes a separate call; there is no
// cross-project search endpoint to fall back to.
func (c *RESTClient) SearchDocuments(ctx context.Context, projectID, query string, limit int) (map[string]any, error) {
	path := "/api/v1/projects/" + projectID + "/documents/search?q=" + url.QueryEscape(query)
	if limit > 0 {
		path += "&limit=" + strconv.Itoa(limit)
	}

	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetDocument returns one document WITH its markdown body. This is the only one
// of the four that ships a page over the wire; every other read exists to avoid
// it.
func (c *RESTClient) GetDocument(ctx context.Context, docID string) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/documents/"+docID, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetDocumentOutline returns a document's heading structure and version, without
// the body.
func (c *RESTClient) GetDocumentOutline(ctx context.Context, docID string) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, "/api/v1/documents/"+docID+"/outline", nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// GetDocumentSection returns one heading of a document and the markdown under
// it. The heading is a query parameter because it is free text; ref accepts
// either an anchor from the outline or the heading text itself.
func (c *RESTClient) GetDocumentSection(ctx context.Context, docID, ref string) (map[string]any, error) {
	path := "/api/v1/documents/" + docID + "/section?heading=" + url.QueryEscape(ref)

	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// CreateDocument creates a document in a project.
func (c *RESTClient) CreateDocument(ctx context.Context, projectID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/projects/"+projectID+"/documents", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// Document comments
//
// Two calls, and the create carries the quote as TEXT. There is no client-side
// anchor step: the offsets are computed by the server inside the same request
// that writes the row, so this package never holds a byte offset and cannot hold
// a stale one. The wrapper for POST /documents/:id/resolve-anchor was deleted
// with that step — the endpoint is still served for the browser editor, which
// measures a real selection, but nothing in this client calls it and a second
// way to reach it is a second way to reintroduce the two-step race.
// ---------------------------------------------------------------------------

// CreateDocumentComment posts a comment on a document.
//
// A quote in body asks the server to find that text and anchor the comment to it;
// quote_prefix/quote_suffix narrow a quote that occurs more than once. Failures
// arrive as *APIError: 400 with Body["code"] == "ambiguous_quote" and
// Body["matches"] when the quote occurs several times, 400 otherwise when it
// occurs not at all. Nothing is written in either case.
func (c *RESTClient) CreateDocumentComment(ctx context.Context, docID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPost, "/api/v1/documents/"+docID+"/comments", body, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// ListDocumentComments returns one page of a document's comments. Resolved
// threads are excluded unless params carries include_resolved=true.
func (c *RESTClient) ListDocumentComments(ctx context.Context, docID string, params map[string]string) (map[string]any, error) {
	path := "/api/v1/documents/" + docID + "/comments"
	if len(params) > 0 {
		q := url.Values{}
		for k, v := range params {
			if v != "" {
				q.Set(k, v)
			}
		}
		if enc := q.Encode(); enc != "" {
			path += "?" + enc
		}
	}

	var result map[string]any
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &result); err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateDocument patches a document. A base_version in the body makes the write
// conditional: when it no longer matches, the caller gets an *APIError with
// StatusCode 409 and the document's current version in Body.
func (c *RESTClient) UpdateDocument(ctx context.Context, docID string, body map[string]any) (map[string]any, error) {
	var result map[string]any
	if err := c.doJSON(ctx, http.MethodPatch, "/api/v1/documents/"+docID, body, &result); err != nil {
		return nil, err
	}
	return result, nil
}
