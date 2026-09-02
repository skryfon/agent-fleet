// Package budget enforces per-run and per-feature usd/minute/question caps
// with a hard kill on breach (development-plan.md §4 M4, §6). Pure
// evaluation, like internal/policy — no IO, no clock: internal/api's usage
// handler is what reads/writes the `budget` table and reacts to a Breach.
package budget

import "fmt"

// Caps is one budget scope's configured ceiling (development-plan.md §6:
// "Cap questions per run (3) and per feature (10)"; usd/minutes are the M4
// hard-kill dimensions).
type Caps struct {
	USD       float64
	Minutes   int
	Questions int
}

// Spent is what a scope has consumed so far.
type Spent struct {
	USD       float64
	Minutes   int
	Questions int
}

// Breach names which dimension broke its cap first, checked in the order
// usd, minutes, questions — an arbitrary but fixed order so Check is
// deterministic when more than one dimension is over.
type Breach struct {
	Kind   string // "usd" | "minutes" | "questions"
	Limit  string
	Actual string
}

func (b Breach) String() string {
	return fmt.Sprintf("%s cap exceeded: %s > %s", b.Kind, b.Actual, b.Limit)
}

// Check returns nil when spent is within every configured cap. A zero cap
// means "uncapped" for that dimension — a scope with no budget row
// configured must not spuriously breach.
func Check(caps Caps, spent Spent) *Breach {
	if caps.USD > 0 && spent.USD > caps.USD {
		return &Breach{Kind: "usd", Limit: fmt.Sprintf("%.4f", caps.USD), Actual: fmt.Sprintf("%.4f", spent.USD)}
	}

	if caps.Minutes > 0 && spent.Minutes > caps.Minutes {
		return &Breach{Kind: "minutes", Limit: fmt.Sprintf("%d", caps.Minutes), Actual: fmt.Sprintf("%d", spent.Minutes)}
	}

	if caps.Questions > 0 && spent.Questions > caps.Questions {
		return &Breach{Kind: "questions", Limit: fmt.Sprintf("%d", caps.Questions), Actual: fmt.Sprintf("%d", spent.Questions)}
	}

	return nil
}
