package mcp

// Checkout fence on the result writes of a card the Runner holds for this
// agent. Contract: evc-mesh-runner docs/checkout-fence.md (the Runner side)
// and evc-mesh internal/handler/result_fence.go (the server side).
//
// The Runner writes one file per card its turn holds:
//
//	<dir>/<agent_id>/<task_id>.json            the lock: token, session, generation, input_revision
//	<dir>/<agent_id>/<task_id>.refusals.jsonl  appended here when Mesh refuses a fenced write
//
// <dir> is MESH_CHECKOUT_FENCE_DIR, else ~/.mesh-runner/checkout-fence.
//
// On every comment, artifact, move and finish of a card the file is read again
// (nothing is cached: a resumed session must write with the lock of the turn
// that resumed it). File present: the lock goes out as X-Mesh-Checkout-Token
// (else X-Mesh-Checkout-Session-Id + X-Mesh-Checkout-Generation);
// X-Mesh-Expected-Input-Revision only on finish. No file: no headers, the write
// is exactly as before. The headers ride on the same request as the
// Idempotency-Key (doKeyed, doMultipartKeyed); the key is not touched (the
// server fingerprints the body, not these headers).
//
// A 409 checkout_precondition_failed or input_revision_changed is journalled
// (attempt_id and turn copied from the file) so the Runner stops a turn whose
// lock is gone, and the tool result tells the agent what to do next.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	checkoutFenceDirEnv = "MESH_CHECKOUT_FENCE_DIR"

	fenceTokenHeader      = "X-Mesh-Checkout-Token"
	fenceSessionHeader    = "X-Mesh-Checkout-Session-Id"
	fenceGenerationHeader = "X-Mesh-Checkout-Generation"
	fenceRevisionHeader   = "X-Mesh-Expected-Input-Revision"

	fenceLockLost        = "checkout_precondition_failed"
	fenceRevisionChanged = "input_revision_changed"

	fenceLockLostHint        = "the lock of this turn is gone; stop, write nothing more"
	fenceRevisionChangedHint = "the task changed; run get_task, then finish again"
)

var (
	fenceUUID     = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	fenceShortID  = regexp.MustCompile(`^[0-9a-f]{6,12}$`)
	fenceJournalM sync.Mutex
)

// checkoutFenceRoot is the directory holding one subdirectory per agent.
func checkoutFenceRoot() string {
	if dir := strings.TrimSpace(os.Getenv(checkoutFenceDirEnv)); dir != "" {
		if strings.HasPrefix(dir, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				dir = filepath.Join(home, dir[2:])
			}
		}
		return dir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".mesh-runner", "checkout-fence")
}

// fenceFile is the Runner's lock file of one card (checkout_fence.py publish).
type fenceFile struct {
	Version       int    `json:"version"`
	TaskID        string `json:"task_id"`
	AttemptID     string `json:"attempt_id"`
	CheckoutToken string `json:"checkout_token"`
	SessionID     string `json:"session_id"`
	Generation    *int64 `json:"generation"`
	InputRevision *int64 `json:"input_revision"`
	Turn          *int64 `json:"turn"`
}

// writeFence is the fence of one write: the headers it sends and what a
// refusal journals.
type writeFence struct {
	tool    string
	taskID  string // full UUID, from the file name
	journal string // refusals.jsonl path
	file    fenceFile
	headers http.Header
}

// resolveFenceFile finds the card's file under dir. taskID may be a full UUID
// or a 6-12 hex short id (the tools accept both); a short id resolves only to
// exactly one file of this agent.
func resolveFenceFile(dir, taskID string) string {
	taskID = strings.ToLower(strings.TrimSpace(taskID))
	if fenceUUID.MatchString(taskID) {
		return filepath.Join(dir, taskID+".json")
	}
	if !fenceShortID.MatchString(taskID) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(dir, taskID+"*.json"))
	found := ""
	for _, m := range matches {
		if !fenceUUID.MatchString(strings.TrimSuffix(filepath.Base(m), ".json")) {
			continue
		}
		if found != "" {
			return "" // ambiguous: never send another card's lock
		}
		found = m
	}
	return found
}

