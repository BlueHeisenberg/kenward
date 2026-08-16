package memory

import (
	"context"
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
// It exists because a write with no receipt cannot be undone. lore can delete,
// but only by id, and the id is in the receipt that never arrived — so a
// duplicate written by a blind retry is permanent. A caller must be able to tell
// "this may not have saved" from "this did not save" and say so. Every other
// write failure — a rejection from lore, store contention that exhausted its
// retries, an argument this client refused — means nothing was written.
//
// Delete uses it in the mirror sense: the tombstone may or may not have landed,
// so the entry may or may not still be there.
//
// Use errors.Is to test for it; the underlying cause is wrapped alongside.
var ErrWriteUncertain = errors.New("memory: write may or may not have been stored")

// ErrBusy is returned when lore's SQLite store was locked by another process for
// longer than the bounded retry window. lore opens the database with WAL, a
// single connection and a five second busy timeout, so a concurrent writer —
// typically a `lore serve` sync round — can hold a call off.
var ErrBusy = errors.New("memory: lore store is busy")

// ErrStoreUnavailable is returned when lore reports that its store is not usable
// at all — "store closed", which lore says when its home was never initialised or
// its store was shut down under it. Unlike ErrBusy it does not clear itself on a
// retry: it is an operator fault, and it is typed so that it can be reported as
// one rather than as an unrecognised rejection.
var ErrStoreUnavailable = errors.New("memory: lore store is unavailable")

// ToolError is a failure lore reported inside a successful MCP response.
//
// The lore MCP server never returns JSON-RPC protocol errors: every failure
// arrives as a tool result with isError set and a human-readable message. Text is
// that message verbatim. Where the message is recognised, Unwrap yields one of
// this package's sentinels — including ErrNotFound — so callers can branch with
// errors.Is without matching on prose.
//
// # What Error does not say
//
// Error renders the tool and the sentinel the message was classified as, never
// Text. lore chooses the wording of its own rejections and kenward does not
// constrain it, so Text is lore's content rather than kenward's diagnostics:
// nothing here can promise that a future lore does not quote the entry it
// refused back at the caller. The prose is unlisted, not lost — read
// [ToolError.Detail], and log the result only where a member's memory is
// allowed to go.
type ToolError struct {
	// Tool is the lore tool that failed, for example "lore_put".
	Tool string
	// Text is lore's message, verbatim. Treat it as lore's content: reach it
	// through Detail rather than rendering it into anything logged by default.
	Text string

	sentinel error
}

// Error implements the error interface. It names the tool and the classification
// only; see the type documentation for why, and [ToolError.Detail] for lore's own
// words.
func (e *ToolError) Error() string {
	if e.sentinel != nil {
		return fmt.Sprintf("memory: %s rejected the call: %s", e.Tool, unprefixed(e.sentinel))
	}
	return fmt.Sprintf("memory: %s rejected the call with a message this client does not recognise", e.Tool)
}

// Detail returns lore's message verbatim.
//
// It is a method rather than part of Error so that disclosing it is a decision.
func (e *ToolError) Detail() string { return e.Text }

// Unwrap returns the sentinel this message was recognised as, or nil when lore
// reported something this client does not have a typed error for.
func (e *ToolError) Unwrap() error { return e.sentinel }

// unprefixed renders a sentinel without this package's own "memory: " prefix, so
// that wrapping it does not repeat the prefix twice in one line.
func unprefixed(err error) string {
	return strings.TrimPrefix(err.Error(), "memory: ")
}

// ParseError reports that lore's output text could not be understood.
//
// This client parses lore's unstructured, human-readable tool output, so a change
// to lore's output format shows up here rather than as an Entry with silently
// empty fields. Reason names the specific expectation that failed, and Tool and
// Line say where, so a format change is diagnosable from the error alone.
//
// # What Error does not say
//
// Error renders the structural failure — which tool, which line, what was
// expected — and never Snippet, because Snippet is a fragment of lore's rendered
// output and lore renders a household's memory: entry titles and body lines. An
// error string is the part of a failure that reaches the operator's log by
// default, and in isolated mode that log crosses the boundary the mode exists to
// hold. The offending text is unlisted, not lost: [ParseError.Detail] returns it
// for whoever is deliberately debugging a lore format change.
type ParseError struct {
	// Tool is the lore tool whose output could not be parsed.
	Tool string
	// Reason names the expectation that failed. It is built from this package's
	// own format strings and carries no fragment of lore's output.
	Reason string
	// Line is the 1-based line number the failure was found on, or 0 when the
	// failure concerns the output as a whole.
	Line int
	// Snippet is the offending text, truncated. It is memory content: reach it
	// through Detail rather than rendering it into anything logged by default.
	Snippet string
}

// Error implements the error interface. It names the tool, the line and the
// expectation that failed; see the type documentation for why it omits the
// snippet, and [ParseError.Detail] for how to read it.
func (e *ParseError) Error() string {
	loc := ""
	if e.Line > 0 {
		loc = fmt.Sprintf(" at line %d", e.Line)
	}
	return fmt.Sprintf("memory: cannot parse %s output%s: %s", e.Tool, loc, e.Reason)
}

// Detail returns the offending text lore emitted, truncated as it was captured.
//
// It is a method rather than part of Error so that disclosing it is a decision.
// The text is whatever lore rendered, which for a search or a fetch is entry
// titles and body lines — a member's memory. Treat the result as content, not as
// diagnostics: log it where content is allowed to go, or not at all.
func (e *ParseError) Detail() string { return e.Snippet }

// parseErrf builds a ParseError. The snippet is truncated because it is retained
// for a human reading Detail, not because truncation makes it safe to log: format
// must never interpolate lore output into the reason.
func parseErrf(tool string, line int, snippet string, format string, a ...any) *ParseError {
	const maxSnippet = 300
	if len(snippet) > maxSnippet {
		snippet = snippet[:maxSnippet] + "…"
	}
	return &ParseError{Tool: tool, Reason: fmt.Sprintf(format, a...), Line: line, Snippet: snippet}
}

// ProcessError reports that the `lore mcp` subprocess failed as a process: the
// MCP handshake did not complete, or it ended in the middle of a call.
//
// # What Error does not say
//
// Error names the stage and the underlying cause, never Stderr. kenward neither
// controls nor constrains what lore prints on its standard error, so the tail
// cannot be assumed to be free of whatever lore was working on, and an error
// string reaches the operator's log by default. Read [ProcessError.Detail] when
// diagnosing a start-up failure — that is where lore explains itself, typically
// with "run `lore init` first".
type ProcessError struct {
	// Stage is what kenward was doing, for example "lore MCP handshake failed"
	// or "lore_put: lore subprocess ended".
	Stage string
	// Stderr is the retained tail of the subprocess's standard error, as lore
	// wrote it. Treat it as lore's content rather than kenward's diagnostics.
	Stderr string
	// Err is the underlying failure.
	Err error
}

// Error implements the error interface.
func (e *ProcessError) Error() string {
	return fmt.Sprintf("memory: %s: %v", e.Stage, e.Err)
}

// Detail returns the subprocess's stderr tail.
//
// It is a method rather than part of Error so that disclosing it is a decision.
func (e *ProcessError) Detail() string { return e.Stderr }

// Unwrap returns the underlying failure.
func (e *ProcessError) Unwrap() error { return e.Err }

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
	case strings.Contains(lower, "store closed"):
		return ErrStoreUnavailable
	case strings.Contains(lower, "context canceled"):
		return context.Canceled
	case strings.Contains(lower, "context deadline exceeded"):
		return context.DeadlineExceeded
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
