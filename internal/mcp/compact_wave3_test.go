package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestWave3LosslessTaskAndComments(t *testing.T) {
	id := uuid.New().String()
	desc, body := strings.Repeat("я", 5000), strings.Repeat("д", 2000)
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{wave2Comment("c1", body, true)}, "total_count": 1, "has_more": false})
		} else {
			_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "description": desc})
		}
	})
	out := callGetTaskCompact(t, s, map[string]any{"task_id": id, "include_comments": true})
	task := out["task"].(map[string]any)
	if task["description"] != desc {
		t.Error("compact description lost text")
	}
	c := out["comments"].([]any)[0].(map[string]any)
	if c["body"] != body {
		t.Error("inline comment lost text")
	}
	page := callListComments(t, s, map[string]any{"task_id": id})
	if page["items"].([]any)[0].(map[string]any)["body"] != body {
		t.Error("list comment lost text")
	}
	for _, key := range []string{"description_truncated", "description_chars", "description_hint"} {
		if _, ok := task[key]; ok {
			t.Errorf("obsolete marker %s", key)
		}
	}
	if _, ok := c["body_truncated"]; ok {
		t.Error("obsolete body marker")
	}
}

func TestWave3TaskFieldsStatusDeltaAndFull(t *testing.T) {
	id, statusID := uuid.New().String(), uuid.New().String()
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/statuses") {
			_ = json.NewEncoder(w).Encode([]any{map[string]any{"id": statusID, "slug": "in_progress"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": id, "title": "t", "description": "context", "status_id": statusID, "assignee_name": "A", "updated_at": "2026-10-05T10:00:00Z",
			"assignee_id": "a", "created_by": "b", "project_id": "p", "position": 2, "vcs_link_count": 5, "is_shipped": true, "human_gate": true, "human_gate_class": "hard", "custom_fields": map[string]any{}, "dod_checks": map[string]any{}, "start_after": "2026-10-06T00:00:00Z", "parent_task_id": "p",
		})
	})
	for _, since := range []string{"", "2026-10-05T09:00:00Z", "2026-10-05T10:00:00Z"} {
		args := map[string]any{"task_id": id}
		if since != "" {
			args["since"] = since
		}
		out := callGetTaskCompact(t, s, args)
		task := out["task"].(map[string]any)
		if task["status"] != "in_progress" {
			t.Errorf("status slug missing: %v", task)
		}
		if task["updated_at"] != "2026-10-05T10:00:00Z" {
			t.Error("since anchor lost")
		}
		for _, k := range []string{"status_id", "assignee_id", "created_by", "project_id", "position", "vcs_link_count", "is_shipped", "human_gate_class", "custom_fields", "dod_checks"} {
			if _, ok := task[k]; ok {
				t.Errorf("compact contains %s", k)
			}
		}
		if since != "2026-10-05T10:00:00Z" && task["human_gate"] != true {
			t.Error("active approval gate must stay visible")
		}
		if since != "2026-10-05T10:00:00Z" && task["description"] != "context" {
			t.Error("description missing")
		}
		if since == "2026-10-05T10:00:00Z" && (task["description"] != nil || out["task_changed"] != false) {
			t.Error("unchanged task repeated")
		}
	}
	task := callGetTaskCompact(t, s, map[string]any{"task_id": id, "full": true})["task"].(map[string]any)
	if task["status_id"] != statusID || task["human_gate"] != true {
		t.Error("full must preserve REST fields")
	}
}

func TestWave3StatusLookupFailurePreservesRead(t *testing.T) {
	id := uuid.New().String()
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/statuses") {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id, "description": "decision", "status_id": "s"})
	})
	task := callGetTaskCompact(t, s, map[string]any{"task_id": id})["task"].(map[string]any)
	if task["description"] != "decision" || task["status_id"] != "s" || task["status_lookup_error"] == nil {
		t.Error("failed lookup must preserve context and identify fallback")
	}
}

