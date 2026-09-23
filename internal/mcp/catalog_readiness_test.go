package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// Every registered tool must carry an explicit behaviour classification, so
// clients and catalogs never fall back to the "may be destructive" default
// for a tool that only reads.
func TestEveryToolIsAnnotated(t *testing.T) {
	srv := NewServer(ServerConfig{Profile: ProfileFull})
	tools := srv.MCPServer().ListTools()
	if len(tools) == 0 {
		t.Fatal("no tools registered — the check below would pass vacuously")
	}
	for name, st := range tools {
		if _, ok := toolKinds[name]; !ok {
			t.Errorf("tool %q has no entry in toolKinds", name)
			continue
		}
		a := st.Tool.Annotations
		if a.ReadOnlyHint == nil || a.DestructiveHint == nil || a.IdempotentHint == nil || a.OpenWorldHint == nil {
			t.Errorf("tool %q: annotation hints not all set: %+v", name, a)
		}
	}
	for name := range toolKinds {
		if _, ok := tools[name]; !ok {
			t.Errorf("toolKinds has %q, but no such tool is registered", name)
		}
	}
}

func TestAnnotationsMatchKind(t *testing.T) {
	srv := NewServer(ServerConfig{Profile: ProfileFull})
	tools := srv.MCPServer().ListTools()
	cases := map[string][3]bool{ // readOnly, destructive, idempotent
		"get_task":    {true, false, true},
		"create_task": {false, false, false},
		"heartbeat":   {false, false, true},
		"forget":      {false, true, false},
		"update_task": {false, true, true},
	}
	for name, want := range cases {
		a := tools[name].Tool.Annotations
		got := [3]bool{*a.ReadOnlyHint, *a.DestructiveHint, *a.IdempotentHint}
		if got != want {
			t.Errorf("%s: got readOnly/destructive/idempotent=%v, want %v", name, got, want)
		}
	}
}

func rpc(t *testing.T, srv *Server, msg string) map[string]any {
	t.Helper()
	resp := srv.MCPServer().HandleMessage(context.Background(), json.RawMessage(msg))
	b, err := json.Marshal(resp)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// Without credentials the server must still initialize and list its tools,
// and a tool call must return setup instructions instead of reaching the API.
func TestUnconfiguredServerListsToolsAndExplainsSetup(t *testing.T) {
	const hint = "EVC Mesh is not configured yet: set MESH_API_URL and MESH_AGENT_KEY"
	srv := NewServer(ServerConfig{Profile: ProfileCore, SetupHint: hint})

	init := rpc(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"Catalog Inspector","version":"1.0"}}}`)
	if init["error"] != nil {
		t.Fatalf("initialize failed: %v", init["error"])
	}

	list := rpc(t, srv, `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) < 10 {
		t.Fatalf("tools/list returned %d tools, want the core set", len(tools))
	}

	call := rpc(t, srv, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"get_my_tasks","arguments":{}}}`)
	res, _ := call["result"].(map[string]any)
	if res == nil || res["isError"] != true {
		t.Fatalf("tool call in unconfigured mode must be an error result, got %v", call)
	}
	if !strings.Contains(mustJSON(t, res), "MESH_AGENT_KEY") {
		t.Fatalf("error result does not carry the setup hint: %v", res)
	}
}

func TestInitializeCountsClient(t *testing.T) {
	srv := NewServer(ServerConfig{Profile: ProfileCore, SetupHint: "x"})
	before := testutil.ToFloat64(initializeTotal.WithLabelValues("catalog-inspector-test", ProfileCore))
	rpc(t, srv, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"Catalog Inspector Test","version":"1.0"}}}`)
	after := testutil.ToFloat64(initializeTotal.WithLabelValues("catalog-inspector-test", ProfileCore))
	if after-before != 1 {
		t.Fatalf("initialize counter moved by %v, want 1", after-before)
	}
}

func TestClientLabelIsBounded(t *testing.T) {
	t.Cleanup(func() {
		clientLabelsMu.Lock()
		clientLabels = map[string]struct{}{}
		clientLabelsMu.Unlock()
	})
	if got := clientLabel("  Claude Code  "); got != "claude-code" {
		t.Errorf("clientLabel = %q", got)
	}
	if got := clientLabel("x\x00y<script>"); got != "xyscript" {
		t.Errorf("clientLabel did not strip unsafe chars: %q", got)
	}
	if got := clientLabel(strings.Repeat("a", 200)); len(got) > 40 {
		t.Errorf("clientLabel length %d > 40", len(got))
	}
	if got := clientLabel(""); got != "unknown" {
		t.Errorf("empty name → %q", got)
	}
	for i := 0; i < maxClientLabels+5; i++ {
		clientLabel("flood-" + strings.Repeat("z", i%30) + string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}
	if got := clientLabel("brand-new-client-after-flood"); got != "other" {
		t.Errorf("label cap not enforced: %q", got)
	}
}

func TestServerVersionIsNotHardcoded(t *testing.T) {
	init := rpc(t, NewServer(ServerConfig{SetupHint: "x"}), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"1"}}}`)
	info := init["result"].(map[string]any)["serverInfo"].(map[string]any)
	if info["version"] == "0.1.0" || info["version"] != ServerVersion() {
		t.Errorf("serverInfo.version = %v, want ServerVersion() = %q", info["version"], ServerVersion())
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
