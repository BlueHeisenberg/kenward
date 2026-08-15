package memory

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// This file is the whole of kenward's dependence on lore's output format.
//
// Every lore MCP tool answers with one block of human-readable text; there is no
// structured content and no JSON mode. The parsers below mirror, one for one, the
// format strings in lore's internal/mcpserver/tools.go. They are strict about the
// parts that carry meaning (ids, enums, timestamps) and lenient only where lore's
// own model is lenient (markers, which are free-form strings). Anything that does
// not match yields a *ParseError naming the expectation that failed.
//
// The fixtures under testdata/ are captured from those same format strings. When
// lore changes its output, the matching fixture is the one thing to update.

const (
	toolSearch = "lore_search"
	toolGet    = "lore_get"
	toolPut    = "lore_put"
	toolSpaces = "lore_spaces"
	toolShare  = "lore_share"
)

// confidences is lore's confidence enum, enforced by a SQL CHECK constraint and a
// Go check in lore's store. A value outside it means the output format changed.
var confidences = map[string]bool{
	"experimental": true,
	"provisional":  true,
	"validated":    true,
	"hardened":     true,
}

// origins is lore's origin enum, enforced the same way as confidences.
var origins = map[string]bool{
	"evidence":   true,
	"directive":  true,
	"convention": true,
	"constraint": true,
}

var (
	// searchHeadRe matches "{entry_id}  space:{name}  domain:{domain}".
	searchHeadRe = regexp.MustCompile(`^(\S+)  space:(.*?)  domain:(.*)$`)
	// searchTitleRe matches "  {title} ({confidence}){markers}". The title group
	// is greedy so a title that itself ends in a parenthesised word does not
	// steal the confidence.
	searchTitleRe = regexp.MustCompile(`^  (.*) \((experimental|provisional|validated|hardened)\)(.*)$`)
	// metaIDRe matches the "id {entry_id} (v{version})" segment of an entry's
	// metadata line.
	metaIDRe = regexp.MustCompile(`^id (\S+) \(v(\d+)\)$`)
	// storedRe matches lore_put's success line. Confidence and origin are
	// anchored to their enums so a domain containing ", confidence " cannot
	// shift the fields.
	storedRe = regexp.MustCompile(`^stored (\S+) \(v(\d+)\) in space (".*") — domain (.*), confidence (experimental|provisional|validated|hardened), origin (evidence|directive|convention|constraint)$`)
	// copiedRe matches lore_share's execute line.
	copiedRe = regexp.MustCompile(`^copied: new entry (\S+) in space (".*") \(source (\S+) kept in (".*")\)$`)
	// spaceRowRe matches one line of lore_spaces output.
	spaceRowRe = regexp.MustCompile(`^(.*?)  kind:(personal|shared)  members:(\d+)  entries:(\d+)((?:  project)?(?:  pinned)?)  id:(\S+)$`)
)

// parseSearch reads lore_search's result list.
//
// The returned entries carry ID, Domain, Title, Confidence and Markers, and Body
// holds lore's FTS5 snippet rather than the entry body — see Client.Search. Space
// is left zero for the caller to fill in, because lore prints a space display
// name, which is neither unique nor stable across lore instances.
func parseSearch(text string) ([]Entry, error) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil, parseErrf(toolSearch, 0, "", "empty output")
	}
	// lore answers an empty result set with prose, not with a zero-length list.
	if strings.HasPrefix(text, "no results") {
		return nil, nil
	}

	lines := strings.Split(text, "\n")
	if !strings.Contains(lines[0], "result(s) for ") {
		return nil, parseErrf(toolSearch, 1, lines[0], "expected a %q header line", "result(s) for")
	}

	var (
		out     []Entry
		cur     *Entry
		body    []string
		haveTop bool
	)
	flush := func() {
		if cur == nil {
			return
		}
		cur.Body = strings.Join(body, "\n")
		out = append(out, *cur)
		cur, body = nil, nil
	}

	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if m := searchHeadRe.FindStringSubmatch(line); m != nil {
			flush()
			cur = &Entry{ID: m[1], Domain: m[3]}
			haveTop = false
			continue
		}
		if cur == nil {
			if strings.TrimSpace(line) == "" {
				continue
			}
			return nil, parseErrf(toolSearch, i+1, line, "expected a result header before this line")
		}
		if !haveTop {
			m := searchTitleRe.FindStringSubmatch(line)
			if m == nil {
				return nil, parseErrf(toolSearch, i+1, line,
					"expected %q", "  {title} ({confidence}){markers}")
			}
			cur.Title = m[1]
			cur.Confidence = m[2]
			cur.Markers = parseMarkerRun(strings.TrimPrefix(m[3], " "))
			haveTop = true
			continue
		}
		body = append(body, strings.TrimPrefix(line, "  "))
	}
	flush()

	if len(out) == 0 {
		return nil, parseErrf(toolSearch, 0, text, "header promised results but none parsed")
	}
	return out, nil
}

