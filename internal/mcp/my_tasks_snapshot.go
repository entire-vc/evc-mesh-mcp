package mcp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
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

// envMyTasksCold=full restores the previous cold envelope (descriptions
// inline). Rollback switch for the compact cold default; the revision and the
// hit/unchanged/delta protocol are identical in both modes.
const envMyTasksCold = "MESH_MCP_MY_TASKS_COLD"

const myTasksDescriptionNote = "descriptions not included: get_task(id) or full=true"

// myTasksKeepKeys are the drop-list fields the compact view restores (url and,
// when set, parent_task_id); the rollback view keeps the previous lean shape.
// url stays through the lean pass so slimMyTasksItems can test it against id.
func myTasksKeepKeys() []string {
	if myTasksColdFull() {
		return nil
	}
	return []string{"url", "parent_task_id"}
}

// myTasksColdDropWhenEmpty are per-item fields the compact view omits while
// they hold the default (a live 50-card page carried checkout_generation:0 and
// vcs_pipeline_status:null on every card); a real value still shows.
var myTasksColdDropWhenEmpty = []string{"checkout_generation", "vcs_pipeline_status"}

// slimMyTasksItems drops what the reader can rebuild: url when it equals
// <prefix><id> on every item (the prefix moves to the envelope as url_template),
// and default-valued fields. Only the Mesh task prefix (allowed) is hoisted: an
// item whose url deviates keeps it, and so does the whole page when a prefix
// differs or is foreign. Returns the template or "".
func slimMyTasksItems(tasks []any, allowed string) string {
	prefix, uniform := "", true
	for _, v := range tasks {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		u, _ := m["url"].(string)
		id, _ := m["id"].(string)
		if u == "" || id == "" || !strings.HasSuffix(u, id) {
			uniform = false
			continue
		}
		p := strings.TrimSuffix(u, id)
		if p == "" {
			// url == id: no prefix to hoist, and "" must not read as "unset".
			uniform = false
			continue
		}
		if prefix == "" {
			prefix = p
		} else if p != prefix {
			uniform = false
		}
	}
	for _, v := range tasks {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if u, _ := m["url"].(string); uniform && prefix != "" && prefix == allowed && u == prefix+fmt.Sprint(m["id"]) {
			delete(m, "url")
		}
		for _, k := range myTasksColdDropWhenEmpty {
			if isEmptyJSONValue(m[k]) {
				delete(m, k)
			}
		}
	}
	if uniform && prefix != "" && prefix == allowed {
		return prefix + "{id}"
	}
	return ""
}

func myTasksColdFull() bool {
	return strings.EqualFold(strings.TrimSpace(os.Getenv(envMyTasksCold)), "full")
}

// compactMyTasksCold returns the cold response without task descriptions. The
// revision was taken over the complete body, so it matches unchanged/delta
// exchanges; items carry has_description and the envelope states the omission,
// so a missing description is never mistaken for an empty one.
func (s *Server) compactMyTasksCold(ctx context.Context, request mcpsdk.CallToolRequest, result map[string]any) map[string]any {
	if myTasksColdFull() {
		return result
	}
	tasks, _ := result["tasks"].([]any)
	compact := make([]any, 0, len(tasks))
	for _, value := range tasks {
		item, ok := value.(map[string]any)
		if !ok {
			compact = append(compact, value)
			continue
		}
		cp := make(map[string]any, len(item))
		for k, v := range item {
			cp[k] = v
		}
		if desc, _ := cp["description"].(string); desc != "" {
			cp["has_description"] = true
		}
		delete(cp, "description")
		delete(cp, "description_truncated")
		compact = append(compact, cp)
	}
	out := make(map[string]any, len(result)+2)
	for k, v := range result {
		out[k] = v
	}
	// An explicit status_category filter is the caller's own narrowing.
	if mcpsdk.ParseString(request, "status_category", "") == "" {
		if active, omitted, err := s.activeOnlyMyTasks(ctx, compact); err != nil {
			// Never a silently unfiltered page: the envelope says the active-only
			// view could not be built and why; full=true needs no lookup.
			out["omitted_unavailable"] = err.Error()
			out["omitted_note"] = myTasksOmittedNote
		} else {
			compact = active
			out["omitted"] = omitted
			out["omitted_note"] = myTasksOmittedNote
			out["count"] = len(compact)
		}
	}
	out["tasks"] = compact
	out["description_omitted"] = myTasksDescriptionNote
	return out
}

