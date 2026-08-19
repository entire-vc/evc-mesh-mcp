package main

import (
	"os/exec"
	"strings"
	"testing"
)

// TestVersionFlag_PrintsBuildSHA builds the binary with an injected BuildSHA
// and confirms `--version`/`-version` print exactly that SHA and exit 0
// without requiring MESH_AGENT_KEY or network access — see task #1c602063
// (the flag didn't exist at all, so an agent had no way to tell an installed
// binary's origin short of `strings` on the host).
func TestVersionFlag_PrintsBuildSHA(t *testing.T) {
	bin := t.TempDir() + "/mesh-mcp-version-test"
	const wantSHA = "test-sha-1234567"

	build := exec.Command("go", "build",
		"-ldflags", "-X github.com/entire-vc/evc-mesh-mcp/internal/mcp.BuildSHA="+wantSHA,
		"-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	for _, flag := range []string{"--version", "-version"} {
		t.Run(flag, func(t *testing.T) {
			cmd := exec.Command(bin, flag)
			cmd.Env = []string{} // deliberately no MESH_AGENT_KEY / MESH_API_URL
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("%s exited with error: %v\noutput: %s", flag, err, out)
			}
			got := strings.TrimSpace(string(out))
			if got != wantSHA {
				t.Errorf("%s printed %q, want %q", flag, got, wantSHA)
			}
		})
	}
}

// TestVersionFlag_DefaultsToDev is the negative control: an unpinned build
// (no -ldflags override) must print "dev", never a blank string or a stale
// SHA left over from a previous build.
func TestVersionFlag_DefaultsToDev(t *testing.T) {
	bin := t.TempDir() + "/mesh-mcp-version-dev-test"
	build := exec.Command("go", "build", "-o", bin, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}

	out, err := exec.Command(bin, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("--version exited with error: %v\noutput: %s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != "dev" {
		t.Errorf("unpinned build printed %q, want \"dev\"", got)
	}
}
