package mcp

import (
	"context"
	"errors"
	"fmt"
	"strings"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// Document comments: comment_doc and list_doc_comments.
//
// ## The one thing this file is for
//
// An agent says WHAT it is commenting on by quoting it, never by saying where the
// text sits. There is no offset parameter on either tool, and that absence is the
// feature: measured on 2026-08-19, a Cyrillic quote that really begins at byte 853
// is reported at 475 by a naive character index, because Go, Postgres and this API
// count bytes while a character count counts characters and Russian is two bytes a
// letter. An anchor built that way does not fail — it points confidently at
// somebody else's sentence, and the comment reads as normal work forever after.
// A parameter that does not exist cannot be passed wrongly.
//
// So comment_doc resolves the quote through the server (POST resolve-anchor,
// pkg/mdoc.ResolveQuote — the same rules the browser editor uses, so an agent's
// comment and a human's on the same sentence land in the same place) and hands the
// result straight back to the create call.
//
// ## What is deliberately absent
//
// No resolve, no unresolve, no delete. Marking a thread resolved is a claim that
// the people in it agreed, and an agent resolving conversations at scale does
// damage that afterwards cannot be told apart from ordinary activity — in a
// document's history it all looks the same. Absent from the surface rather than
// refused at runtime: a tool that is not registered cannot be called by a
// confused caller, a retry loop, or a future prompt.

// docCommentNoiseKeys are dropped from every comment in a listing.
//
// Only document_id, and only because the envelope already names the document
// once: repeating the same uuid on every row is context spent to say nothing.
// A drop-list rather than an allow-list, for the reason set out on stripKeys —
// fields this build has never heard of pass through instead of vanishing.
var docCommentNoiseKeys = []string{"document_id"}

// maxDocCommentPages bounds the walk in list_doc_comments.
//
// The listing is paginated and a thread can straddle a page boundary, so the tool
// has to fetch every page before it can build a correct tree — half a tree is a
// reply orphaned from a parent that is merely on the next page. The bound exists
// so a pathological document cannot turn one tool call into an unbounded walk,
// and truncation is REPORTED rather than silent: a caller told "here are the
// comments" while some were dropped has no way to notice.
const maxDocCommentPages = 20

// docCommentPageSize is the server's maximum page size (pagination.MaxPageSize),
// asked for explicitly so the walk is 4 requests on an 800-comment page rather
// than 16.
const docCommentPageSize = 200

// splitQuoteContext turns the passage a caller saw into the prefix and suffix the
// resolver scores with.
//
// The tool takes one quote_context rather than a prefix and a suffix because that
// is what a caller actually has: it read a paragraph, and asking it to chop that
// paragraph into two correctly-oriented halves is one more place to get an
// off-by-one wrong — the class of mistake this whole design removes.
//
// A context that does not contain the quote is refused rather than ignored. Used
// as-is it would push the resolver toward the wrong occurrence, and dropped
// silently it would leave the caller believing they had disambiguated when they
// had not; either way the answer is a confidently-placed comment on the wrong
// sentence. A context containing the quote more than once is refused for the same
// reason: it cannot say which one is meant.
func splitQuoteContext(quoteContext, quote string) (prefix, suffix string, err error) {
	if quoteContext == "" {
		return "", "", nil
	}

	count := strings.Count(quoteContext, quote)
	switch count {
	case 0:
		return "", "", fmt.Errorf("quote_context does not contain the quote — it must be a longer " +
			"passage of the document with the quote inside it, copied exactly. Send the sentence " +
			"before and after the quote along with the quote itself")
	case 1:
	default:
		return "", "", fmt.Errorf("quote_context contains the quote %d times, so it cannot say which "+
			"occurrence is meant — send a shorter passage that contains it once", count)
	}

	at := strings.Index(quoteContext, quote)
	return quoteContext[:at], quoteContext[at+len(quote):], nil
}

// anchorErrResult renders a failed quote resolution.
//
// The ambiguous case carries the match count out of the server's JSON body rather
// than out of its prose, because that number is the only thing that tells the
// caller what to do next: add context, or quote something else entirely.
func anchorErrResult(err error) (*mcpsdk.CallToolResult, error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) && apiErr.Body != nil {
		if code, _ := apiErr.Body["code"].(string); code == "ambiguous_quote" {
			matches := "several"
			if n, ok := asInt(apiErr.Body, "matches"); ok {
				matches = fmt.Sprintf("%d", n)
			}
			return errResult("ambiguous quote — NOTHING WAS POSTED. That text occurs %s times in this "+
				"document, so there is no way to tell which one you mean. Pass quote_context: a longer "+
				"passage from the document that contains the quote exactly once, and the server will "+
				"place it. Do not retry the same quote unchanged — it will be ambiguous again.", matches)
		}
	}
	return errResult("could not place the quote — NOTHING WAS POSTED. %v", err)
}

