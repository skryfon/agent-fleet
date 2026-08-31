package domain_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentfleet/internal/domain"
)

var update = flag.Bool("update", false, "update golden files")

func renderTaskTable() string {
	var b strings.Builder

	for _, row := range domain.TaskTable() {
		fmt.Fprintf(&b, "%s + %s -> %s [%s]", row.From, row.Trigger, row.To, row.EventKind)

		for _, e := range row.Effects {
			fmt.Fprintf(&b, " effect(%s, %s)", e.Topic, e.KeyTemplate)
		}

		b.WriteByte('\n')
	}

	return b.String()
}

// TestTaskTableGolden pins the state machine's exact shape. Editing
// transition.go's taskTable requires deliberately re-running this test with
// -update and reviewing the diff — that review IS the "table-driven, not
// scattered if-chains, auditable" property from docs/adr/0010 in practice.
func TestTaskTableGolden(t *testing.T) {
	const path = "testdata/task_transitions.golden"

	got := renderTaskTable()

	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}

		return
	}

	want, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		t.Fatalf("reading golden file (run with -update to create it): %v", err)
	}

	if got != string(want) {
		t.Fatalf("task transition table changed — review the diff, then re-run with -update if intended.\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}
