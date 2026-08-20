package mcp

import (
	"strings"
	"testing"
)

// The server can refuse a stale conditional write and can record a reason, but
// an agent can only use either through this tool's schema. A parameter that
// exists on the API and not here is, from the fleet's point of view, a feature
// that does not exist — which is exactly why MESH_MEMORY_REQUIRE_REASON cannot
// be switched on until this ships.
func TestRememberTool_ExposesReasonAndExpectedVersion(t *testing.T) {
	server := NewServer(ServerConfig{})

	tool, ok := server.MCPServer().ListTools()["remember"]
	if !ok {
		t.Fatal("remember tool is not registered")
	}
	props := tool.Tool.InputSchema.Properties

	for _, name := range []string{"reason", "expected_version"} {
		raw, present := props[name]
		if !present {
			t.Fatalf("remember must accept %q, otherwise no agent can send it", name)
		}
		spec, _ := raw.(map[string]any)
		desc, _ := spec["description"].(string)
		if strings.TrimSpace(desc) == "" {
			t.Errorf("%q needs a description: an undocumented parameter is one nobody passes", name)
		}
	}

	// Neither may be required. Making them required here would reject every
	// existing caller at the tool boundary — the same fleet-wide breakage the
	// server-side flag is staged to avoid, just moved one layer out.
	for _, req := range tool.Tool.InputSchema.Required {
		if req == "reason" || req == "expected_version" {
			t.Errorf("%q must stay optional until adoption is measured", req)
		}
	}

	reasonDesc, _ := props["reason"].(map[string]any)["description"].(string)
	if !strings.Contains(strings.ToLower(reasonDesc), "required") {
		t.Errorf("the reason description should warn that it is about to become "+
			"required, or agents have no cue to start sending it\ngot: %s", reasonDesc)
	}

	verDesc, _ := props["expected_version"].(map[string]any)["description"].(string)
	lower := strings.ToLower(verDesc)
	for _, want := range []string{"refused", "conditional"} {
		if !strings.Contains(lower, want) {
			t.Errorf("expected_version must say the write is refused on mismatch, "+
				"not silently retried or ignored; missing %q\ngot: %s", want, verDesc)
		}
	}
}
