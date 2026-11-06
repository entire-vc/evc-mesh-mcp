package mcp

import (
	"strings"
	"testing"
)

func TestListTasksFilters_StatusCategories(t *testing.T) {
	for _, category := range []string{"backlog", "todo", "triage", "in_progress", "review", "done", "cancelled"} {
		t.Run(category, func(t *testing.T) {
			for _, scope := range []map[string]any{
				{"project_id": "p", "status_category": category},
				{"workspace_id": "w", "search": "query", "status_category": category},
			} {
				if err := validateListTasksArguments(requestWith(scope)); err != nil {
					t.Errorf("valid category rejected: %v", err)
				}
			}
		})
	}
}

func TestListTasksFilters_InvalidStatusCategories(t *testing.T) {
	for _, category := range []any{"", "active", "TRIAGE", " triage ", 7, nil} {
		if err := validateListTasksArguments(requestWith(map[string]any{"project_id": "p", "status_category": category})); err == nil {
			t.Errorf("invalid category accepted: %#v", category)
		} else if !strings.Contains(err.Error(), "triage") {
			t.Errorf("error omits triage from supported categories: %v", err)
		}
	}
}

func TestListTasksFilters_StatusCategorySchema(t *testing.T) {
	tool := NewServer(ServerConfig{}).MCPServer().ListTools()["list_tasks"]
	property := tool.Tool.InputSchema.Properties["status_category"].(map[string]any)
	description, _ := property["description"].(string)
	for _, category := range []string{"backlog", "todo", "triage", "in_progress", "review", "done", "cancelled"} {
		if !strings.Contains(description, category) {
			t.Errorf("schema description omits %s: %s", category, description)
		}
	}
	if _, exists := property["enum"]; exists {
		t.Fatal("status_category must not have a schema enum that can exclude project categories")
	}
}
