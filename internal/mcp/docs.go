package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// Documents: list_docs and get_doc.
//
// The whole point of these two tools is what they do NOT return. A document is
// an order of magnitude larger than a task, and an agent that reads a page to
// answer a question about one paragraph pays for the whole page for the rest of
// its session. So the body is never returned unless it was asked for by name,
// and "asked for by name" has three grades: one section, or the whole thing, or
// neither.
//
// Everything here is a projection over the REST API — no shape is invented, and
// unknown fields are passed through rather than enumerated (see stripKeys).

// docListNoiseKeys are dropped from every document in a list.
//
// `body` should never appear on a list response at all — the API populates it
// only for single-document reads. It is dropped anyway, defensively: a future
// server that starts including it would otherwise silently turn the cheap call
// into the expensive one, and nothing in the tool would notice. This is the
// guarantee "a listing carries no bodies" made structural instead of assumed.
//
// `storage_key` is an internal object-storage address. It is not useful to an
// agent and not something to hand out.
var docListNoiseKeys = []string{"body", "storage_key"}

// headingNoiseKeys are dropped from every heading in an outline.
//
// They are byte offsets into the markdown, and an agent has no use for one: the
// entire design of this feature is that agents address text by heading or by
// quotation and the SERVER computes offsets, precisely because an agent
// computing them gets them wrong (the Cyrillic case that returned 475 for a
// correct answer of 853). Carrying them would be paying context for a number
// nobody may act on. The span they describe survives as size_bytes.
var headingNoiseKeys = []string{"start", "end", "line"}

// stripKeys removes the named keys from a copy of m, leaving everything else —
// including fields this build has never heard of — untouched.
//
// A drop-list rather than an allow-list, and that is the whole point: an
// allow-list silently deletes any field a newer server adds, so the tool would
// have to be released in lockstep with the API to keep working. On 19.08 a
// changed /dependencies response broke get_task for the entire fleet for exactly
// that class of reason. Passing unknown fields through costs nothing and means a
// server ahead of this binary degrades to "extra data" instead of "missing data".
func stripKeys(m map[string]any, keys []string) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = v
	}
	for _, k := range keys {
		delete(out, k)
	}
	return out
}

