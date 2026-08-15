package memory

import (
	"errors"
	"fmt"
	"strings"
)

// ErrClosed is returned by every method once Close has been called.
var ErrClosed = errors.New("memory: client is closed")

// ErrInvalidArgument is returned when a call is rejected before it reaches lore,
// because the arguments cannot produce a well-formed lore tool call. It is a
// programming error, not a user-facing condition.
var ErrInvalidArgument = errors.New("memory: invalid argument")

// ErrUnknownSpace is returned when lore does not hold the space that was named.
// A kenward deployment reaching this has a configuration fault: the space id in
// the household configuration does not exist in this lore home.
var ErrUnknownSpace = errors.New("memory: unknown lore space")

// ErrUserModel is returned when a share is refused because the entry lives in
// lore's user-model domains (profile/, feedback/), which never leave the personal
// space on any path.
var ErrUserModel = errors.New("memory: user-model entry cannot leave its space")

// ErrNotWriter is returned when this lore account holds only the reader role in
// the target space and so may not author into it.
var ErrNotWriter = errors.New("memory: not a writer of the target space")

// ErrWriteUncertain marks a write whose outcome is unknown: the request reached
// lore but no answer came back, so the entry may or may not have been stored.
//
// It exists because lore has no delete. A duplicate entry is permanent, so a
// caller must be able to tell "this may not have saved" from "this did not save"
// and say so, rather than inviting a retry that stores the learning twice. Every
// other write failure — a rejection from lore, store contention that exhausted
// its retries, an argument this client refused — means nothing was written.
//
// Use errors.Is to test for it; the underlying cause is wrapped alongside.
var ErrWriteUncertain = errors.New("memory: write may or may not have been stored")

// ErrBusy is returned when lore's SQLite store was locked by another process for
// longer than the bounded retry window. lore opens the database with WAL, a
// single connection and a five second busy timeout, so a concurrent writer —
// typically a `lore serve` sync round — can hold a call off.
var ErrBusy = errors.New("memory: lore store is busy")

// ToolError is a failure lore reported inside a successful MCP response.
//
// The lore MCP server never returns JSON-RPC protocol errors: every failure
// arrives as a tool result with isError set and a human-readable message. Text is
// that message verbatim. Where the message is recognised, Unwrap yields one of
// this package's sentinels — including ErrNotFound — so callers can branch with
// errors.Is without matching on prose.
type ToolError struct {
	// Tool is the lore tool that failed, for example "lore_put".
	Tool string
	// Text is lore's message, verbatim.
	Text string

	sentinel error
}

// Error implements the error interface.
func (e *ToolError) Error() string {
	return fmt.Sprintf("memory: %s: %s", e.Tool, e.Text)
}

// Unwrap returns the sentinel this message was recognised as, or nil when lore
// reported something this client does not have a typed error for.
func (e *ToolError) Unwrap() error { return e.sentinel }

// ParseError reports that lore's output text could not be understood.
//
// This client parses lore's unstructured, human-readable tool output, so a change
// to lore's output format shows up here rather than as an Entry with silently
// empty fields. Reason names the specific expectation that failed and Snippet
// carries the offending text, so a format change is diagnosable from the error
// alone.
type ParseError struct {
	// Tool is the lore tool whose output could not be parsed.
	Tool string
	// Reason names the expectation that failed.
	Reason string
	// Line is the 1-based line number the failure was found on, or 0 when the
	// failure concerns the output as a whole.
	Line int
	// Snippet is the offending text, truncated.
	Snippet string
}

// Error implements the error interface.
func (e *ParseError) Error() string {
	loc := ""
	if e.Line > 0 {
		loc = fmt.Sprintf(" at line %d", e.Line)
	}
	return fmt.Sprintf("memory: cannot parse %s output%s: %s: %q", e.Tool, loc, e.Reason, e.Snippet)
}

// parseErrf builds a ParseError, truncating the snippet to a length that is safe
// to put in a log line.
func parseErrf(tool string, line int, snippet string, format string, a ...any) *ParseError {
	const maxSnippet = 300
	if len(snippet) > maxSnippet {
		snippet = snippet[:maxSnippet] + "…"
	}
	return &ParseError{Tool: tool, Reason: fmt.Sprintf(format, a...), Line: line, Snippet: snippet}
}

// toolError classifies one of lore's isError messages.
//
// The mapping is deliberately conservative: an unrecognised message yields a
// ToolError with no sentinel rather than a guess, so a new lore error text
// surfaces as an opaque failure instead of being mistaken for a known one.
func toolError(tool, text string) error {
	return &ToolError{Tool: tool, Text: text, sentinel: classify(text)}
}

// classify maps lore's error prose onto a sentinel. The matched substrings are
// the literal format strings in lore's internal/mcpserver/tools.go and
// internal/store.
func classify(text string) error {
	lower := strings.ToLower(text)
	switch {
	case strings.Contains(lower, "no entry with id"):
		return ErrNotFound
	case strings.Contains(lower, "unknown space "):
		return ErrUnknownSpace
	case strings.Contains(lower, "user-model entry") ||
		strings.Contains(lower, "user-model entries"):
		return ErrUserModel
	case strings.Contains(lower, "is not a writer/owner of the space"):
		return ErrNotWriter
	case isBusy(lower):
		return ErrBusy
	case strings.Contains(lower, "is required") ||
		strings.Contains(lower, "are required") ||
		strings.Contains(lower, "pass exactly one of") ||
		strings.Contains(lower, "invalid confidence") ||
		strings.Contains(lower, "invalid origin"):
		return ErrInvalidArgument
	}
	return nil
}

// isBusy recognises the SQLite contention messages that modernc.org/sqlite
// surfaces through lore. lore holds one connection per handle with a five second
// busy timeout, so these are transient and worth a bounded retry.
func isBusy(lower string) bool {
	return strings.Contains(lower, "database is locked") ||
		strings.Contains(lower, "database table is locked") ||
		strings.Contains(lower, "sqlite_busy")
}
