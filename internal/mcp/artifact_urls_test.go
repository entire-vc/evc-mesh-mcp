package mcp

import (
	"os"
	"strings"
	"testing"
)

// The artifact download path is two GETs and both halves have a trap; the old
// descriptions said nothing, and get_artifact(include_content=true) returned a key
// named download_url that is NOT the bytes' URL. Measured with three separate agent
// keys on 20.09.2026: step 1 needs X-Agent-Key (X-API-Key and Authorization: Bearer
// both 401); step 2 must carry no Authorization header (it breaks the presigned
// signature, 400).

func artifactWithTR() map[string]any {
	return map[string]any{
		"id":            "a1",
		"download_path": "/api/v1/artifacts/a1/download",
		"metadata": map[string]any{
			"tr_public_url": "https://docs.entire.vc/spark/x/file.txt",
			"note":          "kept",
		},
	}
}

func TestShapeArtifact_MovesTheBrowserOnlyURLOutOfMetadata(t *testing.T) {
	got := shapeArtifact(artifactWithTR())

	meta, _ := got["metadata"].(map[string]any)
	if _, still := meta["tr_public_url"]; still {
		t.Fatalf("tr_public_url must not stay in metadata, where it reads as a download link: %v", meta)
	}
	if meta["note"] != "kept" {
		t.Errorf("other metadata keys must survive: %v", meta)
	}

	b, ok := got["browser_only_url"].(map[string]any)
	if !ok {
		t.Fatalf("expected a browser_only_url block, got %v", got)
	}
	if b["url"] != "https://docs.entire.vc/spark/x/file.txt" {
		t.Errorf("the URL itself must still be reachable for a human: %v", b)
	}
	note, _ := b["note"].(string)
	for _, must := range []string{"401", "download_path"} {
		if !strings.Contains(note, must) {
			t.Errorf("the note must say what this URL is NOT and what to use instead (%q missing): %s", must, note)
		}
	}
	if got["download_path"] != "/api/v1/artifacts/a1/download" {
		t.Errorf("download_path is the API path and must be untouched: %v", got["download_path"])
	}
}

func TestShapeArtifact_WithoutABrowserURLIsUntouched(t *testing.T) {
	in := map[string]any{"id": "a2", "metadata": map[string]any{"k": "v"}}
	got := shapeArtifact(in)
	if _, has := got["browser_only_url"]; has {
		t.Errorf("no tr_public_url, so no browser_only_url block: %v", got)
	}
	if meta, _ := got["metadata"].(map[string]any); meta["k"] != "v" {
		t.Errorf("metadata changed: %v", got)
	}
}

func TestShapeArtifactList_CoversBothListShapes(t *testing.T) {
	// A bare slice (get_task include_artifacts, get_task_context) ...
	items := shapeArtifactList([]any{artifactWithTR()}).([]any)
	if _, has := items[0].(map[string]any)["browser_only_url"]; !has {
		t.Errorf("bare list not shaped: %v", items)
	}
	// ... and the paginated envelope (list_artifacts).
	env := shapeArtifactList(map[string]any{"items": []any{artifactWithTR()}, "total_count": float64(1)}).(map[string]any)
	if _, has := env["items"].([]any)[0].(map[string]any)["browser_only_url"]; !has {
		t.Errorf("envelope items not shaped: %v", env)
	}
	if env["total_count"] != float64(1) {
		t.Errorf("envelope fields must survive: %v", env)
	}
}

func TestDownloadHowTo_IsInEveryToolThatHandsOutAnArtifact(t *testing.T) {
	server := NewServer(ServerConfig{})
	tools := server.MCPServer().ListTools()
	for _, name := range []string{"list_artifacts", "get_artifact", "upload_artifact"} {
		reg, ok := tools[name]
		if !ok {
			t.Fatalf("%s not registered", name)
		}
		d := reg.Tool.Description
		for _, must := range []string{"X-Agent-Key", "NO headers", "Authorization", "401", "400"} {
			if !strings.Contains(d, must) {
				t.Errorf("%s description must carry the download how-to; %q missing\n%s", name, must, d)
			}
		}
	}
}

func TestDownloadHowTo_ReadmeCarriesTheSameText(t *testing.T) {
	// One text, not two: the README is a copy, so a drifting copy must fail here.
	raw, err := os.ReadFile("../../README.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), artifactDownloadHowTo) {
		t.Errorf("README.md must contain the download how-to verbatim:\n%s", artifactDownloadHowTo)
	}
}

func TestGetArtifact_IncludeContentDescriptionDoesNotPromiseContent(t *testing.T) {
	server := NewServer(ServerConfig{})
	reg := server.MCPServer().ListTools()["get_artifact"]
	desc := toolParamDesc(t, "get_artifact", "include_content")
	if strings.Contains(strings.ToLower(desc), "text files under 1mb") {
		t.Errorf("include_content never returned content, only a URL; the description must not promise it: %s", desc)
	}
	if !strings.Contains(desc, "download_api_url") {
		t.Errorf("include_content must name the field it adds (download_api_url): %s", desc)
	}
	_ = reg
}