// handleCommentDoc implements comment_doc.
func (s *Server) handleCommentDoc(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	ref := mcpsdk.ParseString(request, "doc", "")
	if ref == "" {
		return errResult("doc is required (a document uuid, or a slug path with project_id)")
	}
	body := strings.TrimSpace(mcpsdk.ParseString(request, "body", ""))
	if body == "" {
		return errResult("body is required — the text of the comment")
	}

	projectID := mcpsdk.ParseString(request, "project_id", "")
	quote := strings.TrimSpace(mcpsdk.ParseString(request, "quote", ""))
	quoteContext := mcpsdk.ParseString(request, "quote_context", "")
	replyTo := strings.TrimSpace(mcpsdk.ParseString(request, "reply_to", ""))

	// Caught here rather than at the server so the caller is told which of the two
	// to drop. A reply inherits the thread's anchor by construction; letting it
	// carry a second one would be two claims about what one thread is about, and
	// nothing keeps them pointing at the same words once the page is edited.
	if replyTo != "" && quote != "" {
		return errResult("a reply inherits the anchor of the thread it answers, so it cannot carry a " +
			"quote of its own. Drop quote to answer in the thread, or drop reply_to to start a new " +
			"thread on the quoted text.")
	}
	if quote == "" && quoteContext != "" {
		return errResult("quote_context only means anything alongside quote — it is the surrounding " +
			"passage used to tell several occurrences of the quote apart.")
	}

	docID, err := s.resolveDocRef(ctx, ref, projectID)
	if err != nil {
		return errResult("%v", err)
	}

	payload := map[string]any{"body": body}
	if replyTo != "" {
		payload["parent_comment_id"] = replyTo
	}

	if quote != "" {
		prefix, suffix, splitErr := splitQuoteContext(quoteContext, quote)
		if splitErr != nil {
			return errResult("%v", splitErr)
		}

		// The server locates the quote and computes the offsets; they are passed
		// straight back to it on the next call. This is the only place in the tool
		// where an offset exists at all, and it was never in the caller's hands.
		anchor, resolveErr := s.getRESTClient(ctx).ResolveDocumentAnchor(ctx, docID, map[string]any{
			"quote":  quote,
			"prefix": prefix,
			"suffix": suffix,
		})
		if resolveErr != nil {
			return anchorErrResult(resolveErr)
		}
		payload["anchor"] = anchor
	}

	comment, err := s.getRESTClient(ctx).CreateDocumentComment(ctx, docID, payload)
	if err != nil {
		return errResult("failed to post the comment: %v", err)
	}

	return jsonResult(stripKeys(comment, docCommentNoiseKeys))
}

