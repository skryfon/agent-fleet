package tasksmd_test

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agentfleet/internal/domain/tasksmd"
)

var update = flag.Bool("update", false, "update golden files")

// render turns a Parse result into a comparable golden string. A successful
// parse (nil issues) renders as "OK" plus one line per task's external_ref,
// in document order — enough to prove the typed Doc came out right without
// pinning the entire struct dump. A failed parse renders one Issue.String()
// per line, in the order Parse returned them (order IS part of the
// contract: Parse must report every issue found, not just the first).
func render(doc *tasksmd.Doc, issues []tasksmd.Issue) string {
	if len(issues) == 0 {
		var b strings.Builder

		fmt.Fprintln(&b, "OK")

		for _, t := range doc.Tasks {
			fmt.Fprintln(&b, t.ExternalRef)
		}

		return b.String()
	}

	var b strings.Builder

	for _, i := range issues {
		fmt.Fprintln(&b, i.String())
	}

	return b.String()
}

// TestParseGolden walks every fixture in testdata/*.md against its
// testdata/*.golden — the corpus this package's own doc comment on Parse
// promises: valid, missing-block, bad-yaml, schema-invalid, dependency
// cycle, duplicate external_ref, each with a golden Issue list.
func TestParseGolden(t *testing.T) {
	fixtures, err := filepath.Glob("testdata/*.md")
	if err != nil {
		t.Fatalf("globbing testdata: %v", err)
	}

	if len(fixtures) == 0 {
		t.Fatal("no fixtures found in testdata/ — golden corpus is empty")
	}

	for _, path := range fixtures {
		name := strings.TrimSuffix(filepath.Base(path), ".md")

		t.Run(name, func(t *testing.T) {
			src, err := os.ReadFile(path) //nolint:gosec // fixture paths come from filepath.Glob against a fixed testdata dir, not user input
			if err != nil {
				t.Fatalf("reading %s: %v", path, err)
			}

			doc, issues := tasksmd.Parse(src)
			got := render(doc, issues)

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

			// The nil-Doc-on-issues contract: internal/api's tasks:ingest
			// handler relies on this to know it's safe to write nothing.
			if len(issues) > 0 && doc != nil {
				t.Errorf("%s: Parse returned issues AND a non-nil Doc — callers must never see a partial doc", name)
			}
		})
	}
}
