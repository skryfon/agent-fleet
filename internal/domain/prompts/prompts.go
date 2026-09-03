// Package prompts is AgentFleet's versioned role-prompt library
// (development-plan.md §7 M6: "versioned prompt library"). Each file is one
// immutable version — implementer@v1.md, never edited once shipped. A
// prompt change is a NEW file (implementer@v2.md); editing an existing one
// would silently invalidate every run.prompt_version audit row that names
// it, and internal/domain/manifest.Parse resolves a manifest's `prompt:`
// field against Names() at registration time specifically so that promise
// holds before a run ever starts.
//
// ponytail: prepended to the launched TASK string, not a dsh-system-prompt
// row patch — that row belongs to dsh-base and D14 (docs/adr/0014) gates
// patching it from outside our own bundle. Revisit if a real system-prompt
// extension seam ever appears in runner/bundle/cordis.patch.yml.
package prompts

import (
	"embed"
	"fmt"
	"sort"
	"strings"
)

//go:embed *.md
var files embed.FS

// Get returns the named prompt's full text (e.g. "implementer@v1"). The
// name is the embedded filename without its .md extension.
func Get(name string) (string, error) {
	b, err := files.ReadFile(name + ".md")
	if err != nil {
		return "", fmt.Errorf("prompts: unknown prompt %q: %w", name, err)
	}

	return string(b), nil
}

// Names lists every prompt version available, sorted — what
// internal/domain/manifest.Parse validates a manifest's `prompt:` fields
// against.
func Names() []string {
	entries, err := files.ReadDir(".")
	if err != nil {
		// files is compiled in via go:embed; a read failure here is a build
		// bug, not a runtime condition callers can meaningfully handle.
		panic("prompts: embedded FS unreadable: " + err.Error())
	}

	names := make([]string, 0, len(entries))

	for _, e := range entries {
		names = append(names, strings.TrimSuffix(e.Name(), ".md"))
	}

	sort.Strings(names)

	return names
}
