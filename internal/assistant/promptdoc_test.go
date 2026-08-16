package assistant

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// TestToolSpecsMatchPromptDoc holds remember.go's claim that the two tool schemas
// are "docs/PROMPT.md's, verbatim" to being actually true.
//
// The schemas are the contract between the document a human edits when they change
// what the model may do and the constants the model is actually sent. Nothing else
// ties the two together: they are separate literals in separate files, and the only
// thing that has kept them equal so far is someone remembering to edit both. That is
// exactly the arrangement that drifts, and a drifted schema is invisible — the model
// is simply sent something other than what the document says it was sent.
//
// This test does not skip. A doc that cannot be read is a failure, not an absence:
// the whole value of a documentation guard is that it fires when nobody is looking,
// and a guard that quietly stands down when it cannot find its subject is
// indistinguishable from no guard at all.
//
// The comparison is semantic — both sides are parsed as JSON and compared as
// structures — so reindenting either file is free, while a changed required field,
// property name, enum value or description fails.
func TestToolSpecsMatchPromptDoc(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "PROMPT.md")
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("docs/PROMPT.md must be readable: remember.go claims its schemas come from it, and that claim cannot be checked without it: %v", err)
	}

	want := promptDocTools(t, string(doc))

	// The code's side of the contract: the schema constants, plus the names and
	// descriptions toolSpecs attaches to them. A direct scope is used because it is
	// the only one offered both tools.
	got := map[string]any{}
	for _, spec := range toolSpecs(domain.Scope{Kind: domain.ScopeDirect}) {
		got[spec.Name] = decodeJSON(t, "tool spec "+spec.Name, mustMarshal(t, map[string]any{
			"name":         spec.Name,
			"description":  spec.Description,
			"input_schema": json.RawMessage(spec.Schema),
		}))
	}

	// Every tool kenward offers, named explicitly. The reverse pass below catches a
	// tool documented but never offered; only this list catches the opposite, so a
	// new tool that is not added here has its doc-versus-code equality asserted by
	// nothing at all.
	for _, name := range []string{rememberToolName, publishToolName, remindToolName, unremindToolName} {
		w, ok := want[name]
		if !ok {
			t.Errorf("docs/PROMPT.md no longer documents a %q tool; remember.go still defines one", name)
			continue
		}
		g, ok := got[name]
		if !ok {
			t.Errorf("toolSpecs no longer offers a %q tool in a direct scope; docs/PROMPT.md still documents one", name)
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("the %q tool no longer matches docs/PROMPT.md.\n\n--- docs/PROMPT.md ---\n%s\n\n--- internal/assistant ---\n%s\n\nOne of the two was edited without the other. The document is the contract; fix whichever is wrong, deliberately.",
				name, indentJSON(t, w), indentJSON(t, g))
		}
	}
	for name := range want {
		if _, ok := got[name]; !ok {
			t.Errorf("docs/PROMPT.md documents a %q tool that internal/assistant never offers", name)
		}
	}
}

// TestPromptTextMatchesPromptDoc holds prompt.go's other claim — that its text
// constants are "copied from [docs/PROMPT.md] verbatim, placeholders included" — to
// being actually true.
//
// Until this test, only the two JSON schemas were checked. Everything the model is
// actually told in prose was two literals in two files kept equal by somebody
// remembering, which is the arrangement the test above exists because it drifts. The
// prose is the larger half: the schemas say what the tools take, and the paragraphs say
// what the assistant may claim to have done.
//
// A constant is required to appear verbatim inside a fenced block, not merely somewhere
// in the document. Prose about the prompt quotes fragments of it constantly; a match
// against the whole file would be satisfied by the commentary explaining a paragraph
// that had already been deleted from the block above it.
//
// Placeholders are compared unexpanded, because that is how both sides hold them.
// Whitespace is flattened on both sides before comparison, because rewrapping a
// paragraph at a different column changes no word the model reads and a guard that
// fired on it would be turned off within a month. A block may hold more than one
// constant — the reminder block holds the instructions, an example list and the cancel
// line — so a constant has to appear inside a block rather than to be one.
func TestPromptTextMatchesPromptDoc(t *testing.T) {
	path := filepath.Join("..", "..", "docs", "PROMPT.md")
	doc, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("docs/PROMPT.md must be readable: prompt.go claims its text comes from it, and that claim cannot be checked without it: %v", err)
	}
	blocks := promptDocBlocks(t, string(doc))

	// Every constant prompt.go renders. A new one that is not added here has its
	// doc-versus-code equality asserted by nothing at all — which is the hole this
	// test was written to close, so leaving a second one open would be a poor joke.
	for _, c := range []struct{ name, text string }{
		{"identityText", identityText},
		{"flatRegisterText", flatRegisterText},
		{"dateText", dateText},
		{"formattingText", formattingText},
		{"personaGuardText", personaGuardText},
		{"directDisclosureText", directDisclosureText},
		{"groupDisclosureText", groupDisclosureText},
		{"householdDisclosureText", householdDisclosureText},
		{"confidenceText", confidenceText},
		{"untrustedEntryNote", untrustedEntryNote},
		{"captureText", captureText},
		{"captureDirectText", captureDirectText},
		{"captureGroupText", captureGroupText},
		{"captureHouseholdText", captureHouseholdText},
		{"publishText", publishText},
		{"remindText", remindText},
		{"remindCancelText", remindCancelText},
	} {
		want := flattened(c.text)
		found := false
		for _, b := range blocks {
			if strings.Contains(b, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("%s is not in any fenced block of docs/PROMPT.md.\n\n--- internal/assistant ---\n%s\n\nOne of the two was edited without the other. The document is the contract; fix whichever is wrong, deliberately.",
				c.name, strings.TrimSpace(c.text))
		}
	}
}

