package fanout

import "testing"

func TestCheck(t *testing.T) {
	caps := Caps{MaxDepth: 3, MaxChildrenPerRun: 4, MaxActiveSubtree: 10}

	cases := []struct {
		name                                string
		caps                                Caps
		childDepth, siblings, activeSubtree int
		wantAllow                           bool
		wantRule                            string
	}{
		{"under every cap", caps, 1, 0, 1, true, ""},
		{"depth breach", caps, 4, 0, 1, false, "max_depth"},
		{"depth exactly at cap is allowed", caps, 3, 0, 1, true, ""},
		{"children breach", caps, 1, 4, 1, false, "max_children"},
		{"children under cap allowed", caps, 1, 3, 1, true, ""},
		{"subtree breach", caps, 1, 0, 11, false, "max_subtree"},
		{"subtree exactly at cap is allowed", caps, 1, 0, 10, true, ""},
		{"depth checked first when several breach", caps, 4, 99, 99, false, "max_depth"},
		{"zero caps mean uncapped", Caps{}, 999, 999, 999, true, ""},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := Check(c.childDepth, c.siblings, c.activeSubtree, c.caps)

			if got.Allow != c.wantAllow {
				t.Fatalf("Check() Allow = %v, want %v (reason: %q)", got.Allow, c.wantAllow, got.Reason)
			}

			if !c.wantAllow {
				if got.Rule != c.wantRule {
					t.Fatalf("Check().Rule = %q, want %q", got.Rule, c.wantRule)
				}

				if got.Reason == "" {
					t.Fatal("denied with no Reason")
				}
			}
		})
	}
}

// TestCheckAllowHasNoRule proves an allowed Decision carries no stray rule
// label — a caller must be able to tell "allowed" from "denied by an empty
// rule name" without also checking Allow.
func TestCheckAllowHasNoRule(t *testing.T) {
	got := Check(1, 0, 1, Caps{MaxDepth: 3, MaxChildrenPerRun: 4, MaxActiveSubtree: 10})

	if got.Rule != "" {
		t.Fatalf("Rule = %q on an allowed decision, want empty", got.Rule)
	}
}
