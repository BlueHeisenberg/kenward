package memory

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/BlueHeisenberg/kenward/internal/domain"
)

// TestClassifyGolden checks lore's isError prose against the sentinels kenward
// branches on. The corpus is one fixture, so a lore wording change is one edit
// and one obvious failure.
func TestClassifyGolden(t *testing.T) {
	sentinels := map[string]error{
		"none":          nil,
		"not-found":     ErrNotFound,
		"unknown-space": ErrUnknownSpace,
		"user-model":    ErrUserModel,
		"not-writer":    ErrNotWriter,
		"busy":          ErrBusy,
		"invalid":       ErrInvalidArgument,
		// An operational failure of lore's own store is typed too, so it can be
		// reported as one instead of as an unrecognised rejection.
		"store-unavailable": ErrStoreUnavailable,
		"canceled":          context.Canceled,
	}

	lines := strings.Split(golden(t, "errors.txt"), "\n")
	cases := 0
	for i, line := range lines {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		want, message, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("fixture line %d is not <sentinel>TAB<message>: %q", i+1, line)
		}
		sentinel, known := sentinels[want]
		if !known {
			t.Fatalf("fixture line %d names an unknown sentinel %q", i+1, want)
		}
		cases++

		err := toolError(toolPut, message)
		var te *ToolError
		if !errors.As(err, &te) {
			t.Fatalf("%q: want a *ToolError, got %T", message, err)
		}
		if te.Text != message {
			t.Errorf("%q: lore's message must be carried verbatim, got %q", message, te.Text)
		}
		// lore's own wording is unlisted, not lost. It stays off Error(), which is
		// what reaches a log by default, and is reachable by name — several of the
		// messages in this corpus quote a value back (a space name, a domain, a
		// confidence), and a domain comes from the draft the model wrote.
		if strings.Contains(err.Error(), message) {
			t.Errorf("%q: the error string must not contain lore's message, got %q", message, err.Error())
		}
		if te.Detail() != message {
			t.Errorf("%q: Detail() must carry lore's message verbatim, got %q", message, te.Detail())
		}
		if !strings.Contains(err.Error(), toolPut) {
			t.Errorf("%q: the error string must name the tool, got %q", message, err.Error())
		}
		if sentinel == nil {
			if errors.Unwrap(err) != nil {
				t.Errorf("%q: want no sentinel, got %v", message, errors.Unwrap(err))
			}
			continue
		}
		if !errors.Is(err, sentinel) {
			t.Errorf("%q: want %v, got %v", message, sentinel, errors.Unwrap(err))
		}
	}
	if cases == 0 {
		t.Fatal("the error corpus is empty")
	}
}

// TestNotFoundIsTheInterfaceSentinel pins the mapping the Memory interface
// promises: a missing entry is memory.ErrNotFound, not a lore-specific error.
func TestNotFoundIsTheInterfaceSentinel(t *testing.T) {
	err := toolError(toolGet, `no entry with id "nope"`)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestParseErrorMessage(t *testing.T) {
	pe := parseErrf(toolSearch, 3, strings.Repeat("x", 500), "expected %q", "a header")
	msg := pe.Error()
	for _, want := range []string{toolSearch, "line 3", "expected"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	if len(pe.Snippet) > 320 {
		t.Errorf("snippet not truncated: %d bytes", len(pe.Snippet))
	}
}

// loreSecret is the sort of thing lore's rendered output is made of: a member's
// entry title and body. Errors are checked against it verbatim.
const loreSecret = "Miriam's biopsy is on the 12th"

// TestParseErrorDoesNotRenderMemoryContent proves that a lore format change turns
// into an error naming the tool, the line and the expectation — and never into a
// log line quoting the household's memory. lore renders entry titles and bodies,
// so the snippet a parse failure captures is memory content; in isolated mode the
// error carrying it would be aggregated on a host the member's pod exists to keep
// their memory away from.
func TestParseErrorDoesNotRenderMemoryContent(t *testing.T) {
	pe := parseErrf(toolGet, 4, "# "+loreSecret, "expected a %q title line", "# ")

	msg := pe.Error()
	if strings.Contains(msg, loreSecret) {
		t.Errorf("the error string quotes memory content: %s", msg)
	}
	// Diagnosis is not weakened: what failed, where, and what was expected are
	// all still in the string an operator reads first.
	for _, want := range []string{toolGet, "line 4", "title line"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message %q lacks %q", msg, want)
		}
	}
	// The text is unlisted, not lost: someone debugging a format change asks for
	// it by name, at which point disclosing it is a decision.
	if !strings.Contains(pe.Detail(), loreSecret) {
		t.Errorf("Detail() should carry the offending text, got %q", pe.Detail())
	}
	if pe.Snippet != pe.Detail() {
		t.Errorf("Detail() must be the snippet, got %q vs %q", pe.Detail(), pe.Snippet)
	}
}

// TestParseErrorReasonNeverInterpolatesLoreOutput guards the invariant the type
// documents: Reason is built from this package's own format strings. A snippet
// smuggled into the reason would defeat the whole scheme silently, because the
// reason *is* rendered by Error.
func TestParseErrorReasonNeverInterpolatesLoreOutput(t *testing.T) {
	bad := "# " + loreSecret + "\nid whatever | space x"
	fixtures := []struct {
		name  string
		parse func(string) error
	}{
		{"search", func(s string) error { _, err := parseSearch(s); return err }},
		{"get", func(s string) error { _, err := parseEntry(s); return err }},
		{"put", func(s string) error { _, err := parseStored(s); return err }},
		{"share", func(s string) error { _, err := parseCopied(s); return err }},
		{"spaces", func(s string) error { _, err := parseSpaces(s); return err }},
	}
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			err := f.parse(bad)
			if err == nil {
				t.Fatal("malformed output parsed cleanly")
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("want a *ParseError, got %T: %v", err, err)
			}
			if strings.Contains(pe.Reason, loreSecret) {
				t.Errorf("Reason interpolates lore output: %q", pe.Reason)
			}
			if strings.Contains(err.Error(), loreSecret) {
				t.Errorf("the error string quotes memory content: %s", err)
			}
		})
	}
}