// rendered is one entry as lore_get printed it.
//
// SpaceName is kept separate from the Entry because it is a local display name:
// lore does not constrain it to be unique and it can differ between two lore
// instances holding the same space. It is used only to check that an entry
// fetched by id really lives in the space the caller was authorized for.
type rendered struct {
	Entry     Entry
	SpaceName string
	// Version is lore's per-entry version counter.
	Version int
	// SourceEntry is the provenance pointer set on a shared copy, or empty.
	SourceEntry string
}

// parseRendered reads lore_get's output, which is one rendered entry or, in
// domain mode, several separated by a horizontal rule.
func parseRendered(text string) ([]rendered, error) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil, parseErrf(toolGet, 0, "", "empty output")
	}
	if strings.HasPrefix(text, "no entries in domain ") {
		return nil, nil
	}
	var out []rendered
	for _, block := range strings.Split(text, "\n---\n\n") {
		r, err := parseEntryBlock(block)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// parseEntry reads lore_get's output for a single entry, by id.
func parseEntry(text string) (rendered, error) {
	rs, err := parseRendered(text)
	if err != nil {
		return rendered{}, err
	}
	if len(rs) != 1 {
		return rendered{}, parseErrf(toolGet, 0, text, "expected exactly one entry, got %d", len(rs))
	}
	return rs[0], nil
}

// parseEntryBlock reads one rendered entry: a title line, a metadata line and an
// optional body separated from the metadata by a blank line.
//
// Entry.CreatedAt is always left zero: lore's MCP surface does not expose
// created_at on any tool.
func parseEntryBlock(block string) (rendered, error) {
	block = strings.Trim(block, "\n")
	lines := strings.Split(block, "\n")
	if len(lines) < 2 {
		return rendered{}, parseErrf(toolGet, 0, block, "expected a title line and a metadata line")
	}
	if !strings.HasPrefix(lines[0], "# ") {
		return rendered{}, parseErrf(toolGet, 1, lines[0], "expected a %q title line", "# ")
	}
	r := rendered{Entry: Entry{Title: strings.TrimPrefix(lines[0], "# ")}}
	if err := parseEntryMeta(lines[1], &r); err != nil {
		return rendered{}, err
	}
	rest := lines[2:]
	// renderEntry emits "\n" + body, so a non-empty body is preceded by exactly
	// one blank line. Tolerate its absence rather than losing the first line.
	if len(rest) > 0 && rest[0] == "" {
		rest = rest[1:]
	}
	r.Entry.Body = strings.Join(rest, "\n")
	return r, nil
}

// parseEntryMeta reads the pipe-separated metadata line:
//
//	id {id} (v{n}) | space {name} | domain {d} | {confidence} | origin {origin}[ | {markers}][ | copied from {id}] | updated {ts}
func parseEntryMeta(line string, r *rendered) error {
	e := &r.Entry
	seg := strings.Split(line, " | ")
	if len(seg) < 6 {
		return parseErrf(toolGet, 2, line, "expected at least 6 pipe-separated metadata fields, got %d", len(seg))
	}
	m := metaIDRe.FindStringSubmatch(seg[0])
	if m == nil {
		return parseErrf(toolGet, 2, seg[0], "expected %q", "id {entry_id} (v{version})")
	}
	e.ID = m[1]
	v, err := strconv.Atoi(m[2])
	if err != nil {
		return parseErrf(toolGet, 2, m[2], "unparseable version: %v", err)
	}
	r.Version = v

	if !strings.HasPrefix(seg[1], "space ") {
		return parseErrf(toolGet, 2, seg[1], "expected a %q field", "space ")
	}
	r.SpaceName = strings.TrimPrefix(seg[1], "space ")
	if !strings.HasPrefix(seg[2], "domain ") {
		return parseErrf(toolGet, 2, seg[2], "expected a %q field", "domain ")
	}
	e.Domain = strings.TrimPrefix(seg[2], "domain ")

	if !confidences[seg[3]] {
		return parseErrf(toolGet, 2, seg[3], "%q is not one of lore's confidence values", seg[3])
	}
	e.Confidence = seg[3]

	if !strings.HasPrefix(seg[4], "origin ") {
		return parseErrf(toolGet, 2, seg[4], "expected an %q field", "origin ")
	}
	origin := strings.TrimPrefix(seg[4], "origin ")
	if !origins[origin] {
		return parseErrf(toolGet, 2, origin, "%q is not one of lore's origin values", origin)
	}
	e.Origin = origin

	last := seg[len(seg)-1]
	if !strings.HasPrefix(last, "updated ") {
		return parseErrf(toolGet, 2, last, "expected a trailing %q field", "updated ")
	}
	ts, err := parseLoreTime(strings.TrimPrefix(last, "updated "))
	if err != nil {
		return parseErrf(toolGet, 2, last, "unparseable updated timestamp: %v", err)
	}
	e.UpdatedAt = ts

	middle := seg[5 : len(seg)-1]
	if len(middle) > 2 {
		return parseErrf(toolGet, 2, line, "unexpected extra metadata fields: %v", middle)
	}
	seenMarkers := false
	for _, f := range middle {
		if strings.HasPrefix(f, "copied from ") {
			r.SourceEntry = strings.TrimPrefix(f, "copied from ")
			continue
		}
		if seenMarkers {
			return parseErrf(toolGet, 2, f, "second unrecognised metadata field")
		}
		e.Markers = parseMarkerList(f)
		seenMarkers = true
	}
	return nil
}

// spaceRow is one line of lore_spaces output.
//
// The member count lore prints is deliberately not carried here: lore_spaces
// hard-codes members:1 regardless of the real member list.
type spaceRow struct {
	Name    string
	Kind    string
	Entries int
	Project bool
	Pinned  bool
	ID      string
}

// parseSpaces reads lore_spaces' listing.
func parseSpaces(text string) ([]spaceRow, error) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return nil, parseErrf(toolSpaces, 0, "", "empty output")
	}
	if strings.HasPrefix(text, "no spaces") {
		return nil, nil
	}
	var out []spaceRow
	for i, line := range strings.Split(text, "\n") {
		m := spaceRowRe.FindStringSubmatch(line)
		if m == nil {
			return nil, parseErrf(toolSpaces, i+1, line,
				"expected %q", "{name}  kind:{kind}  members:{n}  entries:{n}[  project][  pinned]  id:{space_id}")
		}
		n, err := strconv.Atoi(m[4])
		if err != nil {
			return nil, parseErrf(toolSpaces, i+1, m[4], "unparseable entry count: %v", err)
		}
		out = append(out, spaceRow{
			Name:    m[1],
			Kind:    m[2],
			Entries: n,
			Project: strings.Contains(m[5], "project"),
			Pinned:  strings.Contains(m[5], "pinned"),
			ID:      m[6],
		})
	}
	return out, nil
}

