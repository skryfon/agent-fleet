package manifest_test

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"

	"agentfleet/internal/domain/manifest"
)

// TestCompileGolden pins Policy/Caps/FanoutCaps/Patch's exact output for
// testdata/valid.yaml — the manifest-compiler-output half of the M6 done
// criterion ("snapshot-tested", development-plan.md §5). Editing manifest.go
// requires deliberately re-running with -update and reviewing the diff.
func TestCompileGolden(t *testing.T) {
	const path = "testdata/compiled.golden"

	src, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("reading testdata/valid.yaml: %v", err)
	}

	m, issues := manifest.Parse(src)
	if len(issues) > 0 {
		t.Fatalf("testdata/valid.yaml failed to parse: %v", issues)
	}

	var b strings.Builder

	fmt.Fprintln(&b, "--- policy ---")

	pol := m.Policy()

	roles := make([]string, 0, len(pol.Roles))
	for r := range pol.Roles {
		roles = append(roles, r)
	}

	sort.Strings(roles)

	for _, r := range roles {
		j, _ := json.Marshal(pol.Roles[r])
		fmt.Fprintf(&b, "%s: %s\n", r, j)
	}

	fmt.Fprintln(&b, "--- caps(implementer) ---")
	fmt.Fprintf(&b, "%+v\n", m.Caps("implementer"))

	fmt.Fprintln(&b, "--- fanout ---")
	fmt.Fprintf(&b, "%+v\n", m.FanoutCaps())

	fmt.Fprintln(&b, "--- patch(implementer) ---")

	patch, err := m.Patch("implementer")
	if err != nil {
		t.Fatalf("Patch: %v", err)
	}

	b.Write(patch)

	got := b.String()

	if *update {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatalf("writing golden file: %v", err)
		}

		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading golden file (run with -update to create it): %v", err)
	}

	if got != string(want) {
		t.Fatalf("compiled output changed — review the diff, then re-run with -update if intended.\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}

func TestPatchUnknownRole(t *testing.T) {
	src, err := os.ReadFile("testdata/valid.yaml")
	if err != nil {
		t.Fatalf("reading testdata/valid.yaml: %v", err)
	}

	m, issues := manifest.Parse(src)
	if len(issues) > 0 {
		t.Fatalf("testdata/valid.yaml failed to parse: %v", issues)
	}

	if _, err := m.Patch("nonexistent"); err == nil {
		t.Fatal("Patch(nonexistent role) should have errored")
	}
}
