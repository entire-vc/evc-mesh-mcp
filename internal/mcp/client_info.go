package mcp

import (
	"context"
	"log"
	"strings"
	"sync"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
	mcpserver "github.com/mark3labs/mcp-go/server"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// initializeTotal counts MCP initialize handshakes by the client that sent
// them (clientInfo.name) and by tool profile. It is what tells us which MCP
// clients and catalogs actually bring connections.
var initializeTotal = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "mesh_mcp_initialize_total",
	Help: "MCP initialize handshakes, by client name (clientInfo.name) and tool profile.",
}, []string{"client", "profile"})

// maxClientLabels bounds the number of distinct client label values: the
// name is chosen by the caller, so an unbounded label would let any client
// grow the metric without limit. Past the cap, new names count as "other".
const maxClientLabels = 64

var (
	clientLabelsMu sync.Mutex
	clientLabels   = map[string]struct{}{}
)

// clientLabel turns a caller-supplied clientInfo.name into a bounded,
// normalised metric label: lower-case, [a-z0-9._-] only, at most 40 chars.
func clientLabel(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	var b strings.Builder
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		case r == ' ' || r == '/':
			b.WriteRune('-')
		}
		if b.Len() >= 40 {
			break
		}
	}
	label := b.String()
	if label == "" {
		return "unknown"
	}
	clientLabelsMu.Lock()
	defer clientLabelsMu.Unlock()
	if _, ok := clientLabels[label]; ok {
		return label
	}
	if len(clientLabels) >= maxClientLabels {
		return "other"
	}
	clientLabels[label] = struct{}{}
	return label
}

// clientInfoHooks logs every initialize handshake with the client's
// self-reported name and version, and counts it in initializeTotal.
func clientInfoHooks(profile string, sessionOf func(context.Context) *AgentSession) *mcpserver.Hooks {
	hooks := &mcpserver.Hooks{}
	hooks.AddAfterInitialize(func(ctx context.Context, _ any, req *mcpsdk.InitializeRequest, _ *mcpsdk.InitializeResult) {
		if req == nil {
			return
		}
		ci := req.Params.ClientInfo
		initializeTotal.WithLabelValues(clientLabel(ci.Name), profile).Inc()
		agent := "-"
		if sess := sessionOf(ctx); sess != nil {
			agent = sess.AgentName
		}
		log.Printf("mcp initialize: client=%q version=%q protocol=%q profile=%s agent=%s",
			ci.Name, ci.Version, req.Params.ProtocolVersion, profile, agent)
	})
	return hooks
}

// unconfiguredMiddleware answers every tool call with setup instructions.
// It lets the server start without credentials so MCP clients and catalogs
// can still initialize and list the tools.
func unconfiguredMiddleware(hint string) mcpserver.ToolHandlerMiddleware {
	return func(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
		return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			return mcpsdk.NewToolResultError(hint), nil
		}
	}
}

// deferredAuthMiddleware authenticates on first use when startup
// authentication failed. Until it succeeds every tool call returns the error,
// so the client sees why the server is not working instead of a missing tool.
func (s *Server) deferredAuthMiddleware(authenticate func(context.Context) (*AgentSession, error)) mcpserver.ToolHandlerMiddleware {
	return func(next mcpserver.ToolHandlerFunc) mcpserver.ToolHandlerFunc {
		return func(ctx context.Context, req mcpsdk.CallToolRequest) (*mcpsdk.CallToolResult, error) {
			if s.deferredSession.Load() == nil {
				s.authMu.Lock()
				if s.deferredSession.Load() == nil {
					sess, err := authenticate(ctx)
					if err != nil {
						s.authMu.Unlock()
						return mcpsdk.NewToolResultError("Could not authenticate with the Mesh API (check MESH_API_URL and MESH_AGENT_KEY): " + err.Error()), nil
					}
					s.deferredSession.Store(sess)
					log.Printf("deferred authentication succeeded: agent %s", sess.AgentName)
				}
				s.authMu.Unlock()
			}
			return next(ctx, req)
		}
	}
}
