package mcp

// Idempotency-Key on the three result writes: add_comment, upload_artifact,
// finish_task. The server contract is evc-mesh docs/idempotency.md: a repeat
// with the same key and body answers the first answer (Idempotent-Replayed:
// true) and writes nothing; another body under the key is 409
// idempotency_key_reused; a repeat while the first is still running is 409
// idempotency_request_in_progress with Retry-After. The Runner's client
// (evc-mesh-runner src/mesh_runner/mesh.py, MeshClient.request) is the
// reference behaviour this mirrors.
//
// Two repeats are covered:
//
//  1. A lost answer inside one tool call (timeout, connection reset): the
//     client sends the write again with the same key (lostAnswerDelays).
//  2. The agent repeating the tool call after a lost answer (same caller, same
//     runner checkout, same tool, card and arguments): the key is derived, not
//     random, so the repeat carries the key of the first send. See writeKeyBook.
//
// MESH_MCP_IDEMPOTENCY_KEYS=0 (or false/off/no) turns both off: no header, no
// extra sends; behaviour is exactly as before.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	idempotencyKeysEnv        = "MESH_MCP_IDEMPOTENCY_KEYS"
	idempotencyWindowEnv      = "MESH_MCP_IDEMPOTENCY_WINDOW_SECS"
	idempotencyKeyHeader      = "Idempotency-Key"
	idempotentReplayedHeader  = "Idempotent-Replayed"
	idempotencyKeyReused      = "idempotency_key_reused"
	idempotencyInProgress     = "idempotency_request_in_progress"
	writeKeyPrefix            = "mcp-"
	defaultRepeatWindow       = 10 * time.Minute
	inProgressMaxWait         = 30 * time.Second
	inProgressMaxRetries      = 3
	writeKeyBookRetention     = 25 * time.Hour // the server keeps a key 24 h
	writeKeyBookPruneInterval = time.Hour

	// Runner checkout of the current attempt, exported into the harness
	// environment (evc-mesh-runner runner.py, MESH_RUNNER_CHECKOUT_*).
	runnerCheckoutSessionEnv    = "MESH_RUNNER_CHECKOUT_SESSION"
	runnerCheckoutGenerationEnv = "MESH_RUNNER_CHECKOUT_GENERATION"
)

// lostAnswerDelays are the pauses before re-sending a keyed write whose answer
// was lost. Same values as the Runner (LOST_ANSWER_DELAYS). A var for tests.
var lostAnswerDelays = []time.Duration{1 * time.Second, 3 * time.Second}

// idempotencySleep waits d or until ctx ends. A var for tests.
var idempotencySleep = sleepCtx

// idempotencyKeysEnabled reads the rollback switch on every call, so a changed
// environment of a restarted process takes effect without a release.
func idempotencyKeysEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(idempotencyKeysEnv))) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

func repeatWindowFromEnv() time.Duration {
	raw := strings.TrimSpace(os.Getenv(idempotencyWindowEnv))
	if raw == "" {
		return defaultRepeatWindow
	}
	secs, err := strconv.Atoi(raw)
	if err != nil || secs < 1 || secs > 24*3600 {
		return defaultRepeatWindow
	}
	return time.Duration(secs) * time.Second
}

// writeKeyBook derives the Idempotency-Key of one logical write.
//
// key = "mcp-" + sha256(caller | scope | tool | task | sha256(args) | n)
//
//   - caller: a digest of the credential and API base, so two agents served by
//     one HTTP-mode process never share a key (the server scopes by caller too).
//   - scope: the runner checkout of the attempt (session + generation) when the
//     process runs under mesh-runner, else a random id of this process. A
//     restarted MCP process of the same attempt keeps the scope, so a repeat
//     after a restart lands on the first send's key; a new attempt never does.
//   - n: a repeat of the same arguments within the window (since the last
//     call with them) keeps n, so the server replays instead of writing again;
//     the same arguments after the window are a new write (n+1). A restarted
//     process starts at n=0: inside one checkout a write repeated after a
//     restart is treated as the repeat of the first one, never as a new one.
type writeKeyBook struct {
	mu        sync.Mutex
	entries   map[string]*writeKeyEntry
	now       func() time.Time
	window    func() time.Duration
	scope     func() string
	lastPrune time.Time
}

type writeKeyEntry struct {
	n    int
	last time.Time
}

var processScope = func() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "pid-" + strconv.Itoa(os.Getpid()) + "-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return "proc-" + hex.EncodeToString(b)
}()

func runnerScope() string {
	session := strings.TrimSpace(os.Getenv(runnerCheckoutSessionEnv))
	if session == "" {
		return processScope
	}
	return "runner-" + session + "-" + strings.TrimSpace(os.Getenv(runnerCheckoutGenerationEnv))
}

func newWriteKeyBook() *writeKeyBook {
	return &writeKeyBook{
		entries: map[string]*writeKeyEntry{},
		now:     time.Now,
		window:  repeatWindowFromEnv,
		scope:   runnerScope,
	}
}

