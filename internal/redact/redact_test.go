package redact_test

import (
	"encoding/json"
	"strings"
	"testing"

	"agentfleet/internal/redact"
)

// TestCanary is the canary test .claude/CLAUDE.md and development-plan.md §8
// both call for explicitly: "redaction filter applies to every emitted
// event; test it with a canary string."
func TestCanary(t *testing.T) {
	const canary = "AGENTFLEET_CANARY_ghp_0000000000000000000000000000000000"

	r := redact.New(nil, nil)
	got := r.String("here is a token: " + canary + " end")

	if strings.Contains(got, canary) {
		t.Fatalf("canary secret survived redaction: %q", got)
	}

	if strings.Contains(got, "0000000000000000000000000000000000") {
		t.Fatalf("canary secret's token body survived redaction: %q", got)
	}
}

func TestCanaryThroughJSON(t *testing.T) {
	const canary = "ghp_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"

	r := redact.New(nil, nil)
	payload, _ := json.Marshal(map[string]any{
		"nested": map[string]any{
			"list": []any{"a", canary, "b"},
		},
	})

	out, err := r.JSON(payload)
	if err != nil {
		t.Fatalf("JSON() error: %v", err)
	}

	if strings.Contains(string(out), canary) {
		t.Fatalf("canary secret survived JSON redaction: %s", out)
	}
}

// TestNoOverRedaction is the control case: a non-secret string of similar
// shape/length must survive unchanged, or the redactor is too aggressive to
// be useful (an event log full of [REDACTED] is as useless as one full of
// secrets).
func TestNoOverRedaction(t *testing.T) {
	r := redact.New(nil, nil)

	cases := []string{
		"the quick brown fox jumps over the lazy dog",
		"task-33333333-3333-3333-3333-333333333333",
		"this string is exactly as long as a github token but has no prefix xxxxxxxxxxxxxxxxxxxx",
		"",
	}

	for _, s := range cases {
		if got := r.String(s); got != s {
			t.Errorf("String(%q) = %q; want unchanged (over-redaction)", s, got)
		}
	}
}

func TestBuiltinPatterns(t *testing.T) {
	cases := map[string]string{
		"github classic pat": "ghp_1234567890abcdef1234567890abcdef1234",
		"github fine-grained": "github_pat_11AAAAAAA0aaaaaaaaaaaa_" +
			"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"openai-style key": "sk-abcdefghijklmnopqrstuvwxyz123456",
		"aws access key":   "AKIAABCDEFGHIJKLMNOP",
		"bearer token":     "Bearer abcdefghijklmnopqrstuvwxyz0123456789",
		"jwt":              "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.dozjgNryP4J3jVmNHl0w5N_XgL0n3I9PlFUP0THsR8U",
		"pem private key": "-----BEGIN RSA PRIVATE KEY-----\n" +
			"MIIBOgIBAAJBAK...\n-----END RSA PRIVATE KEY-----",
	}

	for name, secret := range cases {
		t.Run(name, func(t *testing.T) {
			r := redact.New(nil, nil)

			got := r.String("prefix " + secret + " suffix")
			if strings.Contains(got, secret) {
				t.Fatalf("%s: secret survived: %q", name, got)
			}
		})
	}
}

func TestLiteralsAndWithLiterals(t *testing.T) {
	base := redact.New([]string{"base-secret"}, nil)

	if got := base.String("x base-secret y"); strings.Contains(got, "base-secret") {
		t.Fatalf("base literal survived: %q", got)
	}

	scoped := base.WithLiterals("run-token-abc")
	if got := scoped.String("x run-token-abc y base-secret z"); strings.Contains(got, "run-token-abc") || strings.Contains(got, "base-secret") {
		t.Fatalf("scoped literal or base literal survived: %q", got)
	}

	// WithLiterals must not mutate the receiver.
	if got := base.String("run-token-abc"); got != "run-token-abc" {
		t.Fatalf("WithLiterals mutated the base Redactor: base now redacts %q", got)
	}
}

func TestLongerLiteralWinsOverSubstring(t *testing.T) {
	// "secret" is a substring of "Bearer secret123456789012345"; masking the
	// short literal first would leave a mangled remnant of the long one.
	r := redact.New([]string{"secret", "Bearer secret123456789012345"}, nil)

	got := r.String("token=Bearer secret123456789012345 end")
	if strings.Contains(got, "secret123456789012345") {
		t.Fatalf("longer literal did not win: %q", got)
	}
}

func TestFromEnvSkipsUnset(t *testing.T) {
	env := map[string]string{"GH_TOKEN": "ghp_realvalue00000000000000000000000000"}
	lookup := func(name string) (string, bool) { v, ok := env[name]; return v, ok }

	r := redact.FromEnv(lookup, "GH_TOKEN", "NEVER_SET")

	got := r.String("has ghp_realvalue00000000000000000000000000 in it")
	if strings.Contains(got, "ghp_realvalue00000000000000000000000000") {
		t.Fatalf("FromEnv literal not applied: %q", got)
	}
}

func TestJSONInvalidReturnsError(t *testing.T) {
	r := redact.New(nil, nil)
	if _, err := r.JSON([]byte("not json")); err == nil {
		t.Fatal("JSON() with invalid input returned nil error")
	}
}

func TestJSONPreservesNonStringTypes(t *testing.T) {
	r := redact.New(nil, nil)

	in, _ := json.Marshal(map[string]any{"n": 42, "b": true, "nil": nil})

	out, err := r.JSON(in)
	if err != nil {
		t.Fatalf("JSON() error: %v", err)
	}

	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-parsing redacted JSON: %v", err)
	}

	if got["n"] != float64(42) || got["b"] != true || got["nil"] != nil {
		t.Fatalf("non-string values were altered: %+v", got)
	}
}
