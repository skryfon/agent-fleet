package manifest_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"agentfleet/internal/domain/manifest"
)

var update = flag.Bool("update", false, "update golden files")

// render mirrors internal/domain/tasksmd_test's render: "OK" plus each
// role name in sorted order for a successful parse, or one Issue.String()
// per line (in Parse's own reported order) for a rejected one.
func render(m manifest.Manifest, issues []manifest.Issue) string {
	if len(issues) == 0 {
		var b strings.Builder

		fmt.Fprintln(&b, "OK")

		roles := make([]string, 0, len(m.Agents))
		for r := range m.Agents {
			roles = append(roles, r)
		}

		sort.Strings(roles)

		for _, r := range roles {
			fmt.Fprintln(&b, r)
		}

		return b.String()
	}

	var b strings.Builder

	for _, i := range issues {
		fmt.Fprintln(&b, i.String())
	}

	return b.String()
}

// TestParseGolden walks every fixture in testdata/*.yaml against its
// testdata/*.golden — the corpus this package's Parse doc comment promises:
// valid, bad YAML, schema-invalid, a hard-denied tool, an unresolved
// prompt, and a D15 model-family violation.
func TestParseGolden(t *testing.T) {
	fixtures, err := filepath.Glob("testdata/*.yaml")
	if err != nil {
		t.Fatalf("globbing testdata: %v", err)
	}

	if len(fixtures) == 0 {
		t.Fatal("no fixtures found in testdata/ — golden corpus is empty")
	}

	for _, path := range fixtures {
		name := strings.TrimSuffix(filepath.Base(path), ".yaml")

		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(path) //nolint:gosec // fixture paths come from filepath.Glob against a fixed testdata dir
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}

			m, issues := manifest.Parse(src)
			got := render(m, issues)

			goldenPath := filepath.Join("testdata", name+".golden")

			if *update {
				if err := os.WriteFile(goldenPath, []byte(got), 0o644); err != nil { //nolint:gosec // fixed testdata dir
					t.Fatalf("writing golden file: %v", err)
				}

				return
			}

			want, err := os.ReadFile(goldenPath) //nolint:gosec // fixed testdata dir
			if err != nil {
				t.Fatalf("reading golden file (run with -update to create it): %v", err)
			}

			if got != string(want) {
				t.Fatalf("%s: Parse output changed — review the diff, then re-run with -update if intended.\n--- got ---\n%s--- want ---\n%s", name, got, want)
			}
		})
	}
}
