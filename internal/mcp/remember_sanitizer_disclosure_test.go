package mcp

import (
	"strings"
	"testing"
)

// The server-side write path refuses memory content that carries an injected
// instruction or a recognisable credential (evc-mesh internal/service/
// memory_sanitizer.go). That screen is PARTIAL by construction: it keys on
// prefixes and on field names, so a bare secret value on its own line — the
// exact shape of the Casdoor client_secret incident (#d2f79c73) — passes it
// untouched.
//
// A partial control that is believed to be total is worse than no control,
// because it converts "do not paste secrets" into "the tool will catch it".
// The `remember` description is the only place an agent ever reads about this,
// so the disclosure is pinned here rather than left to survive review.
func TestRememberDescription_DisclosesSanitizerAndItsLimits(t *testing.T) {
	server := NewServer(ServerConfig{})

	tool, ok := server.MCPServer().ListTools()["remember"]
	if !ok {
		t.Fatal("remember tool is not registered")
	}
	desc := tool.Tool.Description

	// The screen exists and refuses rather than silently editing.
	for _, want := range []string{"REFUSED", "named reason"} {
		if !strings.Contains(desc, want) {
			t.Errorf("description must say the write is refused with a reason; missing %q\ngot: %s", want, desc)
		}
	}
	if strings.Contains(desc, "silently stripped") && !strings.Contains(desc, "never silently stripped") {
		t.Error("description must not imply content is stripped — it is refused")
	}

	// AC3: the limitation must be stated, not merely implied.
	if !strings.Contains(desc, "LIMITATION") {
		t.Errorf("description must carry an explicit LIMITATION clause\ngot: %s", desc)
	}
	lower := strings.ToLower(desc)
	for _, want := range []string{
		"no recognisable prefix", // shape-based half is narrow
		"no field name",          // name-based half needs a name
		"cannot",                 // stated as an inability, not a caveat
	} {
		if !strings.Contains(lower, want) {
			t.Errorf("description must state that a shapeless, unnamed secret is NOT caught; missing %q\ngot: %s", want, desc)
		}
	}

	// It must not claim safety.
	for _, forbidden := range []string{"all secrets", "any secret", "guarantees", "is safe"} {
		if strings.Contains(lower, forbidden) {
			t.Errorf("description overclaims with %q — the screen is partial\ngot: %s", forbidden, desc)
		}
	}
}
