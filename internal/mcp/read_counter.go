package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// readToolNames lists the MCP tools counted as memory-read operations.
var readToolNames = map[string]bool{
	"recall":                true,
	"get_project_knowledge": true,
	"get_context":           true,
	"get_canonical_updates": true,
	"get_canonical":         true,
	"get_task_context":      true,
}

// ReadCounter tracks per-agent, per-tool memory-read calls in a thread-safe map.
// It is shared across all agent connections in SSE mode.
type ReadCounter struct {
	mu        sync.Mutex
	byAgent   map[string]map[string]int64 // agentID → toolName → count
	byTool    map[string]int64            // toolName → total count
	startedAt time.Time
}

// NewReadCounter initialises a ReadCounter with the current time as epoch.
func NewReadCounter() *ReadCounter {
	return &ReadCounter{
		byAgent:   make(map[string]map[string]int64),
		byTool:    make(map[string]int64),
		startedAt: time.Now().UTC(),
	}
}

// Inc records one read call for the given agent and tool.
func (rc *ReadCounter) Inc(agentID, toolName string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.byAgent[agentID] == nil {
		rc.byAgent[agentID] = make(map[string]int64)
	}
	rc.byAgent[agentID][toolName]++
	rc.byTool[toolName]++
}

// readCounterSnapshot is the JSON-serialisable view of ReadCounter.
type readCounterSnapshot struct {
	GeneratedAt string                       `json:"generated_at"`
	StartedAt   string                       `json:"started_at"`
	TotalByTool map[string]int64             `json:"total_by_tool"`
	ByAgent     map[string]map[string]int64  `json:"by_agent"`
}

// Snapshot returns a consistent copy of all counters suitable for JSON marshalling.
func (rc *ReadCounter) Snapshot() readCounterSnapshot {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	byTool := make(map[string]int64, len(rc.byTool))
	for k, v := range rc.byTool {
		byTool[k] = v
	}
	byAgent := make(map[string]map[string]int64, len(rc.byAgent))
	for agent, tools := range rc.byAgent {
		cp := make(map[string]int64, len(tools))
		for t, c := range tools {
			cp[t] = c
		}
		byAgent[agent] = cp
	}
	return readCounterSnapshot{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		StartedAt:   rc.startedAt.Format(time.RFC3339),
		TotalByTool: byTool,
		ByAgent:     byAgent,
	}
}

// WriteFile atomically writes the counter snapshot as JSON to path.
// Parent directories are created if they do not exist.
func (rc *ReadCounter) WriteFile(path string) error {
	snap := rc.Snapshot()
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// Write to a temp file then rename for atomicity.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