// defaultWriteKeys is process-wide: in HTTP mode the REST client is built per
// request, so the book cannot live on it.
var defaultWriteKeys = newWriteKeyBook()

func digestHex(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		_, _ = io.WriteString(h, p)
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// key returns the key for one write; args must marshal deterministically
// (encoding/json sorts map keys).
func (b *writeKeyBook) key(caller, tool, taskID string, args any) string {
	raw, err := json.Marshal(args)
	if err != nil {
		raw = []byte(err.Error())
	}
	base := digestHex(caller, b.scope(), tool, taskID, digestHex(string(raw)))

	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	b.prune(now)
	e, ok := b.entries[base]
	switch {
	case !ok:
		e = &writeKeyEntry{}
		b.entries[base] = e
	case now.Sub(e.last) > b.window():
		e.n++
	}
	e.last = now
	return writeKeyPrefix + digestHex(base, strconv.Itoa(e.n))
}

// prune drops entries the server has forgotten anyway. Entries younger than
// the server's 24 h retention must stay: forgetting one would restart its n at
// 0 and turn a later, deliberate write into a replay of the old one.
func (b *writeKeyBook) prune(now time.Time) {
	if now.Sub(b.lastPrune) < writeKeyBookPruneInterval {
		return
	}
	b.lastPrune = now
	for k, e := range b.entries {
		if now.Sub(e.last) > writeKeyBookRetention {
			delete(b.entries, k)
		}
	}
}

// writeKey is the key of one result write by this client, or "" when keys are
// switched off.
func (c *RESTClient) writeKey(tool, taskID string, args any) string {
	if !idempotencyKeysEnabled() {
		return ""
	}
	return defaultWriteKeys.key(c.callerDigest(), tool, taskID, args)
}

func (c *RESTClient) callerDigest() string {
	cred := c.agentKey
	if c.chatTokenFile != "" {
		cred = "chat-token-file:" + c.chatTokenFile
	}
	return digestHex(c.baseURL, cred)
}

// sendKeyed runs one keyed write: send builds and sends a fresh request each
// time (with the same body bytes and key). A lost answer is re-sent after
// lostAnswerDelays; a 409 in_progress waits its Retry-After (never less) up to
// inProgressMaxRetries times; anything else is returned to the caller as is.
func (c *RESTClient) sendKeyed(ctx context.Context, send func() (*http.Response, error)) (*http.Response, error) {
	delays := append([]time.Duration(nil), lostAnswerDelays...)
	inProgress := 0
	for {
		resp, err := send()
		if err != nil {
			if ctx.Err() != nil || len(delays) == 0 || !lostAnswer(err) {
				return nil, err
			}
			if serr := idempotencySleep(ctx, delays[0]); serr != nil {
				return nil, err
			}
			delays = delays[1:]
			continue
		}
		if resp.StatusCode != http.StatusConflict || inProgress >= inProgressMaxRetries {
			return resp, nil
		}
		buf, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(buf))
		if errorCodeOf(buf) != idempotencyInProgress {
			return resp, nil
		}
		wait, ok := parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())
		if !ok {
			wait = time.Second // the contract always sends Retry-After: 1
		}
		if wait > inProgressMaxWait {
			return resp, nil // never repeat before the server's Retry-After
		}
		if serr := idempotencySleep(ctx, wait); serr != nil {
			return nil, serr
		}
		inProgress++
	}
}

// lostAnswer reports a transport failure after which the write may or may not
// have happened: everything except a request that could not even be built.
func lostAnswer(err error) bool {
	return !errors.Is(err, errRequestNotSent)
}

// errRequestNotSent marks a failure before anything left the process (request
// construction, credentials): re-sending it would fail the same way.
var errRequestNotSent = errors.New("request not sent")

func errorCodeOf(body []byte) string {
	var m map[string]any
	if json.Unmarshal(body, &m) != nil {
		return ""
	}
	code, _ := m["error_code"].(string)
	return code
}

// isKeyReused reports the server's "another body under this key" refusal. For
// a keyed write this means the write already exists: counted as written.
func isKeyReused(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusConflict {
		return false
	}
	code, _ := apiErr.Body["error_code"].(string)
	return code == idempotencyKeyReused
}

// keyReusedResult is what a keyed write returns when the server says the key
// already holds a write.
func keyReusedResult(taskID string) map[string]any {
	return map[string]any{
		"task_id":           taskID,
		"idempotent_reused": true,
		"note":              "Mesh already holds this write (same Idempotency-Key, answer lost earlier); nothing new was written.",
	}
}

// markReplayed flags a result the server answered from its stored first
// answer.
func markReplayed(result map[string]any, replayed bool) map[string]any {
	if replayed && result != nil {
		result["idempotent_replayed"] = true
	}
	return result
}
