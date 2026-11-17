package mcp

// A card a live session creates for itself is held by that session.
//
// A session outside mesh-runner (a Telegram chat, an operator's terminal) that
// creates a card assigned to its own agent in an in_progress status goes on
// working on it. The Runner feeds every in_progress card of an agent to that
// agent's lane, so it started a second session of the same agent on the card
// while the first one was still decomposing it, and the duplicate session
// split the card a second time. The Runner already leaves alone
// any card with a live checkout ("checked out by another writer"), so the
// creating session takes that checkout right after the create. The lane gets
// the card after release_task, a turn command of this session (wait_for,
// hand_off, finish_task release the caller's checkout) or the lock's expiry.
//
// Opt-in: MESH_MCP_CLAIM_OWN_CREATED=1, read on every call. Never inside a
// Runner card session (MESH_RUNNER_CHECKOUT_SESSION set): there a card the lane
// creates for itself is the next unit of its queue, and the lane holds the lock
// of its own card already. A card created in todo is the queue too: only an
// explicit in_progress status is claimed. A failed checkout never fails the
// create; the result says why the card is not held.

import (
	"context"
	"os"
	"strconv"
	"strings"
)

const (
	claimOwnCreatedEnv    = "MESH_MCP_CLAIM_OWN_CREATED"
	claimOwnCreatedTTLEnv = "MESH_MCP_CLAIM_OWN_CREATED_TTL_MINUTES"
	claimOwnCreatedTTL    = 60
	claimOwnCreatedMaxTTL = 24 * 60
	claimResultField      = "_mesh_claim"

	claimHeldNote = "This session holds the card's checkout: the Runner starts no lane session on it " +
		"until release_task, a turn command of this session (wait_for, hand_off, finish_task) or the lock's expiry. " +
		"To queue it for your lane now, call release_task."
	claimRefusedNote = "The card is NOT held for this session: the Runner may start your lane on it. " +
		"Create it in todo when the lane should do it, or hand it off."
)

func claimOwnCreatedEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(claimOwnCreatedEnv))) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

func claimOwnCreatedTTLMinutes() int {
	raw := strings.TrimSpace(os.Getenv(claimOwnCreatedTTLEnv))
	if raw == "" {
		return claimOwnCreatedTTL
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > claimOwnCreatedMaxTTL {
		return claimOwnCreatedTTL
	}
	return n
}

// insideRunnerCardSession: the Runner exports its checkout of the current card
// into the harness environment (runner.py, MESH_RUNNER_CHECKOUT_*).
func insideRunnerCardSession() bool {
	return strings.TrimSpace(os.Getenv(runnerCheckoutSessionEnv)) != ""
}

// claimOwnCreated takes the checkout of a card this session just created for
// its own agent in an in_progress status. It returns what goes into the tool
// result under _mesh_claim, or nil when the card is not this session's to hold.
// statusCategory is the category of the status the caller asked for ("" when
// the project's default status was used: nothing is claimed then).
func (s *Server) claimOwnCreated(ctx context.Context, created map[string]any, statusCategory string) map[string]any {
	if !claimOwnCreatedEnabled() || insideRunnerCardSession() {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(statusCategory), "in_progress") {
		return nil
	}
	session := s.getSession(ctx)
	if session == nil || created == nil {
		return nil
	}
	taskID, _ := created["id"].(string)
	assignee, _ := created["assignee_id"].(string)
	if taskID == "" || !strings.EqualFold(strings.TrimSpace(assignee), session.AgentID.String()) {
		return nil
	}
	if kind, _ := created["assignee_type"].(string); kind != "" && !strings.EqualFold(kind, "agent") {
		return nil
	}
	ttl := claimOwnCreatedTTLMinutes()
	res, err := s.getRESTClient(ctx).CheckoutTask(ctx, taskID, ttl)
	if err != nil {
		return map[string]any{"held": false, "reason": "checkout failed: " + err.Error(), "note": claimRefusedNote}
	}
	token, _ := res["checkout_token"].(string)
	if token == "" {
		return map[string]any{"held": false, "reason": "checkout returned no token", "note": claimRefusedNote}
	}
	// release_task and extend_checkout of this session find the token here.
	s.checkouts.Store(taskID, token)
	claim := map[string]any{"held": true, "ttl_minutes": ttl, "note": claimHeldNote}
	for _, k := range []string{"expires_at", "checkout_expires", "checkout_expires_at"} {
		if v, ok := res[k]; ok && v != nil && v != "" {
			claim["expires_at"] = v
			break
		}
	}
	return claim
}