const myTasksColdTodoCap = 5

const myTasksOmittedNote = "cold view lists in_progress, review and the top todo only; full=true or get_my_tasks(status_category=...) / list_tasks returns the rest"

// myTasksCats memoizes status categories (and lookup failures) for ONE request,
// carried in the context: the cold view and the delta built for the same call
// must see the same lookup outcome.
type myTasksCatsKey struct{}

type myTasksCats struct {
	cats   map[string]map[string]string
	failed map[string]error
}

func (m *myTasksCats) byProject() map[string]map[string]string {
	if m.cats == nil {
		m.cats = map[string]map[string]string{}
	}
	return m.cats
}

func (m *myTasksCats) fail(project string, err error) {
	if m.failed == nil {
		m.failed = map[string]error{}
	}
	m.failed[project] = err
}

var myTasksPriorityRank = map[string]int{"urgent": 0, "high": 1, "medium": 2, "low": 3}

// activeOnlyMyTasks keeps in_progress and review items plus the top
// myTasksColdTodoCap todo by priority, and counts the rest per category. A
// category that cannot be resolved is an error; the caller reports it in the
// envelope (omitted_unavailable) instead of passing the page off as filtered.
func (s *Server) activeOnlyMyTasks(ctx context.Context, tasks []any) ([]any, map[string]int, error) {
	memo, _ := ctx.Value(myTasksCatsKey{}).(*myTasksCats)
	if memo == nil {
		memo = &myTasksCats{}
	}
	cats := memo.byProject() // project -> status_id -> category
	client := s.getRESTClient(ctx)
	catOf := make([]string, len(tasks))
	for i, v := range tasks {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, nil, fmt.Errorf("task item is not an object")
		}
		pid, _ := m["project_id"].(string)
		sid, _ := m["status_id"].(string)
		if pid == "" || sid == "" {
			return nil, nil, fmt.Errorf("task %v has no project_id/status_id to resolve its category", m["id"])
		}
		if err := memo.failed[pid]; err != nil {
			return nil, nil, err
		}
		lookup, done := cats[pid]
		if !done {
			lookup = map[string]string{}
			list, err := client.GetProjectStatuses(ctx, pid)
			var apiErr *APIError
			if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden {
				list, err = client.GetTaskStatuses(ctx, fmt.Sprint(m["id"]))
			}
			if err != nil {
				err = fmt.Errorf("status lookup for project %s: %w", pid, err)
				memo.fail(pid, err)
				return nil, nil, err
			}
			for _, st := range list {
				id, _ := st["id"].(string)
				cat, _ := st["category"].(string)
				lookup[id] = cat
			}
			cats[pid] = lookup
		}
		if catOf[i] = lookup[sid]; catOf[i] == "" {
			return nil, nil, fmt.Errorf("status %s of project %s has no category", sid, pid)
		}
	}
	var todo []int
	keep := make([]bool, len(tasks))
	for i, c := range catOf {
		switch c {
		case "in_progress", "review":
			keep[i] = true
		case "todo":
			todo = append(todo, i)
		}
	}
	rank := func(i int) int {
		p, _ := tasks[i].(map[string]any)["priority"].(string)
		if r, ok := myTasksPriorityRank[p]; ok {
			return r
		}
		return len(myTasksPriorityRank)
	}
	sort.SliceStable(todo, func(a, b int) bool { return rank(todo[a]) < rank(todo[b]) })
	for n, i := range todo {
		if n < myTasksColdTodoCap {
			keep[i] = true
		}
	}
	out := make([]any, 0, len(tasks))
	omitted := map[string]int{}
	for i, v := range tasks {
		if keep[i] {
			out = append(out, v)
		} else {
			omitted[catOf[i]]++
		}
	}
	return out, omitted, nil
}

