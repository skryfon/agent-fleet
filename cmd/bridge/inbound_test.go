package main

import "testing"

// TestAnswerEscapeHatch is a pure table test for the "/answer <id> <text>"
// parsing (development-plan.md §6's fallback) — no daemon, no network,
// mirroring runner/packages/af-policy's violation()-style pure-predicate
// testing convention.
func TestAnswerEscapeHatch(t *testing.T) {
	tests := []struct {
		name, content, wantID, wantText string
		wantMatch                       bool
	}{
		{"matches id and text", "/answer 123 main branch", "123", "main branch", true},
		{"matches uuid id", "/answer 9c1f8b1e-... yes", "9c1f8b1e-...", "yes", true},
		{"no match without prefix", "just a reply", "", "", false},
		{"no match with only an id", "/answer 123", "", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := answerEscapeHatch.FindStringSubmatch(tt.content)

			if tt.wantMatch != (m != nil) {
				t.Fatalf("match = %v, want %v", m != nil, tt.wantMatch)
			}

			if !tt.wantMatch {
				return
			}

			if m[1] != tt.wantID || m[2] != tt.wantText {
				t.Fatalf("got id=%q text=%q, want id=%q text=%q", m[1], m[2], tt.wantID, tt.wantText)
			}
		})
	}
}

// TestEmojiAnswers proves the documented minimal reaction vocabulary; an
// unmapped emoji falls back to its own name as the answer (choice questions
// can have emoji-named options).
func TestEmojiAnswers(t *testing.T) {
	tests := []struct{ emoji, want string }{
		{"thumbs_up", "yes"},
		{"+1", "yes"},
		{"thumbs_down", "no"},
		{"-1", "no"},
	}

	for _, tt := range tests {
		if got := emojiAnswers[tt.emoji]; got != tt.want {
			t.Errorf("emojiAnswers[%q] = %q, want %q", tt.emoji, got, tt.want)
		}
	}

	if _, ok := emojiAnswers["party_parrot"]; ok {
		t.Fatal("unmapped emoji unexpectedly present — handleReaction's own fallback to the literal emoji name is what should handle it, not a table entry")
	}
}
