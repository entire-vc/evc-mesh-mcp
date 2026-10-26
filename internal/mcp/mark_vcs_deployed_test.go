package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

func markDeployedServer(t *testing.T, status int) (*Server, *[]map[string]any, *[]string) {
	t.Helper()
	var bodies []map[string]any
	var paths []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		bodies = append(bodies, b)
		paths = append(paths, r.Method+" "+r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			_ = json.NewEncoder(w).Encode(map[string]any{"deployed_sha": b["sha"], "deployed_env": b["env"], "deployed_at": "2026-10-25T10:00:00Z"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"message": "deployed.sha must be a 40-character lowercase hex string"})
	}))
	t.Cleanup(srv.Close)
	return &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}, &bodies, &paths
}

func TestMarkVCSDeployed_PostsShaAndEnvToTheDeploymentEndpoint(t *testing.T) {
	server, bodies, paths := markDeployedServer(t, http.StatusOK)
	task, link := uuid.NewString(), uuid.NewString()
	sha := strings.Repeat("a", 40)

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = map[string]any{"task_id": task, "link_id": link, "sha": " " + sha + " ", "env": "production"}
	res, err := server.handleMarkVCSDeployed(context.Background(), req)
	if err != nil || res.IsError {
		t.Fatalf("mark_vcs_deployed failed: %v %+v", err, res)
	}
	if got, want := (*paths)[0], "POST /api/v1/tasks/"+task+"/vcs-links/"+link+"/deployment"; got != want {
		t.Errorf("request = %q, want %q", got, want)
	}
	if (*bodies)[0]["sha"] != sha || (*bodies)[0]["env"] != "production" {
		t.Errorf("body = %v", (*bodies)[0])
	}
	if !strings.Contains(resultText(t, res), "deployed_at") {
		t.Errorf("the link with deployed_at must be returned: %s", resultText(t, res))
	}
}

func TestMarkVCSDeployed_RequiresEveryArgumentAndSurfacesServerErrors(t *testing.T) {
	server, _, paths := markDeployedServer(t, http.StatusBadRequest)
	full := map[string]any{"task_id": uuid.NewString(), "link_id": uuid.NewString(), "sha": "zz", "env": "production"}
	for _, missing := range []string{"task_id", "link_id", "sha", "env"} {
		args := map[string]any{}
		for k, v := range full {
			if k != missing {
				args[k] = v
			}
		}
		req := mcpsdk.CallToolRequest{}
		req.Params.Arguments = args
		res, _ := server.handleMarkVCSDeployed(context.Background(), req)
		if !res.IsError || !strings.Contains(resultText(t, res), missing+" is required") {
			t.Errorf("missing %s: %s", missing, resultText(t, res))
		}
	}
	if len(*paths) != 0 {
		t.Errorf("incomplete calls must not reach the API: %v", *paths)
	}

	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = full
	res, _ := server.handleMarkVCSDeployed(context.Background(), req)
	if !res.IsError || !strings.Contains(resultText(t, res), "40-character") {
		t.Errorf("the server's validation message must reach the agent: %s", resultText(t, res))
	}
}

func TestMarkVCSDeployed_RegisteredInFullProfileOnlyWithRequiredFields(t *testing.T) {
	full := NewServer(ServerConfig{Profile: ProfileFull}).MCPServer().ListTools()
	entry, ok := full["mark_vcs_deployed"]
	if !ok {
		t.Fatal("mark_vcs_deployed is not registered in the full profile")
	}
	for _, f := range []string{"task_id", "link_id", "sha", "env"} {
		if !slices.Contains(entry.Tool.InputSchema.Required, f) {
			t.Errorf("%s must be required", f)
		}
	}
	if _, ok := NewServer(ServerConfig{Profile: ProfileCore}).MCPServer().ListTools()["mark_vcs_deployed"]; ok {
		t.Error("mark_vcs_deployed must not be in the core profile (it would change the pinned core size)")
	}
}

func TestDependencyConditionEnum_ListsEveryConditionIncludingShipped(t *testing.T) {
	tools := NewServer(ServerConfig{Profile: ProfileFull}).MCPServer().ListTools()
	for _, name := range []string{"add_dependency", "update_dependency"} {
		property, ok := tools[name].Tool.InputSchema.Properties["condition"].(map[string]any)
		if !ok {
			t.Fatalf("%s: no condition property", name)
		}
		var got []string
		switch enum := property["enum"].(type) {
		case []string:
			got = enum
		case []any:
			for _, v := range enum {
				got = append(got, v.(string))
			}
		}
		if !slices.Equal(got, dependencyConditions) {
			t.Errorf("%s: enum = %v, want %v", name, got, dependencyConditions)
		}
		desc, _ := property["description"].(string)
		for _, c := range dependencyConditions {
			if !strings.Contains(desc, c) {
				t.Errorf("%s: description does not explain %s", name, c)
			}
		}
	}
	if !slices.Contains(dependencyConditions, "shipped") {
		t.Error("shipped missing")
	}
}
