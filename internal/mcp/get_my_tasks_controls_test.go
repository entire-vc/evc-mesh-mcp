package mcp

import (
	"fmt"
	"testing"
)

// Red→green controls for the get_my_tasks conditional-revision protocol. Each
// check is a function of a rig (a get_my_tasks caller plus fixture mutators).
// It must pass on the real handler (green) and fail on a deliberately broken
// implementation of the same rig (red); a check that cannot fail proves nothing.

type controlRig struct {
	call      func(args map[string]any) map[string]any
	callOther func(args map[string]any) map[string]any // another agent identity, same data
	restart   func(args map[string]any) map[string]any // fresh process, same data
	mutate    func()                                   // child/authoritative version bump only: updated_at and visible fields unchanged
	remove    func()                                   // delete the second card
}

func realRig(t *testing.T) controlRig {
	fixture := revisionFixture()
	s, other, fresh := myTasksHarness(t, fixture), myTasksHarness(t, fixture), myTasksHarness(t, fixture)
	return controlRig{
		call:      func(a map[string]any) map[string]any { return callGetMyTasks(t, s, a) },
		callOther: func(a map[string]any) map[string]any { return callGetMyTasks(t, other, a) },
		restart:   func(a map[string]any) map[string]any { return callGetMyTasks(t, fresh, a) },
		mutate:    func() { fixture["tasks"].([]any)[0].(map[string]any)["version"] = 2 },
		remove:    func() { fixture["tasks"] = fixture["tasks"].([]any)[:1] },
	}
}

// brokenUpdatedAt: revision derived from updated_at alone (the pre-existing
// idea the card rules out): version bumps and deletions of older cards pass
// unnoticed, and nothing is scoped by filter, session, identity or full.
func brokenRig(t *testing.T, kind string) controlRig {
	r := realRig(t)
	real := r.call
	var last string
	naive := func(args map[string]any) map[string]any {
		known, _ := args["known_revision"].(string)
		if known != "" && known == last && kind != "always-cold" && kind != "fallback" {
			return map[string]any{"unchanged": true, "revision": last}
		}
		if kind == "fallback" && known != "" {
			return map[string]any{"unchanged": true, "revision": known}
		}
		clean := map[string]any{}
		for k, v := range args {
			if k != "known_revision" {
				clean[k] = v
			}
		}
		out := real(clean)
		if rev, ok := out["revision"].(string); ok && kind != "always-cold" {
			if kind == "unscoped" || last == "" {
				last = rev
			}
		}
		return out
	}
	r.call, r.callOther, r.restart = naive, naive, naive
	return r
}

func complete(out map[string]any) error {
	if _, ok := out["tasks"].([]any); !ok || out["unchanged"] == true || out["delta"] == true {
		return fmt.Errorf("not a complete cold response: %v", ctlKeys(out))
	}
	if r, _ := out["revision"].(string); len(r) != 64 {
		return fmt.Errorf("cold response carries no revision")
	}
	return nil
}

func ctlKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

var myTasksControls = []struct {
	name  string
	check func(r controlRig) error
}{
	{"repeat", func(r controlRig) error {
		rev, _ := r.call(map[string]any{})["revision"].(string)
		got := r.call(map[string]any{"known_revision": rev})
		if got["unchanged"] != true || len(got) != 2 || got["revision"] != rev {
			return fmt.Errorf("same revision did not return exact unchanged: %v", ctlKeys(got))
		}
		return nil
	}},
	{"mutation", func(r controlRig) error {
		rev, _ := r.call(map[string]any{})["revision"].(string)
		r.mutate()
		got := r.call(map[string]any{"known_revision": rev})
		if err := complete(got); err != nil {
			return fmt.Errorf("after mutation: %w", err)
		}
		if got["revision"] == rev {
			return fmt.Errorf("revision did not change after mutation")
		}
		return nil
	}},
	{"deletion", func(r controlRig) error {
		rev, _ := r.call(map[string]any{})["revision"].(string)
		r.remove()
		got := r.call(map[string]any{"known_revision": rev})
		if err := complete(got); err != nil {
			return fmt.Errorf("after deletion: %w", err)
		}
		if n := len(got["tasks"].([]any)); n != 1 {
			return fmt.Errorf("deleted card still listed (%d tasks)", n)
		}
		return nil
	}},
	{"filter", func(r controlRig) error {
		rev, _ := r.call(map[string]any{"status_category": "todo"})["revision"].(string)
		if err := complete(r.call(map[string]any{"status_category": "review", "known_revision": rev})); err != nil {
			return fmt.Errorf("revision of one filter accepted for another: %w", err)
		}
		return nil
	}},
	{"session", func(r controlRig) error {
		rev, _ := r.call(map[string]any{"snapshot_session": "A"})["revision"].(string)
		if err := complete(r.call(map[string]any{"snapshot_session": "B", "known_revision": rev})); err != nil {
			return fmt.Errorf("revision crossed a consumer session: %w", err)
		}
		if err := complete(r.callOther(map[string]any{"snapshot_session": "A", "known_revision": rev})); err != nil {
			return fmt.Errorf("revision crossed an agent identity: %w", err)
		}
		return nil
	}},
	{"full", func(r controlRig) error {
		rev, _ := r.call(map[string]any{})["revision"].(string)
		got := r.call(map[string]any{"full": true, "known_revision": rev})
		if got["unchanged"] == true {
			return fmt.Errorf("full=true answered unchanged")
		}
		tasks, _ := got["tasks"].([]any)
		if len(tasks) == 0 {
			return fmt.Errorf("full=true returned no tasks")
		}
		if d, _ := tasks[0].(map[string]any)["description"].(string); len(d) < 100 {
			return fmt.Errorf("full=true description not complete (%d chars)", len(d))
		}
		return nil
	}},
	{"fallback", func(r controlRig) error {
		for _, bad := range []string{"not-a-revision", fmt.Sprintf("%064x", 1)} {
			if err := complete(r.call(map[string]any{"known_revision": bad})); err != nil {
				return fmt.Errorf("unknown revision %.12q: %w", bad, err)
			}
		}
		rev, _ := r.call(map[string]any{})["revision"].(string)
		if err := complete(r.restart(map[string]any{"known_revision": rev})); err != nil {
			return fmt.Errorf("revision after restart: %w", err)
		}
		return nil
	}},
}

// kinds of breakage that must turn each control red.
var brokenKinds = map[string][]string{
	"repeat":   {"always-cold"},
	"mutation": {"unscoped"},
	"deletion": {"unscoped"},
	"filter":   {"unscoped"},
	"session":  {"unscoped"},
	"full":     {"unscoped"},
	"fallback": {"fallback"},
}

func TestMyTasksRedGreenControls(t *testing.T) {
	for _, c := range myTasksControls {
		t.Run(c.name, func(t *testing.T) {
			for _, kind := range brokenKinds[c.name] {
				err := c.check(brokenRig(t, kind))
				if err == nil {
					t.Fatalf("RED control did not fail on broken input %q: the check proves nothing", kind)
				}
				t.Logf("RED   %-8s broken=%-11s -> %v", c.name, kind, err)
			}
			if err := c.check(realRig(t)); err != nil {
				t.Fatalf("GREEN %s failed on main: %v", c.name, err)
			}
			t.Logf("GREEN %-8s main", c.name)
		})
	}
}
