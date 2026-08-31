// Package tasksmd parses and validates tasks.md — the entire interface
// between planning and execution (development-plan.md §1's handoff
// contract, §7 M2: "tasks.md ingestion with schema validation"). Format:
// Markdown carrying exactly one fenced ```yaml agentfleet-tasks block;
// everything else in the file is prose for humans and is ignored. A fenced
// typed block is the smallest thing that is both human-readable (Spec Kit
// emits Markdown) and machine-validatable, and it gives error messages a
// real line number in the file the human actually edited.
package tasksmd

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"gopkg.in/yaml.v3"
)

//go:embed schema/tasks.v1.schema.json
var schemaJSON []byte

// schemaURL is this schema's permanent identity — matches the embedded
// file's own "$id". docs/schemas/README.md notes that promoting this to a
// standalone agentfleet-presets repo (development-plan.md §1: "schema
// pinned by an org preset") is out of M2 scope; because the $id is already
// the eventual URL, that move is a relocation, not a rewrite.
const schemaURL = "https://agentfleet.dev/schemas/tasks/v1"

var compiledSchema = mustCompileSchema()

func mustCompileSchema() *jsonschema.Schema {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schemaJSON))
	if err != nil {
		panic(fmt.Sprintf("tasksmd: embedded schema is not valid JSON: %v", err))
	}

	c := jsonschema.NewCompiler()
	if err := c.AddResource(schemaURL, doc); err != nil {
		panic(fmt.Sprintf("tasksmd: embedded schema failed to register: %v", err))
	}

	return c.MustCompile(schemaURL)
}

// SchemaVersion is the "version" field every tasks.md must declare.
func SchemaVersion() string { return "v1" }

// Issue is one problem found in a tasks.md, with enough context that a
// human fixing it needs one round trip, not five — Parse always returns
// EVERY issue it finds, never just the first.
type Issue struct {
	// Line in the ORIGINAL tasks.md (not the extracted block) — 1-based,
	// 0 if the problem isn't line-attributable (e.g. no fenced block at all).
	Line int
	// Path is a JSON-Pointer-shaped location, e.g. "/tasks/2/acceptance_criteria".
	Path    string
	Message string
}

func (i Issue) String() string {
	if i.Line > 0 {
		return fmt.Sprintf("%d: %s: %s", i.Line, i.Path, i.Message)
	}

	return i.Message
}

// SpecRef is a hash-pinned reference into a spec artifact (development-plan.md
// D8: "context passes by hash-pinned reference, never by transcript").
type SpecRef struct {
	Path   string `json:"path"`
	Anchor string `json:"anchor,omitempty"`
	SHA256 string `json:"sha256"`
}

// Task is one parsed, schema-valid task. DependsOn holds external_refs as
// written in the file — resolving them to task uuids is internal/api's job
// at ingest time (P4's tasks:ingest handler), once it knows which
// external_refs already have rows in this feature.
type Task struct {
	ExternalRef        string
	Lane               string
	Title              string
	Intent             string
	AcceptanceCriteria []string
	Touches            []string
	DependsOn          []string
	SpecRefs           []SpecRef
}

// Doc is a fully parsed, schema-valid, cross-field-valid tasks.md.
type Doc struct {
	Version string
	Tasks   []Task
}

const (
	fenceOpenPrefix = "```yaml agentfleet-tasks"
	fenceClose      = "```"
)

// Parse extracts the fenced block, converts YAML to the schema's JSON
// shape, validates against the embedded schema, and runs the cross-field
// checks a JSON Schema cannot express (depends_on resolvability within this
// document, no dependency cycles, unique external_ref). It returns EVERY
// issue found, never just the first. A non-empty Issue slice means Doc is
// nil — callers (internal/api's tasks:ingest handler) must write nothing on
// any issue.
func Parse(src []byte) (*Doc, []Issue) {
	_, node, blockStartLine, found, parseErr := extractFencedBlock(src)
	if !found {
		return nil, []Issue{{Message: "no ```yaml agentfleet-tasks block found"}}
	}

	if parseErr != nil {
		return nil, []Issue{{Line: blockStartLine, Message: "invalid YAML: " + parseErr.Error()}}
	}

	raw, err := nodeToAny(node)
	if err != nil {
		return nil, []Issue{{Line: blockStartLine, Message: "invalid YAML: " + err.Error()}}
	}

	if valErr := compiledSchema.Validate(raw); valErr != nil {
		return nil, schemaIssues(valErr, node, blockStartLine)
	}

	// Schema-valid: re-marshal through JSON and decode into the typed Doc.
	// This is simpler and just as correct as walking `raw` by hand — the
	// schema already guarantees the shape.
	jsonBytes, err := json.Marshal(raw)
	if err != nil {
		return nil, []Issue{{Line: blockStartLine, Message: "internal error re-encoding parsed YAML: " + err.Error()}}
	}

	var typed struct {
		Version string `json:"version"`
		Tasks   []struct {
			ExternalRef        string    `json:"external_ref"`
			Lane               string    `json:"lane"`
			Title              string    `json:"title"`
			Intent             string    `json:"intent"`
			AcceptanceCriteria []string  `json:"acceptance_criteria"`
			Touches            []string  `json:"touches"`
			DependsOn          []string  `json:"depends_on"`
			SpecRefs           []SpecRef `json:"spec_refs"`
		} `json:"tasks"`
	}

	if err := json.Unmarshal(jsonBytes, &typed); err != nil {
		return nil, []Issue{{Line: blockStartLine, Message: "internal error decoding parsed YAML: " + err.Error()}}
	}

	doc := &Doc{Version: typed.Version}
	for _, t := range typed.Tasks {
		doc.Tasks = append(doc.Tasks, Task{
			ExternalRef:        t.ExternalRef,
			Lane:               t.Lane,
			Title:              t.Title,
			Intent:             t.Intent,
			AcceptanceCriteria: t.AcceptanceCriteria,
			Touches:            t.Touches,
			DependsOn:          t.DependsOn,
			SpecRefs:           t.SpecRefs,
		})
	}

	if issues := crossFieldIssues(doc, node, blockStartLine); len(issues) > 0 {
		return nil, issues
	}

	return doc, nil
}

