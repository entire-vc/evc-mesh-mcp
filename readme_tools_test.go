package main

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"unicode/utf8"

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

// Catalogs grade every tool on its description and the descriptions of its
// parameters, and an agent picks tools by the same text. A one-line
// "Add a dependency between two tasks." scored C there, so the floor is
// enforced here rather than rediscovered from the next catalog score.
func TestToolDescriptionsMeetFloor(t *testing.T) {
	const toolMin, paramMin = 120, 8
	// Parameters a catalog grade singled out as unexplained: they must say
	// what they do, their default or their limits, not just name a type.
	detailed := map[string]bool{
		"add_dependency.depends_on_task_id":         true,
		"list_sub_agents.agent_id":                  true,
		"register_sub_agent.agent_type":             true,
		"report_error.error_message":                true,
		"update_agent_profile.max_concurrent_tasks": true,
		"import_workspace_config.yaml_content":      true,
		"list_projects.include_archived":            true,
		"recall.include_archived":                   true,
		"list_recurring_schedules.active_only":      true,
		"upload_artifact.artifact_type":             true,
	}
	const detailedMin = 60

	tools := mcpserver.NewServer(mcpserver.ServerConfig{Profile: mcpserver.ProfileFull, SetupHint: "x"}).MCPServer().ListTools()
	if len(tools) == 0 {
		t.Fatal("no tools registered — the check would pass vacuously")
	}
	seen := map[string]bool{}
	for name, st := range tools {
		if n := utf8.RuneCountInString(st.Tool.Description); n < toolMin {
			t.Errorf("%s: description is %d chars, want >= %d", name, n, toolMin)
		}
		for pn, raw := range st.Tool.InputSchema.Properties {
			prop, _ := raw.(map[string]any)
			d, _ := prop["description"].(string)
			key := name + "." + pn
			min := paramMin
			if detailed[key] {
				min = detailedMin
				seen[key] = true
			}
			if n := utf8.RuneCountInString(d); n < min {
				t.Errorf("%s: parameter description is %d chars (%q), want >= %d", key, n, d, min)
			}
		}
	}
	for key := range detailed {
		if !seen[key] {
			t.Errorf("%s: listed as detailed but not registered — update the list", key)
		}
	}
}