func TestWave3CommentTimeAndInlineAuto(t *testing.T) {
	id := uuid.New().String()
	body := "[fiddler] ✅ completed\nextra log"
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/comments") {
			c := wave2Comment("c", body, true)
			c["created_at"] = "2026-10-05T13:04:59.123+03:00"
			_ = json.NewEncoder(w).Encode(map[string]any{"items": []any{c}, "total_count": 1, "has_more": false})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"id": id})
	})
	for _, mode := range []string{"default", "include_auto", "full"} {
		args := map[string]any{"task_id": id, "include_comments": true}
		if mode != "default" {
			args[mode] = true
		}
		c := callGetTaskCompact(t, s, args)["comments"].([]any)[0].(map[string]any)
		want := body
		if mode == "default" {
			want = "[fiddler] ✅ completed"
		}
		if c["body"] != want {
			t.Errorf("%s body wrong", mode)
		}
		if mode != "full" {
			if c["created_at"] != "2026-10-05T10:04Z" {
				t.Error("timestamp must be UTC minute")
			}
			if _, ok := c["is_internal"]; ok {
				t.Error("internal flag is full-only")
			}
		} else if c["created_at"] != "2026-10-05T13:04:59.123+03:00" || c["is_internal"] != true {
			t.Error("full comment altered")
		}
	}
	// Operational warnings and regular decisions must never be treated as a completion log.
	s2 := newCommentsTestServer(t, []map[string]any{wave2Comment("c", "[fiddler] warning\n❓ Blocking @pavel: ask", false)})
	if callListComments(t, s2, map[string]any{"task_id": id})["items"].([]any)[0].(map[string]any)["body"] != "[fiddler] warning\n❓ Blocking @pavel: ask" {
		t.Error("warning summarized")
	}
}

func TestWave3AfterWalkFiltersBeforePaging(t *testing.T) {
	ids := make([]string, 205)
	for i := range ids {
		ids[i] = uuid.New().String()
	}
	rows := make([]any, len(ids))
	for i := range ids {
		c := wave2Comment(ids[i], fmt.Sprintf("body-%d", i), false)
		c["created_at"] = fmt.Sprintf("2026-10-%02dT10:%02d:%02dZ", 1+i/1440, (i/60)%60, i%60)
		rows[len(ids)-1-i] = c
	}
	s := compactTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		size, _ := strconv.Atoi(r.URL.Query().Get("page_size"))
		if page < 1 {
			page = 1
		}
		if size < 1 {
			size = 10
		}
		start := (page - 1) * size
		if start > len(rows) {
			start = len(rows)
		}
		end := start + size
		if end > len(rows) {
			end = len(rows)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": rows[start:end], "has_more": end < len(rows), "page": page, "total_count": len(rows)})
	})
	for _, cursor := range []string{ids[2], "2026-10-01T10:00:02Z"} {
		out := callListComments(t, s, map[string]any{"task_id": "t", "after": cursor, "limit": 10, "order": "asc", "page": 2})
		items := out["items"].([]any)
		if len(items) != 10 || items[0].(map[string]any)["id"] != ids[13] || out["total_count"] != float64(202) || out["has_more"] != true {
			t.Errorf("delta paging wrong: %v", out)
		}
	}
	out := callListComments(t, s, map[string]any{"task_id": "t", "after": ids[204]})
	if len(out["items"].([]any)) != 0 || out["total_count"] != float64(0) {
		t.Error("latest cursor should be empty")
	}
	for _, cursor := range []string{"", "bad", uuid.New().String()} {
		res, err := s.handleListComments(context.Background(), requestWith(map[string]any{"task_id": "t", "after": cursor}))
		if err != nil || !res.IsError {
			t.Errorf("unknown/invalid cursor accepted: %q", cursor)
		}
	}
}

func TestWave3AutoSummaryAndEscape(t *testing.T) {
	for _, prefix := range []string{"[fiddler] ✅ completed", "[balancer] assigned", "🔀 **fleet-balancer сменил исполнителя: Khan → Hugh**", "INTAKE: accepted", "🔀 INTAKE-DECOMPOSE", "🔼 INTAKE-PROMOTE", "🏁 INTAKE-PARENT-CLOSE"} {
		t.Run(prefix, func(t *testing.T) {
			body := prefix + "\nsecond line\nthird line"
			s := newCommentsTestServer(t, []map[string]any{wave2Comment("auto", body, false), wave2Comment("human", "Decision\n"+strings.Repeat("x", 2000), false)})
			for _, flag := range []string{"default", "include_auto", "full"} {
				args := map[string]any{"task_id": "t"}
				if flag != "default" {
					args[flag] = true
				}
				items := callListComments(t, s, args)["items"].([]any)
				want := body
				if flag == "default" {
					want = prefix
				}
				if got := items[0].(map[string]any)["body"]; got != want {
					t.Errorf("%s auto body = %q, want %q", flag, got, want)
				}
				if got := items[1].(map[string]any)["body"]; got != "Decision\n"+strings.Repeat("x", 2000) {
					t.Error("ordinary comment summarized")
				}
			}
		})
	}
}
