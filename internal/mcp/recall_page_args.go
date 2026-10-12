package mcp

import (
	"encoding/json"
	"fmt"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

// Validate sizing inputs before changing the omitted compact default. A null,
// misspelled option or invalid profile must not silently become an omitted size.
func recallPageArgs(request mcpsdk.CallToolRequest) (limit int, full bool, profile RecallProfile, err error) {
	args := request.GetArguments()
	for key := range args {
		switch key {
		case "query", "scope", "project_id", "tags", "tags_any", "limit", "offset", "created_by", "since", "until", "relevance_min", "min_importance", "apply_recency_decay", "order_by", "include_expired", "include_archived", "full", "recall_profile":
		default:
			return 0, false, "", fmt.Errorf("unknown recall parameter %q", key)
		}
	}
	if value, present := args["full"]; present {
		var ok bool
		full, ok = value.(bool)
		if !ok {
			return 0, false, "", fmt.Errorf("full must be a boolean")
		}
	}
	if value, present := args["recall_profile"]; present {
		text, ok := value.(string)
		profile = RecallProfile(text)
		if !ok || (profile != ProfileDefault && profile != ProfileFactual && profile != ProfileMultiSession) {
			return 0, false, "", fmt.Errorf("recall_profile must be default, factual or multi-session")
		}
	}
	limit = 5
	if full || profile != "" {
		limit = 10
	}
	if value, present := args["limit"]; present {
		encoded, marshalErr := json.Marshal(value)
		var number float64
		if marshalErr != nil || value == nil || json.Unmarshal(encoded, &number) != nil || number < 1 || number > 50 || number != float64(int(number)) {
			return 0, false, "", fmt.Errorf("limit must be an integer between 1 and 50")
		}
		limit = int(number)
	}
	return limit, full, profile, nil
}
