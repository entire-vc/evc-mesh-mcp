package mcp

// artifactDownloadHowTo is the ONE description of how an agent downloads an artifact.
// It is embedded in the descriptions of list_artifacts, get_artifact and
// upload_artifact (which the model reads on every use) and copied verbatim into the
// README, where a test fails if the two drift.
//
// Measured with three separate agent keys on 20.09.2026: step 1 accepts only the
// X-Agent-Key header (X-API-Key and Authorization: Bearer both answer 401); step 2 is
// a presigned URL, so ANY extra header — Authorization above all — breaks the
// signature and answers 400. Neither trap was written anywhere an agent looks.
const artifactDownloadHowTo = "Downloading an artifact is two GETs. " +
	"Step 1: GET <base>/api/v1/artifacts/<id>/download with header X-Agent-Key: <your agent key> -> 200 JSON {\"url\": \"<presigned URL>\"}. " +
	"Step 2: GET that url with NO headers -> 200, the file bytes. " +
	"Pitfalls: on step 1 only X-Agent-Key is accepted (X-API-Key and Authorization: Bearer give 401); " +
	"on step 2 any extra header, Authorization in particular, breaks the presigned signature (400). " +
	"The artifact's download_path is step 1's path. Never fetch browser_only_url with an agent key: it is a human page and answers 401 by design."

// browserOnlyNote explains, in the output itself, what the URL beside it is for.
const browserOnlyNote = "Human-only page (docs.entire.vc, needs a Team Relay browser session). " +
	"It answers 401 for an agent key by design, so do not fetch it. To get the bytes use download_path (two GETs, see the tool description)."

// shapeArtifact makes an artifact's two URLs distinguishable for an agent. The
// backend hands out download_path (the API, by agent key) and, inside metadata,
// tr_public_url (a docs.entire.vc page for a human's browser). Side by side and
// both link-shaped, the second one read as a download link and 401ed for every agent
// that tried it. It moves out of metadata into browser_only_url with a note saying
// what it is NOT. Only the agent-facing MCP output is reshaped; the REST API and the
// web UI, which do read metadata.tr_public_url, are untouched.
func shapeArtifact(m map[string]any) map[string]any {
	if m == nil {
		return m
	}
	meta, ok := m["metadata"].(map[string]any)
	if !ok {
		return m
	}
	u, ok := meta["tr_public_url"].(string)
	if !ok || u == "" {
		return m
	}
	delete(meta, "tr_public_url")
	m["browser_only_url"] = map[string]any{"url": u, "note": browserOnlyNote}
	return m
}

// shapeArtifactList applies shapeArtifact to the two shapes artifacts arrive in: a
// bare list (get_task include_artifacts, get_task_context) and the paginated
// envelope {items: [...]} (list_artifacts). Anything else passes through.
func shapeArtifactList(v any) any {
	switch t := v.(type) {
	case []any:
		for _, it := range t {
			if m, ok := it.(map[string]any); ok {
				shapeArtifact(m)
			}
		}
	case map[string]any:
		if items, ok := t["items"]; ok {
			shapeArtifactList(items)
		}
	}
	return v
}
