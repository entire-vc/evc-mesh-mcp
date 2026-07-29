package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// coreToolCount pins the size of the core profile. The doc comments in
// server.go quote this number; they drifted out of date once already, and a
// stale comment is how a tool ends up registered in the wrong profile.
const coreToolCount = 25

func TestCoreProfile_ExposesAddVCSLink(t *testing.T) {
	server := NewServer(ServerConfig{Profile: ProfileCore})
	tools := server.MCPServer().ListTools()

	if _, ok := tools["add_vcs_link"]; !ok {
		t.Fatal("add_vcs_link is not registered in the core profile — lightweight agents could not link a PR")
	}
	if len(tools) != coreToolCount {
		t.Errorf("core profile registers %d tools, want %d — update the counts in the server.go doc comments",
			len(tools), coreToolCount)
	}
}

func TestParseVCSURL(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want vcsURLFacts
	}{
		{
			name: "github pull request",
			url:  "https://github.com/entire-vc/evc-mesh/pull/426",
			want: vcsURLFacts{Provider: "github", LinkType: "pr", ExternalID: "426", Repository: "entire-vc/evc-mesh"},
		},
		{
			name: "github pull request with trailing path",
			url:  "https://github.com/entire-vc/evc-mesh/pull/426/files",
			want: vcsURLFacts{Provider: "github", LinkType: "pr", ExternalID: "426", Repository: "entire-vc/evc-mesh"},
		},
		{
			name: "github pull request with fragment",
			url:  "https://github.com/entire-vc/evc-mesh/pull/426#issuecomment-1",
			want: vcsURLFacts{Provider: "github", LinkType: "pr", ExternalID: "426", Repository: "entire-vc/evc-mesh"},
		},
		{
			name: "github commit",
			url:  "https://github.com/entire-vc/evc-mesh/commit/2b5cc16aa1",
			want: vcsURLFacts{Provider: "github", LinkType: "commit", ExternalID: "2b5cc16aa1", Repository: "entire-vc/evc-mesh"},
		},
		{
			name: "github branch with slash in the name",
			url:  "https://github.com/entire-vc/evc-mesh/tree/garfield/add-vcs-link",
			want: vcsURLFacts{Provider: "github", LinkType: "branch", ExternalID: "garfield/add-vcs-link", Repository: "entire-vc/evc-mesh"},
		},
		{
			name: "gitlab merge request",
			url:  "https://gitlab.com/group/proj/-/merge_requests/17",
			want: vcsURLFacts{Provider: "gitlab", LinkType: "pr", ExternalID: "17", Repository: "group/proj"},
		},
		{
			name: "host-only url yields nothing",
			url:  "https://github.com/",
			want: vcsURLFacts{Provider: "github"},
		},
		{
			name: "repo root has no resource",
			url:  "https://github.com/entire-vc/evc-mesh",
			want: vcsURLFacts{Provider: "github"},
		},
		{
			name: "non-numeric pr number is not an id",
			url:  "https://github.com/entire-vc/evc-mesh/pull/abc",
			want: vcsURLFacts{Provider: "github", Repository: "entire-vc/evc-mesh"},
		},
		{
			// Neither the host nor the path grammar is recognised, so nothing
			// is claimed — including the repository, which would otherwise be
			// a guess presented as a fact.
			name: "unknown host, unknown path shape",
			url:  "https://example.com/some/random/page",
			want: vcsURLFacts{},
		},
		{
			// Self-hosted GitHub Enterprise: the host says nothing, but the
			// path grammar is still readable.
			name: "unknown host with a recognisable path",
			url:  "https://git.example.internal/entire-vc/evc-mesh/pull/12",
			want: vcsURLFacts{LinkType: "pr", ExternalID: "12", Repository: "entire-vc/evc-mesh"},
		},
		{
			name: "not a url at all",
			url:  "just some text",
			want: vcsURLFacts{},
		},
		{
			name: "empty",
			url:  "",
			want: vcsURLFacts{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseVCSURL(tt.url)
			if got != tt.want {
				t.Errorf("parseVCSURL(%q) = %+v, want %+v", tt.url, got, tt.want)
			}
		})
	}
}

func TestNormalizeVCSLinkType(t *testing.T) {
	tests := map[string]string{
		"pr":           "pr",
		"pull_request": "pr",
		"PR":           "pr",
		"Pull_Request": "pr",
		"  pr  ":       "pr",
		"commit":       "commit",
		"branch":       "branch",
		// Left alone: the API decides what is valid, not this client.
		"tag": "tag",
	}
	for given, want := range tests {
		if got := normalizeVCSLinkType(given); got != want {
			t.Errorf("normalizeVCSLinkType(%q) = %q, want %q", given, got, want)
		}
	}
}

// addVCSLinkHarness stands up a fake API that records the POST body.
func addVCSLinkHarness(t *testing.T, taskID string) (*Server, *map[string]any, func()) {
	t.Helper()

	captured := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/tasks/"+taskID+"/vcs-links" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          uuid.New().String(),
			"task_id":     taskID,
			"link_type":   captured["link_type"],
			"external_id": captured["external_id"],
		})
	}))

	server := &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{AgentID: uuid.New(), WorkspaceID: uuid.New()},
	}
	return server, &captured, srv.Close
}

func callAddVCSLink(t *testing.T, server *Server, args map[string]any) *mcpsdk.CallToolResult {
	t.Helper()
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	result, err := server.handleAddVCSLink(context.Background(), req)
	if err != nil {
		t.Fatalf("handleAddVCSLink returned error: %v", err)
	}
	return result
}