func (s *Server) renderMyTasksCold(ctx context.Context, request mcpsdk.CallToolRequest, result map[string]any) (*mcpsdk.CallToolResult, error) {
	return jsonResult(s.compactMyTasksCold(ctx, request, result))
}

func (s *Server) renderMyTasks(ctx context.Context, request mcpsdk.CallToolRequest, params map[string]string, result map[string]any) (*mcpsdk.CallToolResult, error) {
	ctx = context.WithValue(ctx, myTasksCatsKey{}, &myTasksCats{})
	if mcpsdk.ParseBoolean(request, "full", false) {
		return jsonResult(result)
	}
	versioned, err := validateMyTasksSnapshot(result)
	if err != nil {
		return errResult("failed to get complete tasks: %v", err)
	}
	result["tasks"] = leanTaskSummariesKeep(result["tasks"].([]any), myTasksKeepKeys()...)
	if !myTasksColdFull() {
		if tpl := slimMyTasksItems(result["tasks"].([]any), s.getRESTClient(ctx).taskURLPrefix()); tpl != "" {
			result["url_template"] = tpl
		}
	}
	if !versioned {
		return s.renderMyTasksCold(ctx, request, result)
	}
	body, err := json.Marshal(result)
	if err != nil {
		return errResult("failed to render tasks: %v", err)
	}
	scope := s.myTasksSnapshots.scope(ctx, s.getSession(ctx), s.getRESTClient(ctx), params, mcpsdk.ParseString(request, "snapshot_session", ""))
	if scope == "" {
		return s.renderMyTasksCold(ctx, request, result)
	}
	revision := myTasksRevision(scope, body)
	known := mcpsdk.ParseString(request, "known_revision", "")
	base := s.myTasksSnapshots.exchange(scope, revision, known, body, time.Now())
	if len(base) != 0 && bytes.Equal(base, body) {
		return jsonResult(map[string]any{"unchanged": true, "revision": revision})
	}
	if len(base) != 0 && mcpsdk.ParseBoolean(request, "accept_delta", false) {
		// The consumer holds the cold view, so both sides of the delta are put
		// through the same active-only selection.
		view := func(tasks []any) ([]any, error) {
			if myTasksColdFull() || mcpsdk.ParseString(request, "status_category", "") != "" {
				return tasks, nil
			}
			active, _, err := s.activeOnlyMyTasks(ctx, tasks)
			if err != nil {
				return tasks, nil // the cold response is the whole page in that case too
			}
			return active, nil
		}
		delta, err := myTasksDelta(base, result, known, revision, view)
		if err == nil && !myTasksColdFull() {
			// The envelope the consumer rebuilds is the cold one: it must carry the
			// same omitted/omitted_unavailable keys as a fresh cold response.
			cold := s.compactMyTasksCold(ctx, request, result)
			env := delta["envelope"].(map[string]any)
			for _, k := range []string{"omitted", "omitted_note", "omitted_unavailable", "count"} {
				if v, ok := cold[k]; ok {
					env[k] = v
				}
			}
		}
		if err == nil {
			return jsonResult(delta)
		}
		// Corrupt/obsolete base falls back to complete cold.
	}
	result["revision"] = revision
	return s.renderMyTasksCold(ctx, request, result)
}

func myTasksDelta(base []byte, current map[string]any, known, revision string, view func([]any) ([]any, error)) (map[string]any, error) {
	var previous map[string]any
	if err := json.Unmarshal(base, &previous); err != nil {
		return nil, err
	}
	oldTasks, ok := previous["tasks"].([]any)
	if !ok {
		return nil, fmt.Errorf("invalid base tasks")
	}
	oldTasks, err := view(oldTasks)
	if err != nil {
		return nil, err
	}
	currentTasks, err := view(current["tasks"].([]any))
	if err != nil {
		return nil, err
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
	for _, value := range currentTasks {
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
	if !myTasksColdFull() {
		// The consumer's retained snapshot is the compact cold: keep its notice.
		envelope["description_omitted"] = myTasksDescriptionNote
	}
	return map[string]any{"delta": true, "base_revision": known, "revision": revision, "changed": changed, "removed_ids": removed, "order": order, "envelope": envelope}, nil
}