// asMapSlice coerces a decoded JSON array into []map[string]any, skipping
// anything that is not an object. Tolerant on purpose: a malformed or
// unexpected element is ignored rather than failing the whole read.
func asMapSlice(v any) []map[string]any {
	raw, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// asString returns m[key] as a string, or "" if absent or not a string.
func asString(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

// asInt returns m[key] as an int. JSON numbers decode as float64 through
// map[string]any; both are accepted so the helper does not depend on which
// decoder produced the map.
func asInt(m map[string]any, key string) (int, bool) {
	switch n := m[key].(type) {
	case float64:
		return int(n), true
	case int:
		return n, true
	case json.Number:
		if i, err := strconv.Atoi(n.String()); err == nil {
			return i, true
		}
	}
	return 0, false
}

// documentPaths computes the slug path of every document in a project — the
// "architecture/adr/adr-004" form that get_doc accepts in place of a uuid.
//
// The API returns each document's slug and parent_id but not its path, because a
// path is a property of the tree rather than of the row. Walking parents here
// keeps that one call body-free; resolving a path by asking the server for each
// candidate document would download a markdown page per probe.
//
// A document whose parent is missing from the set (an unreadable parent, or a
// page beyond the last one fetched) is keyed by its own slug alone, so it stays
// addressable instead of vanishing.
func documentPaths(docs []map[string]any) map[string]string {
	slugByID := make(map[string]string, len(docs))
	parentByID := make(map[string]string, len(docs))
	for _, d := range docs {
		id := asString(d, "id")
		if id == "" {
			continue
		}
		slugByID[id] = asString(d, "slug")
		if p, ok := d["parent_id"].(string); ok {
			parentByID[id] = p
		}
	}

	paths := make(map[string]string, len(docs))
	for id := range slugByID {
		segments := []string{}
		// Bounded by the number of documents: a cycle in parent_id, which the
		// schema should prevent but this code must survive, terminates here
		// instead of hanging the agent's session.
		seen := make(map[string]bool, 8)
		for cur := id; cur != "" && !seen[cur] && len(segments) <= len(slugByID); {
			seen[cur] = true
			slug, known := slugByID[cur]
			if !known {
				break
			}
			segments = append([]string{slug}, segments...)
			cur = parentByID[cur]
		}
		paths[id] = strings.Join(segments, "/")
	}
	return paths
}

// fetchProjectDocuments pulls every page of a project's document list.
//
// The tool's job is to be a map of what exists, and a map that stops at the
// first 200 rows is a map with the edges cut off — the caller cannot tell a
// short project from a truncated one. Pages are capped so a runaway server
// cannot spin here; the cap is reported to the caller rather than hidden.
func (s *Server) fetchProjectDocuments(ctx context.Context, projectID string, includeArchived bool) ([]map[string]any, bool, error) {
	const (
		pageSize = 200
		maxPages = 25
	)

	client := s.getRESTClient(ctx)
	all := []map[string]any{}

	for page := 1; page <= maxPages; page++ {
		params := map[string]string{
			"page":      strconv.Itoa(page),
			"page_size": strconv.Itoa(pageSize),
		}
		if includeArchived {
			params["include_archived"] = "true"
		}

		resp, err := client.ListDocuments(ctx, projectID, params)
		if err != nil {
			return nil, false, err
		}

		items := asMapSlice(resp["items"])
		all = append(all, items...)

		hasMore, _ := resp["has_more"].(bool)
		if !hasMore || len(items) == 0 {
			return all, false, nil
		}
	}

	return all, true, nil
}

// ============================================================================
// list_docs
// ============================================================================

func (s *Server) handleListDocs(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}
	includeArchived := mcpsdk.ParseBoolean(request, "include_archived", false)

	docs, truncated, err := s.fetchProjectDocuments(ctx, projectID, includeArchived)
	if err != nil {
		return errResult("failed to list documents: %v", err)
	}

	paths := documentPaths(docs)

	hasChildren := make(map[string]bool, len(docs))
	for _, d := range docs {
		if p, ok := d["parent_id"].(string); ok && p != "" {
			hasChildren[p] = true
		}
	}

	items := make([]map[string]any, 0, len(docs))
	for _, d := range docs {
		item := stripKeys(d, docListNoiseKeys)
		id := asString(d, "id")
		item["path"] = paths[id]
		item["has_children"] = hasChildren[id]
		items = append(items, item)
	}

	out := map[string]any{
		"items": items,
		"count": len(items),
	}
	if truncated {
		// Named, not swallowed: a listing that quietly stopped early reads
		// exactly like a complete one.
		out["truncated"] = true
		out["note"] = "listing stopped at the page cap; some documents are not shown"
	}

	return jsonResult(out)
}

// ============================================================================
// get_doc
// ============================================================================

// resolveDocRef turns the tool's `doc` argument into a document uuid. A uuid is
// returned as-is; anything else is treated as a slug path and looked up in the
// project's listing, which is the body-free way to do it.
func (s *Server) resolveDocRef(ctx context.Context, ref, projectID string) (string, error) {
	if _, err := uuid.Parse(ref); err == nil {
		return ref, nil
	}

	if projectID == "" {
		return "", fmt.Errorf("%q is not a uuid, so it is read as a slug path — pass project_id to resolve it", ref)
	}

	docs, _, err := s.fetchProjectDocuments(ctx, projectID, false)
	if err != nil {
		return "", fmt.Errorf("failed to resolve path: %w", err)
	}

	want := strings.Trim(strings.TrimSpace(ref), "/")
	paths := documentPaths(docs)

	// An exact path match first. A bare slug is accepted too, but only when it
	// is unambiguous: silently picking one of several same-named leaves would
	// hand back the wrong document with no signal that a choice was made.
	var bySlug []string
	for id, p := range paths {
		if p == want {
			return id, nil
		}
		if segments := strings.Split(p, "/"); segments[len(segments)-1] == want {
			bySlug = append(bySlug, id)
		}
	}

	switch len(bySlug) {
	case 1:
		return bySlug[0], nil
	case 0:
		return "", fmt.Errorf("no document at path %q in this project", want)
	default:
		options := make([]string, 0, len(bySlug))
		for _, id := range bySlug {
			options = append(options, paths[id])
		}
		return "", fmt.Errorf("%q matches %d documents (%s) — pass the full path",
			want, len(bySlug), strings.Join(options, ", "))
	}
}

// maxDefaultHeadings bounds the outline returned by a default get_doc.
//
// An outline's cost scales with the NUMBER of headings, not with the length of
// the body, and those come apart badly: a 50KB page of ordinary prose has a few
// dozen headings and costs well under a kilobyte, but the same 50KB chopped into
// 80 short sections costs more than ten. Without a bound, "the cheap read" is
// only cheap on documents that happen to be shaped kindly — which is exactly the
// kind of default that passes its test and then surprises somebody in
// production.
//
// Truncation is reported rather than silent, and it costs the caller nothing:
// every section remains reachable by name through section=, and outline_depth
// surveys a deep document top-down.
const maxDefaultHeadings = 40

// trimOutline drops byte offsets from each heading and replaces the span they
// describe with size_bytes, so an agent can still judge whether a section is
// worth reading without reading it.
//
// depth > 0 keeps only headings at that level or shallower — the way a printed
// table of contents shows chapters before it shows every subsection.
func trimOutline(headings []map[string]any, depth int) []map[string]any {
	out := make([]map[string]any, 0, len(headings))
	for _, h := range headings {
		if depth > 0 {
			if level, ok := asInt(h, "level"); ok && level > depth {
				continue
			}
		}
		trimmed := stripKeys(h, headingNoiseKeys)
		start, okStart := asInt(h, "start")
		end, okEnd := asInt(h, "end")
		if okStart && okEnd && end >= start {
			trimmed["size_bytes"] = end - start
		}
		out = append(out, trimmed)
	}
	return out
}

func (s *Server) handleGetDoc(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	ref := mcpsdk.ParseString(request, "doc", "")
	if ref == "" {
		return errResult("doc is required (a document uuid, or a slug path with project_id)")
	}
	projectID := mcpsdk.ParseString(request, "project_id", "")
	wantBody := mcpsdk.ParseBoolean(request, "body", false)
	section := strings.TrimSpace(mcpsdk.ParseString(request, "section", ""))
	versionOnly := mcpsdk.ParseBoolean(request, "version_only", false)

	docID, err := s.resolveDocRef(ctx, ref, projectID)
	if err != nil {
		return errResult("%v", err)
	}

	client := s.getRESTClient(ctx)

	// One section. Checked before `body` so that asking for both is not silently
	// resolved into the expensive one.
	if section != "" {
		resp, err := client.GetDocumentSection(ctx, docID, section)
		if err != nil {
			return errResult("failed to read section %q: %v", section, err)
		}
		if h, ok := resp["heading"].(map[string]any); ok {
			resp["heading"] = stripKeys(h, headingNoiseKeys)
		}
		return jsonResult(resp)
	}

	// The whole page, because it was asked for by name.
	if wantBody {
		resp, err := client.GetDocument(ctx, docID)
		if err != nil {
			return errResult("failed to read document: %v", err)
		}
		return jsonResult(stripKeys(resp, []string{"storage_key"}))
	}

	outline, err := client.GetDocumentOutline(ctx, docID)
	if err != nil {
		return errResult("failed to read document outline: %v", err)
	}

	// version_only: the cheapest question there is — "has this changed since I
	// read it?" — answered without the outline it was computed alongside.
	if versionOnly {
		out := stripKeys(outline, []string{"outline"})
		return jsonResult(out)
	}

	depth := 0
	if d, err := strconv.Atoi(mcpsdk.ParseString(request, "outline_depth", "0")); err == nil {
		depth = d
	}

	headings := trimOutline(asMapSlice(outline["outline"]), depth)

	out := stripKeys(outline, nil)
	out["body_omitted"] = true
	hint := "body not returned by default. Read one section with section=\"<heading or anchor>\", or the whole page with body=true."

	if len(headings) > maxDefaultHeadings {
		out["outline_total"] = len(headings)
		out["outline_truncated"] = true
		headings = headings[:maxDefaultHeadings]
		hint += fmt.Sprintf(" Outline truncated to the first %d of %d headings —"+
			" narrow it with outline_depth=1|2, or read a section by name with section=.",
			maxDefaultHeadings, out["outline_total"])
	}

	out["outline"] = headings
	out["hint"] = hint

	return jsonResult(out)
}

// ============================================================================
// create_doc
// ============================================================================

func (s *Server) handleCreateDoc(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	projectID := mcpsdk.ParseString(request, "project_id", "")
	if projectID == "" {
		return errResult("project_id is required")
	}
	title := strings.TrimSpace(mcpsdk.ParseString(request, "title", ""))
	if title == "" {
		return errResult("title is required")
	}

	payload := map[string]any{
		"title": title,
		"body":  mcpsdk.ParseString(request, "body", ""),
	}
	if slug := mcpsdk.ParseString(request, "slug", ""); slug != "" {
		payload["slug"] = slug
	}
	if parent := mcpsdk.ParseString(request, "parent_id", ""); parent != "" {
		payload["parent_id"] = parent
	}
	if hasArgument(request, "position") {
		payload["position"] = int(mcpsdk.ParseFloat64(request, "position", 0))
	}

	doc, err := s.getRESTClient(ctx).CreateDocument(ctx, projectID, payload)
	if err != nil {
		return errResult("failed to create document: %v", err)
	}

	// The body is dropped from the reply: the caller wrote it, so echoing it back
	// spends context on text they already have. `version` is kept and is the
	// point of the reply — it is what the next update_doc passes as base_version,
	// so a create followed by an edit needs no read in between.
	return jsonResult(stripKeys(doc, docListNoiseKeys))
}

// ============================================================================
// update_doc
// ============================================================================

// conflictResult renders a 409 from a conditional write.
//
// Returned as an ERROR, because the single most important fact is that nothing
// was written — a caller that reads this as success will believe an edit landed
// that did not. The current version is lifted out of the server's payload rather
// than scraped from its prose, and the retry is spelled out, because a conflict
// an agent cannot act on becomes a loop of blind repeats.
func conflictResult(apiErr *APIError) (*mcpsdk.CallToolResult, error) {
	current := "unknown"
	if apiErr.Body != nil {
		if v, ok := asInt(apiErr.Body, "current_version"); ok {
			current = strconv.Itoa(v)
		}
	}
	return errResult("409 version conflict — NOTHING WAS WRITTEN. "+
		"The document is now at version %s; somebody else wrote to it after you read it. "+
		"Re-read it (get_doc, or get_doc version_only=true), re-apply your change on top of "+
		"the current text, and retry with base_version=%s. "+
		"Do not retry with the same base_version — it will conflict again. "+
		"If you are only adding to the end, use append instead: it needs no base_version and cannot conflict.",
		current, current)
}

func (s *Server) handleUpdateDoc(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	ref := mcpsdk.ParseString(request, "doc", "")
	if ref == "" {
		return errResult("doc is required (a document uuid, or a slug path with project_id)")
	}
	projectID := mcpsdk.ParseString(request, "project_id", "")

	hasAppend := hasArgument(request, "append")
	hasBody := hasArgument(request, "body")
	hasBase := hasArgument(request, "base_version")

	// Replace and append are different intentions, and guessing which one was
	// meant is how the wrong text ends up in a document. The server refuses this
	// too; refusing here saves the round trip and can say it more usefully.
	if hasAppend && hasBody {
		return errResult("send either body (replace the whole document) or append (add to the end), not both")
	}

	payload := map[string]any{}
	if hasBody {
		payload["body"] = mcpsdk.ParseString(request, "body", "")
	}
	if hasAppend {
		payload["append_body"] = mcpsdk.ParseString(request, "append", "")
	}
	if hasArgument(request, "title") {
		payload["title"] = mcpsdk.ParseString(request, "title", "")
	}
	if hasArgument(request, "parent_id") {
		payload["parent_id"] = mcpsdk.ParseString(request, "parent_id", "")
	}
	if hasArgument(request, "position") {
		payload["position"] = int(mcpsdk.ParseFloat64(request, "position", 0))
	}

	if len(payload) == 0 {
		return errResult("nothing to write: pass body, append, title, parent_id or position")
	}

	// The gate this tool exists to hold.
	//
	// The API accepts a PATCH with no base_version and writes unconditionally —
	// deliberately, because the existing editor sends exactly that and predates
	// the version column. That leniency is right for a human typing in a browser
	// and wrong for an agent: it means one forgetful call silently overwrites
	// somebody else's edit, which is the accident this whole feature was built
	// after. So the requirement lives here, where the agent-facing surface is,
	// and the server keeps its compatibility.
	//
	// An append is exempt because it cannot destroy an edit it never read: two
	// appends both land. Requiring a version there would force a caller to read a
	// whole document just to add a line to the end.
	if !hasBase && !hasAppend {
		return errResult("base_version is required: an unconditional write can silently overwrite " +
			"somebody else's edit. Read the document first (get_doc, or get_doc version_only=true for " +
			"just the number) and pass the version you saw. To add to the end without reading, use " +
			"append instead — it needs no base_version.")
	}
	if hasBase {
		// Passed through even alongside append: an append normally retries through
		// a conflict, and sending a version is how a caller says "tell me instead".
		payload["base_version"] = int(mcpsdk.ParseFloat64(request, "base_version", 0))
	}

	docID, err := s.resolveDocRef(ctx, ref, projectID)
	if err != nil {
		return errResult("%v", err)
	}

	doc, err := s.getRESTClient(ctx).UpdateDocument(ctx, docID, payload)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict {
			return conflictResult(apiErr)
		}
		return errResult("failed to update document: %v", err)
	}

	return jsonResult(stripKeys(doc, docListNoiseKeys))
}