// stored is the parsed result of a lore_put.
type stored struct {
	ID         string
	Version    int
	SpaceName  string
	Domain     string
	Confidence string
	Origin     string
}

// parseStored reads lore_put's success line.
func parseStored(text string) (stored, error) {
	text = strings.TrimRight(text, "\n")
	m := storedRe.FindStringSubmatch(text)
	if m == nil {
		return stored{}, parseErrf(toolPut, 0, text,
			"expected %q", `stored {id} (v{n}) in space "{name}" — domain {d}, confidence {c}, origin {o}`)
	}
	v, err := strconv.Atoi(m[2])
	if err != nil {
		return stored{}, parseErrf(toolPut, 0, m[2], "unparseable version: %v", err)
	}
	name, err := strconv.Unquote(m[3])
	if err != nil {
		return stored{}, parseErrf(toolPut, 0, m[3], "unparseable quoted space name: %v", err)
	}
	return stored{ID: m[1], Version: v, SpaceName: name, Domain: m[4], Confidence: m[5], Origin: m[6]}, nil
}

// copied is the parsed result of an executed lore_share.
type copied struct {
	NewID     string
	ToSpace   string
	SourceID  string
	FromSpace string
}

// parseCopied reads lore_share's execute line. A preview (confirm omitted or
// false) does not match and is reported as a parse failure, which is the intended
// behaviour: this client never previews, so preview output means the call was
// built wrong.
func parseCopied(text string) (copied, error) {
	text = strings.TrimRight(text, "\n")
	m := copiedRe.FindStringSubmatch(text)
	if m == nil {
		return copied{}, parseErrf(toolShare, 0, text,
			"expected %q", `copied: new entry {id} in space "{name}" (source {id} kept in "{name}")`)
	}
	to, err := strconv.Unquote(m[2])
	if err != nil {
		return copied{}, parseErrf(toolShare, 0, m[2], "unparseable quoted space name: %v", err)
	}
	from, err := strconv.Unquote(m[4])
	if err != nil {
		return copied{}, parseErrf(toolShare, 0, m[4], "unparseable quoted space name: %v", err)
	}
	return copied{NewID: m[1], ToSpace: to, SourceID: m[3], FromSpace: from}, nil
}