// flattened reduces text to its words, so that a paragraph rewrapped at a different
// column still compares equal.
func flattened(s string) string { return strings.Join(strings.Fields(s), " ") }

// promptDocBlocks is every fenced block in the document that is not a ```json tool
// definition, flattened.
func promptDocBlocks(t *testing.T, doc string) []string {
	t.Helper()
	var blocks []string
	rest := doc
	for {
		open := strings.Index(rest, "\n```")
		if open < 0 {
			break
		}
		rest = rest[open+len("\n```"):]
		// The info string, if any: ```json blocks are the tool definitions and are
		// the other test's business.
		lang, body, ok := strings.Cut(rest, "\n")
		if !ok {
			t.Fatalf("docs/PROMPT.md has an unclosed fence")
		}
		end := strings.Index(body, "\n```")
		if end < 0 {
			t.Fatalf("docs/PROMPT.md has an unclosed ``` fence after a %q info string", lang)
		}
		if strings.TrimSpace(lang) == "" {
			blocks = append(blocks, flattened(body[:end]))
		}
		rest = body[end+len("\n```"):]
	}
	if len(blocks) == 0 {
		t.Fatalf("docs/PROMPT.md contains no unlabelled fenced blocks; prompt.go's text has nothing to be verbatim from")
	}
	return blocks
}

// promptDocTools parses every ```json fence in PROMPT.md and keeps the ones that
// look like a tool definition, keyed by name. Selecting by shape rather than by
// position means reordering the document, or adding a JSON example beside them,
// does not break the guard.
func promptDocTools(t *testing.T, doc string) map[string]any {
	t.Helper()
	tools := map[string]any{}
	rest := doc
	for {
		open := strings.Index(rest, "```json")
		if open < 0 {
			break
		}
		rest = rest[open+len("```json"):]
		end := strings.Index(rest, "```")
		if end < 0 {
			t.Fatalf("docs/PROMPT.md has an unclosed ```json fence")
		}
		block := rest[:end]
		rest = rest[end+3:]

		var probe map[string]json.RawMessage
		if json.Unmarshal([]byte(block), &probe) != nil {
			continue // not an object; not a tool definition
		}
		if _, ok := probe["input_schema"]; !ok {
			continue
		}
		var name string
		if json.Unmarshal(probe["name"], &name) != nil || name == "" {
			t.Fatalf("docs/PROMPT.md has a tool block with no usable \"name\":\n%s", block)
		}
		if _, dup := tools[name]; dup {
			t.Fatalf("docs/PROMPT.md defines the %q tool twice; there is no way to tell which one is the contract", name)
		}
		tools[name] = decodeJSON(t, "docs/PROMPT.md block for "+name, []byte(block))
	}
	if len(tools) == 0 {
		t.Fatalf("docs/PROMPT.md contains no ```json tool blocks; remember.go's schemas have nothing to be verbatim from")
	}
	return tools
}

func decodeJSON(t *testing.T, what string, b []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatalf("%s is not valid JSON: %v\n%s", what, err, b)
	}
	return v
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling the tool spec: %v", err)
	}
	return b
}

func indentJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatalf("rendering the mismatch: %v", err)
	}
	return string(b)
}