// yamlSyntaxError exists only so extractFencedBlock's caller shape reads
// cleanly; the wrapped field is never read, only yaml.Unmarshal's returned
// error matters.
type yamlSyntaxError struct{ wrapped any }

// extractFencedBlock finds the first ```yaml agentfleet-tasks ... ``` block,
// returns its raw bytes, a parsed *yaml.Node (nil if the YAML itself doesn't
// parse — callers re-derive the error message in that case), and the
// 1-based line number in the ORIGINAL file where the block's content
// starts (the line after the opening fence) — every issue inside the block
// is reported relative to this offset.
func extractFencedBlock(src []byte) (block []byte, node *yaml.Node, startLine int, found bool, parseErr error) {
	lines := strings.Split(string(src), "\n")

	openIdx := -1

	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), fenceOpenPrefix) {
			openIdx = i

			break
		}
	}

	if openIdx == -1 {
		return nil, nil, 0, false, nil
	}

	closeIdx := -1

	for i := openIdx + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == fenceClose {
			closeIdx = i

			break
		}
	}

	if closeIdx == -1 {
		// Unterminated fence — treat the rest of the file as the block so
		// the YAML parser produces a useful error rather than silently
		// truncating.
		closeIdx = len(lines)
	}

	blockLines := lines[openIdx+1 : closeIdx]
	blockContent := strings.Join(blockLines, "\n")

	var root yaml.Node
	if err := yaml.Unmarshal([]byte(blockContent), &root); err != nil {
		return []byte(blockContent), nil, openIdx + 2, true, err
	}

	return []byte(blockContent), &root, openIdx + 2, true, nil
}

// nodeToAny decodes a *yaml.Node into a plain `any` shaped the way
// encoding/json would produce it (map[string]any, []any, string, bool,
// nil — no numbers appear in this schema, so int-vs-float64 never matters
// here), which is what jsonschema.Schema.Validate expects.
func nodeToAny(node *yaml.Node) (any, error) {
	if node == nil {
		return nil, nil
	}

	var v any
	if err := node.Decode(&v); err != nil {
		return nil, err
	}

	return v, nil
}

// schemaIssues converts a jsonschema.ValidationError tree (one root cause
// per top-level failure, with nested Causes) into a flat Issue list, one
// per LEAF cause — a leaf is where the actual problem is; the intermediate
// "value does not match schema" wrapper nodes add no information a human
// needs.
func schemaIssues(err error, node *yaml.Node, blockStartLine int) []Issue {
	valErr, ok := err.(*jsonschema.ValidationError)
	if !ok {
		return []Issue{{Line: blockStartLine, Message: err.Error()}}
	}

	var issues []Issue

	collectLeafIssues(valErr, node, blockStartLine, &issues)

	if len(issues) == 0 {
		// Defensive: every ValidationError should have at least one leaf,
		// but never return a validation failure as "no issues" (which
		// Parse's contract would misread as success).
		issues = append(issues, Issue{Line: blockStartLine, Message: valErr.Error()})
	}

	return issues
}

func collectLeafIssues(e *jsonschema.ValidationError, node *yaml.Node, blockStartLine int, out *[]Issue) {
	if len(e.Causes) == 0 {
		path := "/" + strings.Join(e.InstanceLocation, "/")
		line := blockStartLine

		if node != nil {
			if n := lookupNode(node, e.InstanceLocation); n != nil {
				line = blockStartLine + n.Line - 1
			}
		}

		*out = append(*out, Issue{Line: line, Path: path, Message: leafMessage(e)})

		return
	}

	for _, c := range e.Causes {
		collectLeafIssues(c, node, blockStartLine, out)
	}
}