// TestSearchParseFailureDoesNotLeakThroughTheClient walks the whole path a
// degraded retrieval takes: lore answers with something unparseable that happens
// to contain a member's memory, and the error the client hands back — wrapped,
// as the caller sees it — says nothing about what was in it.
func TestSearchParseFailureDoesNotLeakThroughTheClient(t *testing.T) {
	// A well-formed header followed by a line lore's format no longer produces —
	// and that line is a member's entry title, because that is what lore renders.
	f := newFake(t, fakeScript{Replies: map[string][]fakeReply{
		toolSpaces: {{Text: golden(t, "spaces_list.txt")}},
		toolSearch: {{Text: "1 result(s) for boiler\n" + loreSecret + "\n"}},
	}}, nil)

	_, err := f.Search(ctxT(t), SearchQuery{Text: "boiler", Spaces: []domain.SpaceID{spacePrivate}})
	if err == nil {
		t.Fatal("want a parse error")
	}
	if strings.Contains(err.Error(), loreSecret) {
		t.Errorf("the wrapped error quotes memory content: %s", err)
	}
	var pe *ParseError
	if !errors.As(err, &pe) {
		t.Fatalf("want a *ParseError, got %T: %v", err, err)
	}
	if !strings.Contains(pe.Detail(), loreSecret) {
		t.Errorf("Detail() should still carry what lore actually sent, got %q", pe.Detail())
	}
}

// TestToolErrorDoesNotRenderLoresProse: lore chooses the wording of its own
// rejections and quotes values back in several of them, so its message stays off
// the string a caller logs by default and travels on Detail instead.
func TestToolErrorDoesNotRenderLoresProse(t *testing.T) {
	err := toolError(toolPut, `refused: "`+loreSecret+`" is a user-model entry (profile/, feedback/)`)
	if strings.Contains(err.Error(), loreSecret) {
		t.Errorf("the error string quotes lore's prose: %s", err)
	}
	// The classification survives, so a caller can still branch and an operator
	// can still see which of lore's refusals this was.
	if !errors.Is(err, ErrUserModel) {
		t.Errorf("want ErrUserModel, got %v", err)
	}
	if !strings.Contains(err.Error(), toolPut) {
		t.Errorf("the error should name the tool, got %s", err)
	}
	var te *ToolError
	if !errors.As(err, &te) || !strings.Contains(te.Detail(), loreSecret) {
		t.Errorf("Detail() should carry lore's message verbatim, got %v", err)
	}
}

// TestProcessErrorDoesNotRenderStderr: kenward does not control what lore prints
// on its standard error, so the tail is treated as lore's content and reaches an
// operator only through Detail. The stage and the cause stay in the string, which
// is what an operator needs to know a subprocess died at all.
func TestProcessErrorDoesNotRenderStderr(t *testing.T) {
	pe := &ProcessError{
		Stage:  toolGet + ": lore subprocess ended",
		Stderr: "lore: panic while rendering " + loreSecret + "\n",
		Err:    errors.New("EOF"),
	}
	if strings.Contains(pe.Error(), loreSecret) {
		t.Errorf("the error string quotes the subprocess stderr: %s", pe)
	}
	for _, want := range []string{toolGet, "subprocess ended", "EOF"} {
		if !strings.Contains(pe.Error(), want) {
			t.Errorf("message %q lacks %q", pe.Error(), want)
		}
	}
	if !strings.Contains(pe.Detail(), loreSecret) {
		t.Errorf("Detail() should carry the stderr tail, got %q", pe.Detail())
	}
	if !errors.Is(pe, pe.Err) {
		t.Error("the underlying cause must stay reachable through Unwrap")
	}
}

// TestDraftValuesAreNotNamedInArgumentErrors: a draft is written by the model out
// of a member's conversation, so every field of it is content — including the
// ones this client rejects before the call goes out.
func TestDraftValuesAreNotNamedInArgumentErrors(t *testing.T) {
	f := newFake(t, fakeScript{}, nil)

	_, err := f.Put(ctxT(t), spacePrivate, Draft{
		Domain: "d", Title: "t", Body: "b", Confidence: loreSecret,
	})
	if err == nil || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	if strings.Contains(err.Error(), loreSecret) {
		t.Errorf("the error names the rejected draft value: %s", err)
	}

	_, err = f.Put(ctxT(t), spacePrivate, Draft{
		Domain: "d", Title: "t", Body: "b", Markers: []string{"[OK]", loreSecret + ", and more"},
	})
	if err == nil || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("want ErrInvalidArgument, got %v", err)
	}
	if strings.Contains(err.Error(), loreSecret) {
		t.Errorf("the error names the rejected marker: %s", err)
	}
	if !strings.Contains(err.Error(), "marker 1") {
		t.Errorf("the error should say which marker failed, got %s", err)
	}
}