// handleListDocComments implements list_doc_comments.
func (s *Server) handleListDocComments(ctx context.Context, request mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
	ref := mcpsdk.ParseString(request, "doc", "")
	if ref == "" {
		return errResult("doc is required (a document uuid, or a slug path with project_id)")
	}
	projectID := mcpsdk.ParseString(request, "project_id", "")
	includeResolved := mcpsdk.ParseBoolean(request, "include_resolved", false)

	docID, err := s.resolveDocRef(ctx, ref, projectID)
	if err != nil {
		return errResult("%v", err)
	}

	comments, truncated, err := s.fetchDocComments(ctx, docID, includeResolved)
	if err != nil {
		return errResult("failed to read comments: %v", err)
	}

	out := map[string]any{
		"document_id": docID,
		"threads":     threadDocComments(comments),
	}
	if !includeResolved {
		// Named, because "no comments" and "no UNRESOLVED comments" are different
		// facts and the caller cannot tell them apart from an empty list.
		out["note"] = "resolved threads are hidden; pass include_resolved=true to see them"
	}
	if truncated {
		// The count of what was actually read, not the count that was asked for:
		// the server may serve a smaller page than requested, and a figure derived
		// from the request would name a number of comments nobody ever saw.
		out["truncated"] = fmt.Sprintf("this document has more comments than one call reads; the "+
			"oldest %d are here and the rest were not read", len(comments))
	}
	return jsonResult(out)
}

// fetchDocComments walks every page of a document's comments.
//
// All of them, not the first page: threads are assembled here and the listing is
// ordered by creation time, so a reply and its parent routinely land on different
// pages. Building a tree from one page would silently reparent them.
func (s *Server) fetchDocComments(ctx context.Context, docID string, includeResolved bool) ([]map[string]any, bool, error) {
	client := s.getRESTClient(ctx)
	var all []map[string]any

	for page := 1; page <= maxDocCommentPages; page++ {
		params := map[string]string{
			"page":      fmt.Sprintf("%d", page),
			"page_size": fmt.Sprintf("%d", docCommentPageSize),
		}
		if includeResolved {
			params["include_resolved"] = "true"
		}

		result, err := client.ListDocumentComments(ctx, docID, params)
		if err != nil {
			return nil, false, err
		}

		items := asMapSlice(result["items"])
		all = append(all, items...)

		hasMore, _ := result["has_more"].(bool)
		if !hasMore || len(items) == 0 {
			return all, false, nil
		}
	}
	return all, true, nil
}

// threadDocComments nests each reply under the comment it answers.
//
// The wire order is flat and chronological because that is what the database
// returns; a caller wants conversations. Threading is one level deep server-side,
// so this is one pass and not a recursive walk.
//
// A reply whose parent is not in the set is kept at the top level rather than
// dropped. That happens when the parent is resolved and resolved threads are
// hidden — losing the reply would lose the comment, and a comment that exists but
// is not shown is the failure mode this whole tool is meant to avoid. Nothing is
// dropped, and no reply is shown as if it were a thread of its own.
func threadDocComments(comments []map[string]any) []map[string]any {
	nodes := make(map[string]map[string]any, len(comments))
	threads := make([]map[string]any, 0, len(comments))

	// Two passes: a reply may be read before its parent only when the clock says
	// so, which it does not here, but the listing's order is the server's business
	// and not something worth depending on.
	for _, c := range comments {
		node := stripKeys(c, docCommentNoiseKeys)
		if id := asString(c, "id"); id != "" {
			nodes[id] = node
		}
	}

	for _, c := range comments {
		id := asString(c, "id")
		node := nodes[id]
		if node == nil {
			node = stripKeys(c, docCommentNoiseKeys)
		}

		parentID := asString(c, "parent_comment_id")
		parent, known := nodes[parentID]
		if parentID == "" || !known {
			if parentID != "" && !known {
				node["orphaned_reply"] = "the comment this answers is not in this listing — it is " +
					"probably a resolved thread; pass include_resolved=true to see it"
			}
			threads = append(threads, node)
			continue
		}

		replies, _ := parent["replies"].([]map[string]any)
		parent["replies"] = append(replies, node)
	}

	return threads
}