// The tool exists because restating provider/link_type/external_id was enough
// friction that nobody linked anything. A bare task_id + PR URL must be enough.
func TestHandleAddVCSLink_InfersEverythingFromPRURL(t *testing.T) {
	taskID := uuid.New().String()
	server, captured, closeSrv := addVCSLinkHarness(t, taskID)
	defer closeSrv()

	prURL := "https://github.com/entire-vc/evc-mesh/pull/426"
	result := callAddVCSLink(t, server, map[string]any{"task_id": taskID, "url": prURL})
	if result.IsError {
		t.Fatalf("tool returned an error result: %v", result.Content)
	}

	body := *captured
	if body["provider"] != "github" {
		t.Errorf("provider = %v, want github", body["provider"])
	}
	if body["link_type"] != "pr" {
		t.Errorf("link_type = %v, want pr", body["link_type"])
	}
	if body["external_id"] != "426" {
		t.Errorf("external_id = %v, want 426", body["external_id"])
	}
	if body["url"] != prURL {
		t.Errorf("url = %v, want %s", body["url"], prURL)
	}
	if _, ok := body["title"]; ok {
		t.Errorf("title was sent without being asked for: %v", body["title"])
	}
}

// The alias is normalised client-side, so the tool works against an API that
// has not yet taken the server-side alias change.
func TestHandleAddVCSLink_NormalisesPullRequestAlias(t *testing.T) {
	for _, given := range []string{"pull_request", "Pull_Request", "PR"} {
		t.Run(given, func(t *testing.T) {
			taskID := uuid.New().String()
			server, captured, closeSrv := addVCSLinkHarness(t, taskID)
			defer closeSrv()

			result := callAddVCSLink(t, server, map[string]any{
				"task_id":   taskID,
				"url":       "https://github.com/entire-vc/evc-mesh/pull/426",
				"link_type": given,
			})
			if result.IsError {
				t.Fatalf("tool returned an error result: %v", result.Content)
			}
			if got := (*captured)["link_type"]; got != "pr" {
				t.Errorf("link_type sent to the API = %v, want pr", got)
			}
		})
	}
}

func TestHandleAddVCSLink_ExplicitArgumentsWinOverInference(t *testing.T) {
	taskID := uuid.New().String()
	server, captured, closeSrv := addVCSLinkHarness(t, taskID)
	defer closeSrv()

	result := callAddVCSLink(t, server, map[string]any{
		"task_id":     taskID,
		"url":         "https://github.com/entire-vc/evc-mesh/pull/426",
		"provider":    "gitlab",
		"link_type":   "commit",
		"external_id": "deadbeef",
		"title":       "Add the tool",
	})
	if result.IsError {
		t.Fatalf("tool returned an error result: %v", result.Content)
	}

	body := *captured
	for field, want := range map[string]string{
		"provider":    "gitlab",
		"link_type":   "commit",
		"external_id": "deadbeef",
		"title":       "Add the tool",
	} {
		if body[field] != want {
			t.Errorf("%s = %v, want %s", field, body[field], want)
		}
	}
}

// A URL we cannot read must fail loudly with the fix in the message, not
// silently post a link with a blank external_id.
func TestHandleAddVCSLink_UnparseableURLAsksForExternalID(t *testing.T) {
	taskID := uuid.New().String()
	server, captured, closeSrv := addVCSLinkHarness(t, taskID)
	defer closeSrv()

	result := callAddVCSLink(t, server, map[string]any{
		"task_id": taskID,
		"url":     "https://git.example.internal/some/thing",
	})
	if !result.IsError {
		t.Fatal("expected an error result for a URL with no inferable external_id")
	}
	if len(*captured) != 0 {
		t.Errorf("a request was sent despite the missing external_id: %v", *captured)
	}
	text, ok := result.Content[0].(mcpsdk.TextContent)
	if !ok {
		t.Fatalf("content[0] is not TextContent, got %T", result.Content[0])
	}
	for _, want := range []string{"external_id", "PR number", "commit SHA", "branch name"} {
		if !strings.Contains(text.Text, want) {
			t.Errorf("error message %q does not mention %q", text.Text, want)
		}
	}
}

// A self-hosted URL still works when the caller supplies what the URL cannot
// tell us.
func TestHandleAddVCSLink_SelfHostedWithExplicitExternalID(t *testing.T) {
	taskID := uuid.New().String()
	server, captured, closeSrv := addVCSLinkHarness(t, taskID)
	defer closeSrv()

	result := callAddVCSLink(t, server, map[string]any{
		"task_id":     taskID,
		"url":         "https://git.example.internal/some/thing",
		"external_id": "77",
	})
	if result.IsError {
		t.Fatalf("tool returned an error result: %v", result.Content)
	}

	body := *captured
	if body["provider"] != "github" {
		t.Errorf("provider = %v, want the github default", body["provider"])
	}
	if body["link_type"] != "pr" {
		t.Errorf("link_type = %v, want the pr default", body["link_type"])
	}
	if body["external_id"] != "77" {
		t.Errorf("external_id = %v, want 77", body["external_id"])
	}
}

func TestHandleAddVCSLink_RequiredArguments(t *testing.T) {
	taskID := uuid.New().String()
	server, _, closeSrv := addVCSLinkHarness(t, taskID)
	defer closeSrv()

	if r := callAddVCSLink(t, server, map[string]any{"url": "https://github.com/o/r/pull/1"}); !r.IsError {
		t.Error("expected an error result when task_id is missing")
	}
	if r := callAddVCSLink(t, server, map[string]any{"task_id": taskID}); !r.IsError {
		t.Error("expected an error result when url is missing")
	}
}
