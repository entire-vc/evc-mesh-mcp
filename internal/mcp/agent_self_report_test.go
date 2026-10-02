package mcp

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	mcpsdk "github.com/mark3labs/mcp-go/mcp"
)

type recordedCall struct {
	method, path string
	body         map[string]any
}

func selfReportServer(t *testing.T) (*Server, *[]recordedCall) {
	t.Helper()
	var calls []recordedCall
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		_ = json.NewDecoder(r.Body).Decode(&b)
		calls = append(calls, recordedCall{r.Method, r.URL.Path, b})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(srv.Close)
	return &Server{
		restClient: NewRESTClient(srv.URL, "test-key"),
		tracker:    NewSessionTracker(),
		session:    &AgentSession{},
	}, &calls
}

func reqWith(args map[string]any) mcpsdk.CallToolRequest {
	req := mcpsdk.CallToolRequest{}
	req.Params.Arguments = args
	return req
}

func TestUpdateAgentProfile_SendsHarnessAndModel(t *testing.T) {
	s, calls := selfReportServer(t)
	res, err := s.handleUpdateAgentProfile(context.Background(),
		reqWith(map[string]any{"agent_type": "codex", "model": " gpt-6.1-sol "}))
	if err != nil || res.IsError {
		t.Fatalf("unexpected failure: %v %+v", err, res)
	}
	if len(*calls) != 1 || (*calls)[0].method != http.MethodPatch || (*calls)[0].path != "/api/v1/agents/me" {
		t.Fatalf("want one PATCH /agents/me, got %+v", *calls)
	}
	b := (*calls)[0].body
	if b["agent_type"] != "codex" || b["model"] != "gpt-6.1-sol" {
		t.Fatalf("body = %+v", b)
	}
}

func TestUpdateAgentProfile_EmptyModelResets(t *testing.T) {
	s, calls := selfReportServer(t)
	res, _ := s.handleUpdateAgentProfile(context.Background(), reqWith(map[string]any{"model": ""}))
	if res.IsError || len(*calls) != 1 {
		t.Fatalf("calls=%+v res=%+v", *calls, res)
	}
	if v, ok := (*calls)[0].body["model"]; !ok || v != "" {
		t.Fatalf("empty model must be sent as \"\", body=%+v", (*calls)[0].body)
	}
}

func TestUpdateAgentProfile_InvalidInputFailsBeforeAnyWrite(t *testing.T) {
	for name, args := range map[string]map[string]any{
		"bad harness":   {"agent_type": "skynet", "role": "dev"},
		"blank harness": {"agent_type": "  ", "role": "dev"},
		"long model":    {"model": strings.Repeat("x", 129), "role": "dev"},
	} {
		t.Run(name, func(t *testing.T) {
			s, calls := selfReportServer(t)
			res, _ := s.handleUpdateAgentProfile(context.Background(), reqWith(args))
			if !res.IsError {
				t.Fatalf("want error result, got %+v", res)
			}
			if len(*calls) != 0 {
				t.Fatalf("no request may be sent, got %+v", *calls)
			}
		})
	}
}

func TestHeartbeat_ForwardsHarnessAndModel(t *testing.T) {
	s, calls := selfReportServer(t)
	res, _ := s.handleHeartbeat(context.Background(),
		reqWith(map[string]any{"status": "idle", "agent_type": "codex", "model": "gpt-6.1-sol"}))
	if res.IsError || len(*calls) != 1 {
		t.Fatalf("calls=%+v res=%+v", *calls, res)
	}
	b := (*calls)[0].body
	if (*calls)[0].path != "/api/v1/agents/heartbeat" || b["agent_type"] != "codex" || b["model"] != "gpt-6.1-sol" {
		t.Fatalf("call = %+v", (*calls)[0])
	}
}

func TestHeartbeat_OmittedFieldsNotSent(t *testing.T) {
	s, calls := selfReportServer(t)
	_, _ = s.handleHeartbeat(context.Background(), reqWith(map[string]any{"status": "idle"}))
	b := (*calls)[0].body
	if _, ok := b["model"]; ok {
		t.Fatalf("model must not be sent when omitted: %+v", b)
	}
	if _, ok := b["agent_type"]; ok {
		t.Fatalf("agent_type must not be sent when omitted: %+v", b)
	}
}

func TestHeartbeat_InvalidHarnessRejectedLocally(t *testing.T) {
	s, calls := selfReportServer(t)
	res, _ := s.handleHeartbeat(context.Background(), reqWith(map[string]any{"agent_type": "skynet"}))
	if !res.IsError || len(*calls) != 0 {
		t.Fatalf("want local error, calls=%+v res=%+v", *calls, res)
	}
}
