package supervisor

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestRunTokenDeterministic(t *testing.T) {
	id := uuid.New()

	a := runToken("secret", id)
	b := runToken("secret", id)

	if a != b {
		t.Fatalf("runToken not deterministic: %q != %q", a, b)
	}

	if c := runToken("other-secret", id); c == a {
		t.Fatalf("runToken ignored the secret")
	}

	if d := runToken("secret", uuid.New()); d == a {
		t.Fatalf("runToken ignored the run id")
	}
}

func TestTokenHashMatchesRunToken(t *testing.T) {
	token := runToken("secret", uuid.New())

	h1 := tokenHash(token)
	h2 := tokenHash(token)

	if string(h1) != string(h2) {
		t.Fatalf("tokenHash not deterministic")
	}

	if string(h1) == token {
		t.Fatalf("tokenHash returned the plaintext")
	}
}

func TestTaskPromptIncludesAcceptanceCriteria(t *testing.T) {
	prompt := taskPrompt("Add widget", "Implement the widget.", []string{"widget renders", "widget is tested"})

	for _, want := range []string{"Add widget", "Implement the widget.", "widget renders", "widget is tested"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("taskPrompt missing %q: %s", want, prompt)
		}
	}
}

func TestTaskPromptNoAcceptanceCriteria(t *testing.T) {
	prompt := taskPrompt("Add widget", "Implement the widget.", nil)

	if strings.Contains(prompt, "Acceptance criteria") {
		t.Errorf("taskPrompt added an empty acceptance-criteria section: %s", prompt)
	}
}
