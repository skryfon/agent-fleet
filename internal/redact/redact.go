// Package redact scrubs secrets out of anything that will be persisted or
// logged. development-plan.md §8: "Secrets never enter agent context or
// event payloads — redaction filter applies to every emitted event; test it
// with a canary string."
//
// internal/store.ApplyTaskTransition/ApplyRunTransition (control-plane-native
// events) and internal/store.AppendMirror (the dsh session mirror) are the
// two sanctioned choke points that call this package for every event
// payload they write. This is enforced by code-review discipline, not the
// compiler — Go has no way to make internal/store/gen's sqlc-generated
// AppendMirrorEvents unreachable except through AppendMirror while keeping
// that package public for sqlc's own generation model — the same posture
// `event`'s append-only-ness has outside its Postgres trigger: a rule
// everyone touching this code must know, not one every path is structurally
// incapable of violating. A new code path that needs to write an event row
// gets a new *exported* internal/store method that also redacts; it never
// calls the generated query directly.
package redact

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
)

const mask = "[REDACTED]"

// builtinPatterns catches common credential shapes regardless of which
// service issued them. Replacement is always the fixed mask string above,
// never a length-preserving one — a length-preserving mask leaks the
// secret's length, which is itself sometimes enough to narrow a brute force.
//
// Deliberately no LEADING \b before a token's own prefix: a real secret
// often appears embedded after another word character (an env var line
// "SOME_VAR_ghp_xxx", a URL query string, a log line's own prefix), and
// regexp's \b treats "_" as a word character — "...RY_ghp_..." has no word
// boundary at that point, so a leading \b silently fails to match exactly
// the secrets most likely to show up mid-string. Trailing \b is kept: it
// only needs to stop the match at the end of the token's own alphanumeric
// run, where a false negative is not a concern.
//
// This set is deliberately NOT exhaustive: an unprefixed, low-structure
// secret (a Zulip bot API key, a plain shared secret) has no distinctive
// shape a regex can key on without an unacceptable false-positive rate.
// Those are caught only via the literal list (New/FromEnv/WithLiterals) —
// every known deployment secret (GH_TOKEN, OMNI_ROUTE_API_KEY,
// ZULIP_BOT_API_KEY, the per-run bearer token, ...) must be registered as a
// literal at the call site; this pattern set is a second line for the
// *unknown* case (a stray key pasted into a commit message, a leaked
// third-party token), not the primary defense.
var builtinPatterns = []*regexp.Regexp{
	// GitHub tokens: personal access (classic ghp_, fine-grained
	// github_pat_), OAuth (gho_), server-to-server (ghs_).
	regexp.MustCompile(`gh[ps]_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`gho_[A-Za-z0-9]{20,}\b`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{20,}\b`),
	// Generic "sk-" secret-key prefix (OpenAI-style and others).
	regexp.MustCompile(`sk-[A-Za-z0-9]{20,}\b`),
	// AWS access key id.
	regexp.MustCompile(`AKIA[0-9A-Z]{16}\b`),
	// PEM private key blocks (any algorithm).
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----[\s\S]*?-----END [A-Z ]*PRIVATE KEY-----`),
	// Bearer tokens in an Authorization-header-shaped string.
	regexp.MustCompile(`Bearer\s+[A-Za-z0-9._~+/=-]{20,}`),
	// JWTs: three base64url segments separated by dots.
	regexp.MustCompile(`eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\b`),
}

// Redactor scrubs secret literals and secret-shaped patterns from strings
// and JSON documents. The zero value is not usable — construct with New or
// FromEnv.
type Redactor struct {
	literals []string // exact substrings to mask, longest first (avoids partial-mask overlap)
	patterns []*regexp.Regexp
}

// New builds a Redactor from an explicit list of secret literals (exact
// values, not names) plus any additional patterns beyond the built-in set.
func New(literals []string, extra []*regexp.Regexp) *Redactor {
	r := &Redactor{
		literals: dedupeNonEmpty(literals),
		patterns: append(append([]*regexp.Regexp{}, builtinPatterns...), extra...),
	}
	r.sortLiteralsLongestFirst()

	return r
}

// FromEnv builds a Redactor whose literal set is the current value of each
// named environment variable that is actually set — e.g.
// FromEnv("GH_TOKEN", "OMNI_ROUTE_API_KEY", "DATABASE_URL"). Unset names are
// silently skipped (nothing to redact).
func FromEnv(lookup func(string) (string, bool), names ...string) *Redactor {
	literals := make([]string, 0, len(names))

	for _, name := range names {
		if v, ok := lookup(name); ok && v != "" {
			literals = append(literals, v)
		}
	}

	return New(literals, nil)
}

// WithLiterals returns a new Redactor that also masks the given values — for
// request-scoped secrets (e.g. one run's bearer token) layered on top of a
// process-wide base Redactor built by FromEnv. Does not mutate the receiver.
func (r *Redactor) WithLiterals(more ...string) *Redactor {
	next := &Redactor{
		literals: dedupeNonEmpty(append(append([]string{}, r.literals...), more...)),
		patterns: r.patterns,
	}
	next.sortLiteralsLongestFirst()

	return next
}

func (r *Redactor) sortLiteralsLongestFirst() {
	// A shorter literal that happens to be a substring of a longer one
	// (e.g. a raw API key that is itself a substring of "Bearer <key>")
	// must not mask before the longer, more specific one runs.
	for i := 1; i < len(r.literals); i++ {
		for j := i; j > 0 && len(r.literals[j-1]) < len(r.literals[j]); j-- {
			r.literals[j-1], r.literals[j] = r.literals[j], r.literals[j-1]
		}
	}
}

func dedupeNonEmpty(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))

	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}

		seen[s] = true

		out = append(out, s)
	}

	return out
}

// String returns s with every known literal and every built-in secret
// pattern replaced by the fixed mask.
func (r *Redactor) String(s string) string {
	for _, lit := range r.literals {
		s = strings.ReplaceAll(s, lit, mask)
	}

	for _, pat := range r.patterns {
		s = pat.ReplaceAllString(s, mask)
	}

	return s
}

// JSON parses raw as arbitrary JSON, redacts every string it finds at any
// depth (object values, array elements, object keys), and returns the
// result re-marshaled. Non-string JSON values (numbers, booleans, null) pass
// through unchanged — only string leaves can carry a secret. Returns an
// error only if raw is not valid JSON; a payload event.payload is expected
// to always be, so callers should treat an error here as a bug in the
// caller, not a redaction failure to swallow.
func (r *Redactor) JSON(raw []byte) ([]byte, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, err
	}

	redacted := r.redactValue(v)

	var buf bytes.Buffer

	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)

	if err := enc.Encode(redacted); err != nil {
		return nil, err
	}

	// json.Encoder.Encode always appends a trailing newline; event.payload
	// is a jsonb column, not a line-oriented log, so trim it.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func (r *Redactor) redactValue(v any) any {
	switch x := v.(type) {
	case string:
		return r.String(x)
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[r.String(k)] = r.redactValue(val)
		}

		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = r.redactValue(val)
		}

		return out
	default:
		// numbers, bools, nil — nothing to redact
		return v
	}
}