// parseMarkerRun splits lore_search's marker run, which is the marker list joined
// with no separator at all, for example "[CONTEXT][NON-NEGOTIABLE]".
//
// Markers are free-form strings in lore — the eight-marker convention is exactly
// that — so anything that is not a clean run of bracketed tokens is returned
// whole rather than guessed at or dropped.
func parseMarkerRun(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	rest := s
	for strings.HasPrefix(rest, "[") {
		i := strings.IndexByte(rest, ']')
		if i < 0 {
			break
		}
		out = append(out, rest[:i+1])
		rest = rest[i+1:]
	}
	if rest != "" {
		return []string{s}
	}
	return out
}

// parseMarkerList splits lore_get's marker field, which is the marker list joined
// with single spaces. Markers are not validated against any vocabulary.
func parseMarkerList(s string) []string {
	var out []string
	for _, f := range strings.Split(s, " ") {
		if f != "" {
			out = append(out, f)
		}
	}
	return out
}

// parseLoreTime parses one of lore's timestamps.
//
// lore writes RFC3339 with nine fractional digits in UTC. When a new version is
// written faster than the clock advances, lore appends literal "0" characters to
// force the string strictly greater than the previous value, which makes the
// timestamp no longer valid RFC3339. Those are stripped before giving up.
func parseLoreTime(s string) (time.Time, error) {
	const maxSkewPadding = 8
	cur := s
	var err error
	for range maxSkewPadding + 1 {
		var t time.Time
		t, err = time.Parse(time.RFC3339Nano, cur)
		if err == nil {
			return t.UTC(), nil
		}
		if !strings.HasSuffix(cur, "0") {
			break
		}
		cur = cur[:len(cur)-1]
	}
	return time.Time{}, err
}
