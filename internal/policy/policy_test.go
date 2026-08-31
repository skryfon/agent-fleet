package policy_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"agentfleet/internal/policy"
)

// fixture mirrors one testdata/*.json file. request.args (a JSON object) is
// the common case; request.raw_args (an arbitrary string) exists
// specifically so a fixture can inject deliberately-invalid JSON to prove
// Evaluate fails closed on malformed arguments.
type fixture struct {
	Name    string `json:"name"`
	Request struct {
		Role     string          `json:"role"`
		Tool     string          `json:"tool"`
		Args     json.RawMessage `json:"args"`
		RawArgs  *string         `json:"raw_args"`
		Manifest policy.Manifest `json:"manifest"`
	} `json:"request"`
	Want struct {
		Allow bool   `json:"allow"`
		Rule  string `json:"rule"`
	} `json:"want"`
}

// TestEvaluateGolden walks every fixture in testdata/ — the corpus IS the
// spec for internal/policy.Evaluate; a new denial case is added here, not
// asserted only in prose.
func TestEvaluateGolden(t *testing.T) {
	files, err := filepath.Glob("testdata/*.json")
	if err != nil {
		t.Fatalf("globbing testdata: %v", err)
	}

	if len(files) == 0 {
		t.Fatal("no fixtures found in testdata/ — golden corpus is empty")
	}

	for _, path := range files {
		t.Run(filepath.Base(path), func(t *testing.T) {
			raw, err := os.ReadFile(path) //nolint:gosec // fixture paths come from filepath.Glob against a fixed testdata dir, not user input
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}

			var f fixture
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatalf("parsing %s: %v", path, err)
			}

			args := f.Request.Args
			if f.Request.RawArgs != nil {
				args = json.RawMessage(*f.Request.RawArgs)
			}

			got := policy.Evaluate(policy.Request{
				Role:     f.Request.Role,
				Tool:     f.Request.Tool,
				Args:     args,
				Manifest: f.Request.Manifest,
			})

			if got.Allow != f.Want.Allow {
				t.Errorf("%s: Allow = %v, want %v (reason: %q)", f.Name, got.Allow, f.Want.Allow, got.Reason)
			}

			if f.Want.Rule != "" && got.Rule != f.Want.Rule {
				t.Errorf("%s: Rule = %q, want %q", f.Name, got.Rule, f.Want.Rule)
			}

			if !f.Want.Allow && got.Reason == "" {
				t.Errorf("%s: denied with no Reason — every denial must explain itself (it lands verbatim in the policy_violation event)", f.Name)
			}
		})
	}
}

// TestHardDenyWinsOverEverything proves the hard-deny floor fires even when
// the role itself is unknown to the manifest — D3's "no merge tool, ever" is
// not conditional on manifest validity.
func TestHardDenyWinsOverEverything(t *testing.T) {
	got := policy.Evaluate(policy.Request{
		Role: "no-such-role",
		Tool: "gh_pr_merge",
	})

	if got.Allow {
		t.Fatal("gh_pr_merge was allowed despite an unknown role — hard-deny must win regardless")
	}

	if got.Rule != "hard_deny" {
		t.Fatalf("Rule = %q, want hard_deny", got.Rule)
	}
}

// TestEvaluateNoRulesMeansAllow proves a tool with no arg_rules entry is
// allowed outright once it clears the allow-list — arg rules are additive
// restrictions, never implicitly required.
func TestEvaluateNoRulesMeansAllow(t *testing.T) {
	got := policy.Evaluate(policy.Request{
		Role: "implementer",
		Tool: "spawn_worker",
		Args: json.RawMessage(`{"anything":"goes"}`),
		Manifest: policy.Manifest{
			Roles: map[string]policy.Role{
				"implementer": {MediatedTools: []string{"spawn_worker"}},
			},
		},
	})

	if !got.Allow {
		t.Fatalf("expected allow, got deny: %s (%s)", got.Reason, got.Rule)
	}
}

// TestEvaluateEmptyArgsWithRulesDenies proves a tool that HAS arg_rules but
// receives no arguments at all is denied, not vacuously allowed — a rule
// path that's simply absent must fail its predicate (present == false), not
// be skipped.
func TestEvaluateEmptyArgsWithRulesDenies(t *testing.T) {
	got := policy.Evaluate(policy.Request{
		Role: "implementer",
		Tool: "gh_pr_create",
		Manifest: policy.Manifest{
			Roles: map[string]policy.Role{
				"implementer": {
					MediatedTools: []string{"gh_pr_create"},
					ArgRules: map[string][]policy.Rule{
						"gh_pr_create": {{Path: "base", Op: policy.OpEquals, Value: "develop"}},
					},
				},
			},
		},
	})

	if got.Allow {
		t.Fatal("expected deny: required arg 'base' was never supplied")
	}
}
