package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

type mcpbManifest struct {
	Server struct {
		EntryPoint string `json:"entry_point"`
		MCPConfig  struct {
			Command           string `json:"command"`
			PlatformOverrides map[string]struct {
				Command string `json:"command"`
			} `json:"platform_overrides"`
		} `json:"mcp_config"`
	} `json:"server"`
}

const launcherPath = "${__dirname}/server/evc-mesh-mcp"

// The Smithery CLI runs mcp_config.command and ignores platform_overrides, so
// a default that names one platform's binary fails everywhere else (it was the
// Linux binary, and every macOS install from the Smithery listing died with
// spawn ENOEXEC). The default must be the launcher.
func TestMCPBDefaultCommandIsLauncher(t *testing.T) {
	var m mcpbManifest
	b, err := os.ReadFile("mcpb/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if m.Server.MCPConfig.Command != launcherPath {
		t.Errorf("mcp_config.command = %q, want %q", m.Server.MCPConfig.Command, launcherPath)
	}
	if m.Server.EntryPoint != "server/evc-mesh-mcp" {
		t.Errorf("entry_point = %q, want server/evc-mesh-mcp", m.Server.EntryPoint)
	}

	// Every file the manifest points at must be put into the bundle by the
	// release workflow.
	wf, err := os.ReadFile(".github/workflows/release.yml")
	if err != nil {
		t.Fatal(err)
	}
	targets := []string{m.Server.MCPConfig.Command}
	for _, o := range m.Server.MCPConfig.PlatformOverrides {
		targets = append(targets, o.Command)
	}
	for _, c := range targets {
		rel := "bundle/" + strings.TrimPrefix(c, "${__dirname}/")
		if !writesPath(string(wf), rel) {
			t.Errorf("release.yml never writes %s, which the manifest runs", rel)
		}
	}
}

// writesPath reports whether some workflow line other than a chmod names path
// as a whole word — `chmod +x a/b-c` must not count as producing `a/b`.
func writesPath(workflow, path string) bool {
	re := regexp.MustCompile(`(^|\s)` + regexp.QuoteMeta(path) + `(\s|$)`)
	for _, line := range strings.Split(workflow, "\n") {
		if strings.Contains(line, "chmod") {
			continue
		}
		if re.MatchString(line) {
			return true
		}
	}
	return false
}

// The launcher picks the binary for the host it runs on.
func TestMCPBLauncherPicksHostBinary(t *testing.T) {
	var want string
	switch {
	case runtime.GOOS == "darwin":
		want = "evc-mesh-mcp-darwin-universal"
	case runtime.GOOS == "linux" && runtime.GOARCH == "amd64":
		want = "evc-mesh-mcp-linux-amd64"
	default:
		t.Skipf("no bundled binary for %s/%s", runtime.GOOS, runtime.GOARCH)
	}

	dir := t.TempDir()
	src, err := os.ReadFile("mcpb/launch.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "evc-mesh-mcp"), src, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"evc-mesh-mcp-darwin-universal", "evc-mesh-mcp-linux-amd64"} {
		stub := "#!/bin/sh\necho " + name + " \"$@\"\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(stub), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// Run it from elsewhere and through a relative path: the launcher must
	// resolve binaries next to itself, not in the caller's directory.
	cmd := exec.Command("./"+filepath.Base(dir)+"/evc-mesh-mcp", "--version")
	cmd.Dir = filepath.Dir(dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("launcher failed: %v\n%s", err, out)
	}
	if got := strings.TrimSpace(string(out)); got != want+" --version" {
		t.Errorf("launcher ran %q, want %q", got, want+" --version")
	}
}
