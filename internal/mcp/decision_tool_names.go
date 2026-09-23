package mcp

import (
	"os"
	"strings"
)

const (
	// toolRecordOwnerDecision is the published name of the canonical-decision writer.
	toolRecordOwnerDecision = "record_owner_decision"
	// toolLegacyDecisionAlias is the name this tool had before it was published.
	// Registered only when legacyToolAliasesEnabled reports true.
	toolLegacyDecisionAlias = "pavel_decision"

	// envLegacyToolAliases opts a deployment into registering legacy tool names
	// next to their current ones. Off by default, so tools/list of the published
	// binary carries only current names.
	envLegacyToolAliases = "MESH_MCP_LEGACY_TOOL_ALIASES"
	// envDeciderUsername names the human recorded as decided_by when
	// record_owner_decision links a decision to a gated task. Empty means
	// "the workspace owner".
	envDeciderUsername = "MESH_MCP_DECIDER_USERNAME"
)

func legacyToolAliasesEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(envLegacyToolAliases))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