// loadWriteFence reads the card's file now. nil means no fence: no file, no
// lock in it, or an unreadable file (the write goes out as before).
func loadWriteFence(agentID, tool, taskID string, finish bool) *writeFence {
	root := checkoutFenceRoot()
	if root == "" || agentID == "" {
		return nil
	}
	path := resolveFenceFile(filepath.Join(root, agentID), taskID)
	if path == "" {
		return nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			log.Printf("checkout fence: read %s: %v", path, err)
		}
		return nil
	}
	var f fenceFile
	if err := json.Unmarshal(raw, &f); err != nil {
		log.Printf("checkout fence: %s is not JSON: %v", path, err)
		return nil
	}
	fullID := strings.TrimSuffix(filepath.Base(path), ".json")
	if f.TaskID != "" && !strings.EqualFold(f.TaskID, fullID) {
		log.Printf("checkout fence: %s names task %s; ignored", path, f.TaskID)
		return nil
	}
	h := http.Header{}
	switch {
	case strings.TrimSpace(f.CheckoutToken) != "":
		h.Set(fenceTokenHeader, strings.TrimSpace(f.CheckoutToken))
	case strings.TrimSpace(f.SessionID) != "" && f.Generation != nil && *f.Generation > 0:
		h.Set(fenceSessionHeader, strings.TrimSpace(f.SessionID))
		h.Set(fenceGenerationHeader, strconv.FormatInt(*f.Generation, 10))
	default:
		return nil // the file names no lock
	}
	if finish {
		rev := int64(0)
		if f.InputRevision != nil {
			rev = *f.InputRevision
		}
		if seen := defaultInputRevisions.get(fullID); seen > rev {
			rev = seen // a get_task in this process read a newer one
		}
		if rev > 0 {
			h.Set(fenceRevisionHeader, strconv.FormatInt(rev, 10))
		}
	}
	return &writeFence{
		tool:    tool,
		taskID:  fullID,
		journal: strings.TrimSuffix(path, ".json") + ".refusals.jsonl",
		file:    f,
		headers: h,
	}
}

// inputRevisions remembers the newest input_revision a get_task of a card
// returned in this process. Revisions only grow, so the newest wins over the
// file's (read at turn start) and over an older read.
//
// Entries are kept for the life of the process: evicting one would make a
// later finish send the file's older revision. They are recorded only on a
// host where the Runner keeps fence files (the fence directory exists), so a
// shared HTTP-mode server, which never sends a fence, never accumulates them.
type inputRevisions struct {
	mu   sync.Mutex
	revs map[string]int64
}

var defaultInputRevisions = &inputRevisions{revs: map[string]int64{}}

// fenceHostActive reports whether this host has a fence directory at all.
func fenceHostActive() bool {
	root := checkoutFenceRoot()
	if root == "" {
		return false
	}
	st, err := os.Stat(root)
	return err == nil && st.IsDir()
}

func (r *inputRevisions) observe(task map[string]any) {
	if !fenceHostActive() {
		return
	}
	id, _ := task["id"].(string)
	id = strings.ToLower(id)
	if !fenceUUID.MatchString(id) {
		return
	}
	var rev int64
	switch v := task["input_revision"].(type) {
	case float64:
		rev = int64(v)
	case json.Number:
		rev, _ = v.Int64()
	}
	if rev <= 0 {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if rev > r.revs[id] {
		r.revs[id] = rev
	}
}

func (r *inputRevisions) get(taskID string) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.revs[strings.ToLower(taskID)]
}

// writeFenceKey carries the fence of one write to the request helpers.
type writeFenceKey struct{}

