package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
)

const myTasksSchema = "my-tasks-v1"
const myTasksCacheBytes = 32 << 20
const myTasksCacheEntries = 128
const myTasksCacheTTL = 30 * time.Minute

type myTasksBase struct {
	scope   string
	body    []byte
	expires time.Time
}

// Only immutable, complete rendered snapshots enter this bounded cache. It is
// never a source of current data: every call reads the authenticated API first.
type myTasksSnapshots struct {
	mu    sync.Mutex
	nonce string
	bases map[string]myTasksBase
	bytes int
}

func (c *myTasksSnapshots) scope(ctx context.Context, session *AgentSession, client *RESTClient, params map[string]string, callerSession string) string {
	c.mu.Lock()
	if c.nonce == "" {
		c.nonce = uuid.NewString()
	}
	nonce := c.nonce
	c.mu.Unlock()
	transport := "stdio"
	if s := mcpserver.ClientSessionFromContext(ctx); s != nil {
		transport = s.SessionID()
		if transport == "" && callerSession == "" {
			return "" // Stateless HTTP needs an explicit consumer cache namespace.
		}
	}
	// The authentication carrier itself never enters the public digest. The
	// authenticated identity and API origin isolate tenants and environments.
	b, _ := json.Marshal([]any{myTasksSchema, nonce, session.WorkspaceID.String(), session.AgentID.String(), client.baseURL, transport, callerSession, params, "lean-complete"})
	// Retain a fixed-size scope even when request filters or caller namespace
	// are large. The cache byte bound counts snapshot bodies.
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:])
}

func (c *myTasksSnapshots) exchange(scope, revision, known string, body []byte, now time.Time) []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.bases == nil {
		c.bases = make(map[string]myTasksBase)
	}
	for key, base := range c.bases {
		if !now.Before(base.expires) {
			c.bytes -= len(base.body)
			delete(c.bases, key)
		}
	}
	var previous []byte
	if base, ok := c.bases[known]; ok && base.scope == scope && myTasksRevision(scope, base.body) == known {
		previous = base.body
	}
	// An oversized snapshot remains a complete cold response but cannot become
	// a conditional base. Evict oldest expirations until both bounds hold.
	if len(body) > myTasksCacheBytes {
		return nil
	}
	// Replace any old entry before accounting/eviction, including a corrupt
	// entry whose size differs from its original canonical snapshot.
	if old, exists := c.bases[revision]; exists {
		c.bytes -= len(old.body)
		delete(c.bases, revision)
	}
	for len(c.bases) >= myTasksCacheEntries || c.bytes+len(body) > myTasksCacheBytes {
		oldest := ""
		for key, base := range c.bases {
			if oldest == "" || base.expires.Before(c.bases[oldest].expires) {
				oldest = key
			}
		}
		c.bytes -= len(c.bases[oldest].body)
		delete(c.bases, oldest)
	}
	c.bytes += len(body)
	c.bases[revision] = myTasksBase{scope: scope, body: bytes.Clone(body), expires: now.Add(myTasksCacheTTL)}
	return previous
}

// Validate completeness before advertising a reusable base. If a future API
// trims text, fail closed rather than labelling that partial page complete.
// Missing authoritative child versions permits cold, never shortcuts.
func validateMyTasksSnapshot(result map[string]any) (bool, error) {
	if result["truncated"] == true {
		return false, fmt.Errorf("upstream task page is truncated; complete snapshot required")
	}
	tasks, ok := result["tasks"].([]any)
	if !ok {
		return false, fmt.Errorf("upstream tasks is not an array")
	}
	versioned := true
	ids := make(map[string]bool, len(tasks))
	for _, value := range tasks {
		item, ok := value.(map[string]any)
		if !ok {
			return false, fmt.Errorf("upstream task is not an object")
		}
		id, _ := item["id"].(string)
		if id == "" || ids[id] {
			return false, fmt.Errorf("upstream task IDs are missing or duplicated")
		}
		ids[id] = true
		desc, _ := item["description"].(string)
		if item["description_truncated"] == true || (item["has_description"] == true && desc == "") {
			return false, fmt.Errorf("upstream task description is incomplete")
		}
		version, ok := item["version"].(float64)
		if !ok || version < 1 || version != float64(int64(version)) {
			versioned = false
		}
	}
	return versioned, nil
}

func myTasksRevision(scope string, body []byte) string {
	digest := sha256.New()
	_, _ = digest.Write([]byte(scope))
	_, _ = digest.Write([]byte{0})
	_, _ = digest.Write(body)
	return hex.EncodeToString(digest.Sum(nil))
}

func (s *Server) renderMyTasks(ctx context.Context, request mcpsdk.CallToolRequest, params map[string]string, result map[string]any) (*mcpsdk.CallToolResult, error) {
	if mcpsdk.ParseBoolean(request, "full", false) {
		return jsonResult(result)
	}
	versioned, err := validateMyTasksSnapshot(result)
	if err != nil {
		return errResult("failed to get complete tasks: %v", err)
	}
	result["tasks"] = leanTaskSummaries(result["tasks"].([]any))
	if !versioned {
		return jsonResult(result)
	}
	body, err := json.Marshal(result)
	if err != nil {
		return errResult("failed to render tasks: %v", err)
	}
	scope := s.myTasksSnapshots.scope(ctx, s.getSession(ctx), s.getRESTClient(ctx), params, mcpsdk.ParseString(request, "snapshot_session", ""))
	if scope == "" {
		return jsonResult(result)
	}
	revision := myTasksRevision(scope, body)
	known := mcpsdk.ParseString(request, "known_revision", "")
	base := s.myTasksSnapshots.exchange(scope, revision, known, body, time.Now())
	if len(base) != 0 && bytes.Equal(base, body) {
		return jsonResult(map[string]any{"unchanged": true, "revision": revision})
	}
	if len(base) != 0 && mcpsdk.ParseBoolean(request, "accept_delta", false) {
		delta, err := myTasksDelta(base, result, known, revision)
		if err == nil {
			return jsonResult(delta)
		}
		// Corrupt/obsolete base falls back to complete cold.
	}
	result["revision"] = revision
	return jsonResult(result)
}

func myTasksDelta(base []byte, current map[string]any, known, revision string) (map[string]any, error) {
	var previous map[string]any
	if err := json.Unmarshal(base, &previous); err != nil {
		return nil, err
	}
	oldTasks, ok := previous["tasks"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid base tasks")
	}
	old := make(map[string][]byte, len(oldTasks))
	oldOrder := make([]string, 0, len(oldTasks))
	for _, value := range oldTasks {
		item, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("invalid base item")
		}
		id, _ := item["id"].(string)
		if id == "" || old[id] != nil {
			return nil, fmt.Errorf("invalid base IDs")
		}
		b, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		old[id] = b
		oldOrder = append(oldOrder, id)
	}
	changed := make([]any, 0)
	order := make([]string, 0)
	seen := make(map[string]bool)
	for _, value := range current["tasks"].([]any) {
		item := value.(map[string]any)
		id := item["id"].(string)
		order = append(order, id)
		seen[id] = true
		b, err := json.Marshal(item)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(old[id], b) {
			changed = append(changed, item)
		}
	}
	removed := make([]string, 0)
	for _, id := range oldOrder {
		if !seen[id] {
			removed = append(removed, id)
		}
	}
	envelope := make(map[string]any)
	for key, value := range current {
		if key != "tasks" {
			envelope[key] = value
		}
	}
	return map[string]any{"delta": true, "base_revision": known, "revision": revision, "changed": changed, "removed_ids": removed, "order": order, "envelope": envelope}, nil
}
