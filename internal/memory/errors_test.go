package memory

import (
	"errors"
	"strings"
	"testing"
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
		if !strings.Contains(err.Error(), message) {
			t.Errorf("%q: the error string must contain lore's message, got %q", message, err.Error())
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