func withWriteFence(ctx context.Context, f *writeFence) context.Context {
	if f == nil {
		return ctx
	}
	return context.WithValue(ctx, writeFenceKey{}, f)
}

// applyFenceHeaders sets the fence headers of the write in ctx on req; no-op
// for a write without one.
func applyFenceHeaders(ctx context.Context, req *http.Request) {
	f, _ := ctx.Value(writeFenceKey{}).(*writeFence)
	if f == nil {
		return
	}
	for k, vs := range f.headers {
		for _, v := range vs {
			req.Header.Set(k, v)
		}
	}
}

// fenceRefusal is one line of the refusal journal (the Runner's
// checkout_fence.FenceDir.refusals reads it).
type fenceRefusal struct {
	AttemptID     string `json:"attempt_id"`
	Turn          *int64 `json:"turn"`
	Code          string `json:"code"`
	At            string `json:"at"`
	Tool          string `json:"tool"`
	Reassigned    bool   `json:"reassigned"`
	InputRevision *int64 `json:"input_revision"`
}

// fenceRefusalNow is the journal clock. A var for tests.
var fenceRefusalNow = func() time.Time { return time.Now().UTC() }

// fenceRefusalCode is the fence refusal code of err, or "".
func fenceRefusalCode(err error) string {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		return ""
	}
	switch code, _ := apiErr.Body["error_code"].(string); code {
	case fenceLockLost, fenceRevisionChanged:
		return code
	}
	return ""
}

// isFenceRefusal reports a 409 checkout_precondition_failed or
// input_revision_changed: nothing was written and the agent must act on it.
func isFenceRefusal(err error) bool { return fenceRefusalCode(err) != "" }

// noteRefusal journals a fence refusal of err and puts the agent's next step
// in front of the server's message. Any other error is returned unchanged.
func (f *writeFence) noteRefusal(err error) error {
	if f == nil || err == nil {
		return err
	}
	code := fenceRefusalCode(err)
	if code == "" {
		return err
	}
	var apiErr *APIError
	errors.As(err, &apiErr)
	hint := fenceLockLostHint
	if code == fenceRevisionChanged {
		hint = fenceRevisionChangedHint
	}
	row := fenceRefusal{
		AttemptID: f.file.AttemptID,
		Turn:      f.file.Turn,
		Code:      code,
		At:        fenceRefusalNow().Format(time.RFC3339),
		Tool:      f.tool,
	}
	if c, ok := apiErr.Body["context"].(map[string]any); ok {
		row.Reassigned, _ = c["reassigned"].(bool)
		if v, ok := c["input_revision"].(float64); ok {
			rev := int64(v)
			row.InputRevision = &rev
		}
	}
	if jerr := appendFenceRefusal(f.journal, row); jerr != nil {
		log.Printf("checkout fence: journal %s: %v", f.journal, jerr)
	}
	if !strings.HasPrefix(apiErr.Message, hint) {
		apiErr.Message = fmt.Sprintf("%s (%s): %s", hint, code, apiErr.Message)
	}
	return err
}

func appendFenceRefusal(path string, row fenceRefusal) error {
	line, err := json.Marshal(row)
	if err != nil {
		return err
	}
	fenceJournalM.Lock()
	defer fenceJournalM.Unlock()
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err := fh.Write(append(line, '\n')); err != nil {
		_ = fh.Close()
		return err
	}
	return fh.Close()
}

// fencedWrite runs one result write of a card under its checkout fence: the
// file is read now, its headers ride on the request(s) write sends, and a
// fence refusal is journalled. Without a session or a file, write runs as
// before.
func (s *Server) fencedWrite(ctx context.Context, tool, taskID string, finish bool, write func(context.Context) error) error {
	agentID := ""
	if session := s.getSession(ctx); session != nil {
		agentID = session.AgentID.String()
	}
	f := loadWriteFence(agentID, tool, taskID, finish)
	return f.noteRefusal(write(withWriteFence(ctx, f)))
}