// leafMessage renders a ValidationError's own kind without also repeating
// its full nested-cause dump (ValidationError.Error() includes the whole
// subtree, which is unreadable as a single flat Issue line).
// englishPrinter is required by ErrorKind.LocalizedString — verified live
// that passing nil panics inside golang.org/x/text/message (it calls
// Sprintf on the nil receiver unconditionally, not a documented "nil means
// default" contract). This package has no localization story of its own;
// English is the fixed error language everywhere.
var englishPrinter = message.NewPrinter(language.English)

func leafMessage(e *jsonschema.ValidationError) string {
	if e.ErrorKind != nil {
		return e.ErrorKind.LocalizedString(englishPrinter)
	}

	return "validation failed"
}

// lookupNode walks a decoded YAML mapping/sequence root following path
// segments (as produced by jsonschema's InstanceLocation) to the *yaml.Node*
// carrying line information for that exact location — this is what makes
// Issue.Line point at the actual offending field, not just the block start.
func lookupNode(root *yaml.Node, path []string) *yaml.Node {
	n := root
	if n.Kind == yaml.DocumentNode && len(n.Content) > 0 {
		n = n.Content[0]
	}

	for _, seg := range path {
		switch n.Kind {
		case yaml.MappingNode:
			found := false

			for i := 0; i+1 < len(n.Content); i += 2 {
				if n.Content[i].Value == seg {
					n = n.Content[i+1]
					found = true

					break
				}
			}

			if !found {
				return nil
			}
		case yaml.SequenceNode:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(n.Content) {
				return nil
			}

			n = n.Content[idx]
		default:
			return nil
		}
	}

	return n
}

// crossFieldIssues implements the checks a JSON Schema cannot express:
// external_ref uniqueness within the document, depends_on resolvability
// against other tasks in THIS document, and dependency-cycle detection.
func crossFieldIssues(doc *Doc, node *yaml.Node, blockStartLine int) []Issue {
	var issues []Issue

	seen := make(map[string]int, len(doc.Tasks)) // external_ref -> task index

	for i, t := range doc.Tasks {
		if prev, dup := seen[t.ExternalRef]; dup {
			issues = append(issues, Issue{
				Line:    lineForTaskField(node, blockStartLine, i, "external_ref"),
				Path:    fmt.Sprintf("/tasks/%d/external_ref", i),
				Message: fmt.Sprintf("duplicate external_ref %q (first used by task %d)", t.ExternalRef, prev),
			})

			continue
		}

		seen[t.ExternalRef] = i
	}

	for i, t := range doc.Tasks {
		for _, dep := range t.DependsOn {
			if _, ok := seen[dep]; !ok {
				issues = append(issues, Issue{
					Line:    lineForTaskField(node, blockStartLine, i, "depends_on"),
					Path:    fmt.Sprintf("/tasks/%d/depends_on", i),
					Message: fmt.Sprintf("depends_on references external_ref %q, which is not defined in this document", dep),
				})
			}
		}
	}

	if len(issues) > 0 {
		// A cycle check over unresolvable references would itself be
		// meaningless — report the resolvability problem first and let the
		// human fix it before spending a second round trip on cycles.
		return issues
	}

	if cyclePath := findCycle(doc); cyclePath != "" {
		issues = append(issues, Issue{
			Message: "dependency cycle: " + cyclePath,
		})
	}

	return issues
}

func lineForTaskField(node *yaml.Node, blockStartLine, taskIndex int, field string) int {
	if node == nil {
		return blockStartLine
	}

	n := lookupNode(node, []string{"tasks", strconv.Itoa(taskIndex), field})
	if n == nil {
		n = lookupNode(node, []string{"tasks", strconv.Itoa(taskIndex)})
	}

	if n == nil {
		return blockStartLine
	}

	return blockStartLine + n.Line - 1
}

// findCycle does a plain DFS over the depends_on graph (already verified
// fully resolvable by the caller) and returns a human-readable cycle path,
// or "" if the graph is acyclic.
func findCycle(doc *Doc) string {
	byRef := make(map[string]Task, len(doc.Tasks))
	for _, t := range doc.Tasks {
		byRef[t.ExternalRef] = t
	}

	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)

	state := make(map[string]int, len(doc.Tasks))

	var path []string

	var visit func(ref string) string

	visit = func(ref string) string {
		switch state[ref] {
		case done:
			return ""
		case visiting:
			// Found the cycle: report from where `ref` first appears in
			// the current path.
			for i, p := range path {
				if p == ref {
					return strings.Join(path[i:], " -> ") + " -> " + ref
				}
			}

			return ref
		}

		state[ref] = visiting

		path = append(path, ref)

		for _, dep := range byRef[ref].DependsOn {
			if c := visit(dep); c != "" {
				return c
			}
		}

		path = path[:len(path)-1]
		state[ref] = done

		return ""
	}

	for _, t := range doc.Tasks {
		if state[t.ExternalRef] == unvisited {
			if c := visit(t.ExternalRef); c != "" {
				return c
			}
		}
	}

	return ""
}
