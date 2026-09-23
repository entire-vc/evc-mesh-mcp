package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	mcpserver "github.com/entire-vc/evc-mesh-mcp/internal/mcp"
)

// README's tool tables are what MCP catalogs show next to the listing, so
// they must name exactly the tools the server registers — per profile.
func TestREADMEToolTablesMatchServer(t *testing.T) {
	b, err := os.ReadFile("README.md")
	if err != nil {
		t.Fatal(err)
	}
	readme := string(b)
	coreStart := strings.Index(readme, "## MCP Tools — Core Profile")
	fullStart := strings.Index(readme, "## MCP Tools — Full Profile")
	if coreStart < 0 || fullStart < coreStart {
		t.Fatal("README tool sections not found — the check would pass vacuously")
	}
	end := strings.Index(readme[fullStart+1:], "\n## ")
	if end < 0 {
		t.Fatal("end of the full-profile section not found")
	}
	row := regexp.MustCompile("(?m)^\\| `([a-z_]+)` \\|")
	names := func(section string) []string {
		var out []string
		for _, m := range row.FindAllStringSubmatch(section, -1) {
			out = append(out, m[1])
		}
		sort.Strings(out)
		return out
	}
	readmeCore := names(readme[coreStart:fullStart])
	readmeAdv := names(readme[fullStart : fullStart+1+end])

	registered := func(profile string) map[string]bool {
		out := map[string]bool{}
		for n := range mcpserver.NewServer(mcpserver.ServerConfig{Profile: profile, SetupHint: "x"}).MCPServer().ListTools() {
			out[n] = true
		}
		return out
	}
	core, full := registered(mcpserver.ProfileCore), registered(mcpserver.ProfileFull)
	var wantCore, wantAdv []string
	for n := range full {
		if core[n] {
			wantCore = append(wantCore, n)
		} else {
			wantAdv = append(wantAdv, n)
		}
	}
	sort.Strings(wantCore)
	sort.Strings(wantAdv)

	if strings.Join(readmeCore, ",") != strings.Join(wantCore, ",") {
		t.Errorf("README core table:\n got  %v\n want %v", readmeCore, wantCore)
	}
	if strings.Join(readmeAdv, ",") != strings.Join(wantAdv, ",") {
		t.Errorf("README full-profile table:\n got  %v\n want %v", readmeAdv, wantAdv)
	}
}
