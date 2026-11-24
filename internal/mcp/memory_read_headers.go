package mcp

// Attribution of memory reads. mesh-api records every read of memory and task
// context (memory_read_events) with who asked: the caller kind, the lane
// session and the card the session works on. This client marks its reads:
//
//	X-Mesh-Caller: agent_tool                         always
//	X-Mesh-Task-Id: $MESH_RUNNER_TASK                 else task_id from $FIDDLER_STATE_FILE
//	X-Mesh-Session-Id: $MESH_RUNNER_CHECKOUT_SESSION
//
// An empty value sends no header. The Runner exports both variables into the
// lane process and this stdio server inherits them. Only the reads listed in
// the contract carry the headers (recall, recall-final, recall_graph,
// get_memory, get_project_knowledge, get_project_updates, list_comments,
// get_task, get_task_context); writes never do, and the checkout fence headers
// of writes (checkout_fence.go) are a separate contract left untouched.
//
// One tool, several requests: get_task(include_comments=true) reads the task
// and then its comments. mesh-api's daily tool counter would count that as
// get_task plus list_comments, so the two requests name the tool they serve:
//
//	X-Mesh-Tool: get_task+comments    the primary GET of the task (not counted)
//	X-Mesh-Tool: get_task             the GET of its comments (counts the call)
//
// Every other request, a plain list_comments included, carries no X-Mesh-Tool.
//
// An API that does not know the headers ignores them.

import (
	"context"
	"net/http"
	"os"
	"strings"
)

const (
	memoryReadCallerHeader  = "X-Mesh-Caller"
	memoryReadTaskHeader    = "X-Mesh-Task-Id"
	memoryReadSessionHeader = "X-Mesh-Session-Id"

	memoryReadCallerAgentTool = "agent_tool"

	meshToolHeader = "X-Mesh-Tool"
	// meshToolGetTaskWithComments marks the task GET of
	// get_task(include_comments=true); meshToolGetTask marks its comments GET.
	meshToolGetTaskWithComments = "get_task+comments"
	meshToolGetTask             = "get_task"

	// The session variable, runnerCheckoutSessionEnv, is declared in idempotency.go.
	runnerTaskEnv = "MESH_RUNNER_TASK"
)

type memoryReadKey struct{}

type meshToolKey struct{}

// withMeshTool names the MCP tool the request made with ctx serves, so doKeyed
// sends it as X-Mesh-Tool.
func withMeshTool(ctx context.Context, tool string) context.Context {
	return context.WithValue(ctx, meshToolKey{}, tool)
}

// applyMeshToolHeader sets X-Mesh-Tool when the request was marked with
// withMeshTool; an unmarked request carries none.
func applyMeshToolHeader(ctx context.Context, req *http.Request) {
	if tool, _ := ctx.Value(meshToolKey{}).(string); tool != "" {
		req.Header.Set(meshToolHeader, tool)
	}
}

// asMemoryRead marks the request made with ctx as a memory read, so doKeyed
// adds the attribution headers.
func asMemoryRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, memoryReadKey{}, true)
}

func isMemoryRead(ctx context.Context) bool {
	v, _ := ctx.Value(memoryReadKey{}).(bool)
	return v
}

// applyMemoryReadHeaders sets the attribution headers on a marked read. The
// environment is read on every request: nothing here is cached.
func applyMemoryReadHeaders(ctx context.Context, req *http.Request) {
	if !isMemoryRead(ctx) {
		return
	}
	req.Header.Set(memoryReadCallerHeader, memoryReadCallerAgentTool)
	taskID := strings.TrimSpace(os.Getenv(runnerTaskEnv))
	if taskID == "" {
		fiddlerTask, _ := readFiddlerContext()
		taskID = strings.TrimSpace(fiddlerTask)
	}
	if taskID != "" {
		req.Header.Set(memoryReadTaskHeader, taskID)
	}
	if session := strings.TrimSpace(os.Getenv(runnerCheckoutSessionEnv)); session != "" {
		req.Header.Set(memoryReadSessionHeader, session)
	}
}
