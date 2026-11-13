package mcp

import (
	"crypto/sha256"
	"encoding/json"
	"sync"
)

// taskReadCache remembers, per task, what this MCP session already handed to
// its caller through get_task, so a repeat read of the same card returns only
// what changed. In 24h of agent session transcripts 684 get_task
// calls re-read a card the same session had already read — 4.36M characters,
// 1.97M of them unchanged descriptions — while only 93 of 1934 calls passed
// `since`. The explicit `since` never caught on, so the server tracks it.
//
// Enabled only for stdio, where one process serves exactly one client
// session; an SSE/HTTP server is shared by many clients and must not decide
// what a caller has already seen. full=true and an explicit since bypass it.
type taskReadCache struct {
	mu    sync.Mutex
	tasks map[string]*taskReadMemo
}

// taskReadMemo is what the session has seen of one task.
type taskReadMemo struct {
	taskSig [32]byte // compact task object as last returned
	descSig [32]byte // description as last returned
	hasDesc bool
	// Comment ids already returned, per representation: an automation
	// comment shown as a one-line summary (include_auto=false) is not "seen"
	// for a later include_auto=true read that asks for its whole body.
	commentsSummary map[string]struct{}
	commentsFull    map[string]struct{}
}

// taskReadCacheMaxTasks bounds memory in a long session: past it the cache
// starts over, which only costs one more full read per card.
const taskReadCacheMaxTasks = 2000

// taskUnchangedNote tells the caller why the card came back as a stub and how
// to get it whole again (e.g. after its own context was compacted).
const taskUnchangedNote = "unchanged since your earlier get_task of this card in this session: description and other fields omitted; full=true returns the whole card"

func newTaskReadCache() *taskReadCache {
	return &taskReadCache{tasks: map[string]*taskReadMemo{}}
}

func jsonSig(v any) [32]byte {
	b, _ := json.Marshal(v)
	return sha256.Sum256(b)
}

// taskReadPlan is one get_task call's view of the cache. The handler shapes
// its response through it and calls commit only when that response is
// actually returned: a call that ends in an error records nothing, so the
// next read is not trimmed against data the caller never received.
type taskReadPlan struct {
	cache       *taskReadCache
	taskID      string
	prior       *taskReadMemo // snapshot; nil on the first read
	taskSet     bool
	taskSig     [32]byte
	descSig     [32]byte
	hasDesc     bool
	includeAuto bool
	shownIDs    []string
}

// plan snapshots what the session has already seen of taskID.
func (c *taskReadCache) plan(taskID string) *taskReadPlan {
	c.mu.Lock()
	defer c.mu.Unlock()
	p := &taskReadPlan{cache: c, taskID: taskID}
	if memo := c.tasks[taskID]; memo != nil {
		cp := *memo
		cp.commentsSummary = cloneIDSet(memo.commentsSummary)
		cp.commentsFull = cloneIDSet(memo.commentsFull)
		p.prior = &cp
	}
	return p
}

func cloneIDSet(in map[string]struct{}) map[string]struct{} {
	out := make(map[string]struct{}, len(in))
	for k := range in {
		out[k] = struct{}{}
	}
	return out
}

// applyTask decides what of the compact task to return given what the session
// saw before and writes the result into resp. Unchanged task → stub +
// task_changed=false. Changed task with the same description → task without
// description + description_unchanged=true. Otherwise (first read, or a new
// description) → the whole task.
func (p *taskReadPlan) applyTask(task map[string]any, resp map[string]any) {
	sig := jsonSig(task)
	desc, hasDesc := task["description"]
	descSig := jsonSig(desc)
	memo := p.prior
	switch {
	case memo == nil || (memo.taskSig == [32]byte{} && !memo.hasDesc):
		resp["task"] = task
	case memo.taskSig == sig:
		resp["task"] = taskDeltaStub(task)
		resp["task_changed"] = false
		resp["task_note"] = taskUnchangedNote
	case hasDesc && memo.hasDesc && memo.descSig == descSig:
		lean := make(map[string]any, len(task))
		for k, v := range task {
			if k != "description" {
				lean[k] = v
			}
		}
		resp["task"] = lean
		resp["task_changed"] = true
		resp["description_unchanged"] = true
	default:
		resp["task"] = task
		resp["task_changed"] = true
	}
	p.taskSet, p.taskSig, p.descSig, p.hasDesc = true, sig, descSig, hasDesc
}

// filterComments drops comments this session already received in the same
// or a fuller representation and remembers the rest for commit. It returns
// the remaining comments and how many were dropped.
func (p *taskReadPlan) filterComments(items []any, includeAuto bool) ([]any, int) {
	p.includeAuto = includeAuto
	out := make([]any, 0, len(items))
	dropped := 0
	for _, it := range items {
		id := ""
		if m, ok := it.(map[string]any); ok {
			id, _ = m["id"].(string)
		}
		if id != "" && p.prior != nil {
			_, full := p.prior.commentsFull[id]
			_, summary := p.prior.commentsSummary[id]
			if full || (!includeAuto && summary) {
				dropped++
				continue
			}
		}
		if id != "" {
			p.shownIDs = append(p.shownIDs, id)
		}
		out = append(out, it)
	}
	return out, dropped
}

// commit records what this call returned. Call it only on success.
func (p *taskReadPlan) commit() {
	c := p.cache
	c.mu.Lock()
	defer c.mu.Unlock()
	memo := c.tasks[p.taskID]
	if memo == nil {
		if len(c.tasks) >= taskReadCacheMaxTasks {
			c.tasks = map[string]*taskReadMemo{}
		}
		memo = &taskReadMemo{commentsSummary: map[string]struct{}{}, commentsFull: map[string]struct{}{}}
		c.tasks[p.taskID] = memo
	}
	if p.taskSet {
		memo.taskSig, memo.descSig, memo.hasDesc = p.taskSig, p.descSig, p.hasDesc
	}
	set := memo.commentsSummary
	if p.includeAuto {
		set = memo.commentsFull
	}
	for _, id := range p.shownIDs {
		set[id] = struct{}{}
	}
}
