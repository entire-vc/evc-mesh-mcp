package mcp

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestReadCounterInc(t *testing.T) {
	rc := NewReadCounter()
	rc.Inc("agent-1", "recall")
	rc.Inc("agent-1", "recall")
	rc.Inc("agent-1", "get_project_knowledge")
	rc.Inc("agent-2", "recall")

	snap := rc.Snapshot()

	if snap.ByAgent["agent-1"]["recall"] != 2 {
		t.Errorf("agent-1 recall: want 2, got %d", snap.ByAgent["agent-1"]["recall"])
	}
	if snap.ByAgent["agent-1"]["get_project_knowledge"] != 1 {
		t.Errorf("agent-1 get_project_knowledge: want 1, got %d", snap.ByAgent["agent-1"]["get_project_knowledge"])
	}
	if snap.ByAgent["agent-2"]["recall"] != 1 {
		t.Errorf("agent-2 recall: want 1, got %d", snap.ByAgent["agent-2"]["recall"])
	}
	if snap.TotalByTool["recall"] != 3 {
		t.Errorf("total recall: want 3, got %d", snap.TotalByTool["recall"])
	}
	if snap.TotalByTool["get_project_knowledge"] != 1 {
		t.Errorf("total get_project_knowledge: want 1, got %d", snap.TotalByTool["get_project_knowledge"])
	}
}

func TestReadCounterConcurrent(t *testing.T) {
	rc := NewReadCounter()
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc.Inc("agent-x", "recall")
		}()
	}
	wg.Wait()

	snap := rc.Snapshot()
	if snap.TotalByTool["recall"] != 100 {
		t.Errorf("concurrent total: want 100, got %d", snap.TotalByTool["recall"])
	}
}

func TestReadCounterWriteFile(t *testing.T) {
	rc := NewReadCounter()
	rc.Inc("agent-1", "recall")
	rc.Inc("agent-2", "get_canonical_updates")

	dir := t.TempDir()
	path := filepath.Join(dir, "mcp-read-counter.json")

	if err := rc.WriteFile(path); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}

	var snap readCounterSnapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}

	if snap.TotalByTool["recall"] != 1 {
		t.Errorf("file total recall: want 1, got %d", snap.TotalByTool["recall"])
	}
	if snap.GeneratedAt == "" {
		t.Error("generated_at is empty")
	}
}
