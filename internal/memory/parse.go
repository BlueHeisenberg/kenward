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

var (
	// searchHeadRe matches "{entry_id}  space:{name}  domain:{domain}".
	searchHeadRe = regexp.MustCompile(`^(\S+)  space:(.*?)  domain:(.*)$`)
	// searchTitleRe matches "  {title} ({confidence}){markers}". The title group
	// is greedy so a title that itself ends in a parenthesised word does not
	// steal the confidence.
	searchTitleRe = regexp.MustCompile(`^  (.*) \((experimental|provisional|validated|hardened)\)(.*)$`)
	// metaLineRe matches a whole rendered entry's metadata line in one pass:
	//
	//	id {id} (v{n}) | space {name} | domain {d} | {confidence} | origin {origin}[ | {markers}][ | copied from {id}] | updated {ts}
	//
	// It is anchored on the parts lore controls — the id shape, the two enums
	// and the trailing timestamp — rather than split on " | ", because markers
	// are free-form: a marker lore normalises to "[A | B]" used to inject a
	// separator and make its own entry permanently unreadable. The name and
	// domain groups are lazy so the first enum pair wins, and the optional
	// group is greedy so the last "updated" wins; a marker can therefore hold
	// any of those words without shifting a field.
	metaLineRe = regexp.MustCompile(`^id (\S+) \(v(\d+)\) \| space (.*?) \| domain (.*?) \| (experimental|provisional|validated|hardened) \| origin (evidence|directive|convention|constraint)(?: \| (.*))? \| updated (\S+)$`)
	// metaCopiedRe matches the provenance field, which lore emits last, just
	// before "updated". Anchoring it to the end of the optional middle is what
	// lets the markers field in front of it contain a " | ".
	metaCopiedRe = regexp.MustCompile(`(?:^| \| )copied from (\S+)$`)
	// entryHeadRe matches the two lines a rendered entry starts with. It is what
	// makes splitting domain-mode output safe: lore's horizontal rule is a
	// separator only when a real entry head follows it.
	entryHeadRe = regexp.MustCompile(`^# [^\n]*\nid \S+ \(v\d+\) \| `)
	// storedRe matches lore_put's success line. Confidence and origin are
	// anchored to their enums so a domain containing ", confidence " cannot
	// shift the fields.
	storedRe = regexp.MustCompile(`^stored (\S+) \(v(\d+)\) in space (".*") — domain (.*), confidence (experimental|provisional|validated|hardened), origin (evidence|directive|convention|constraint)$`)
	// copiedRe matches lore_share's execute line.
	copiedRe = regexp.MustCompile(`^copied: new entry (\S+) in space (".*") \(source (\S+) kept in (".*")\)$`)
	// spaceRowRe matches one line of lore_spaces output.
	spaceRowRe = regexp.MustCompile(`^(.*?)  kind:(personal|shared)  members:(\d+)  entries:(\d+)((?:  project)?(?:  pinned)?)  id:(\S+)$`)
)

// highlightRe matches one of FTS5's highlighted spans. lore builds its snippets
// with snippet(entry_fts, 1, '[', ']', '…', 12), so matched terms arrive wrapped
// in square brackets. The pattern refuses nested brackets so that at worst one
// bracketed run is unwrapped.
var highlightRe = regexp.MustCompile(`\[([^\[\]]*)\]`)

// stripHighlights removes FTS5's match brackets and keeps everything else,
// including the ellipsis that marks elided text.
//
// It cannot tell a highlight bracket from a square bracket that was in the body
// all along, which is why the raw snippet is kept on Excerpt.Snippet.
func stripHighlights(s string) string {
	return highlightRe.ReplaceAllString(s, "$1")
}

// parseSearch reads lore_search's result list.
//
// The returned excerpts carry ID, Domain, Title, Confidence and Markers, with
// Entry.Body holding the de-highlighted snippet and Snippet the raw one. Space is
// left zero for the caller to fill in, because lore prints a space display name,
// which is neither unique nor stable across lore instances; that echoed name is
// kept on Excerpt.spaceName so the caller can still check it against the space it
// asked for.
func parseSearch(text string) ([]Excerpt, error) {
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
		out       []Excerpt
		cur       *Entry
		spaceName string
		body      []string
		haveTop   bool
		head      string
		headLine  int
	)
	flush := func() error {
		if cur == nil {
			return nil
		}
		// A result whose title line never arrived is a truncated or changed
		// format, not an entry with an empty title.
		if !haveTop {
			return parseErrf(toolSearch, headLine, head, "expected %q", "  {title} ({confidence}){markers}")
		}
		raw := strings.Join(body, "\n")
		cur.Body = stripHighlights(raw)
		// A search hit is an excerpt: the body is elided and lore reports no
		// origin and no timestamps for it.
		cur.Partial = true
		out = append(out, Excerpt{Entry: *cur, Snippet: raw, spaceName: spaceName})
		cur, body = nil, nil
		return nil
	}

	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if m := searchHeadRe.FindStringSubmatch(line); m != nil {
			if err := flush(); err != nil {
				return nil, err
			}
			cur = &Entry{ID: m[1], Domain: m[3]}
			spaceName, head, headLine = m[2], line, i+1
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
	if err := flush(); err != nil {
		return nil, err
	}

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
	for _, block := range splitEntryBlocks(text) {
		r, err := parseEntryBlock(block)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

// splitEntryBlocks cuts domain-mode output at lore's horizontal rule, but only
// where a rendered entry head actually follows it. A body containing a rule and a
// blank line is body text, not a new entry.
func splitEntryBlocks(text string) []string {
	const sep = "\n---\n\n"
	var out []string
	start, i := 0, 0
	for {
		j := strings.Index(text[i:], sep)
		if j < 0 {
			return append(out, text[start:])
		}
		next := i + j + len(sep)
		if !entryHeadRe.MatchString(text[next:]) {
			i += j + 1
			continue
		}
		out = append(out, text[start:i+j])
		start, i = next, next
	}
}

// parseEntry reads lore_get's output for a single entry, by id.
//
// It does not split: a get by id renders exactly one entry, so everything after
// the metadata line is body — including a line that looks like a separator, or
// like the head of another entry. Splitting here is what let an ordinary note
// containing a horizontal rule make itself unreadable. Domain mode, which really
// can return several entries, is parseRendered's problem.
func parseEntry(text string) (rendered, error) {
	text = strings.TrimRight(text, "\n")
	if text == "" {
		return rendered{}, parseErrf(toolGet, 0, "", "empty output")
	}
	return parseEntryBlock(text)
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
	// lore_get renders the whole entry, so this is never partial.
	r := rendered{Entry: Entry{Title: strings.TrimPrefix(lines[0], "# "), Partial: false}}
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

// parseEntryMeta reads the metadata line, matching it whole rather than splitting
// it on " | ".
//
// Splitting was what a free-form marker could break: markers are arbitrary
// strings in lore, so a marker containing " | " injected a separator that shifted
// every later field and left the entry unreadable for good. Matching against
// metaLineRe pins the fields lore's own model constrains — the id, the two enums,
// the trailing timestamp — and lets the two free-form ones absorb whatever they
// contain. The price is that an unrecognised extra field is no longer rejected:
// it lands among the markers, which is where lore's leniency already lives.
func parseEntryMeta(line string, r *rendered) error {
	m := metaLineRe.FindStringSubmatch(line)
	if m == nil {
		return parseErrf(toolGet, 2, line, "expected %q",
			"id {id} (v{n}) | space {name} | domain {d} | {confidence} | origin {origin}[ | {markers}][ | copied from {id}] | updated {ts}")
	}
	v, err := strconv.Atoi(m[2])
	if err != nil {
		return parseErrf(toolGet, 2, m[2], "unparseable version")
	}
	ts, err := parseLoreTime(m[8])
	if err != nil {
		return parseErrf(toolGet, 2, m[8], "unparseable updated timestamp")
	}

	e := &r.Entry
	e.ID, r.Version, r.SpaceName = m[1], v, m[3]
	e.Domain, e.Confidence, e.Origin = m[4], m[5], m[6]
	e.UpdatedAt = ts

	markers := m[7]
	if cp := metaCopiedRe.FindStringSubmatch(markers); cp != nil {
		r.SourceEntry = cp[1]
		markers = strings.TrimSuffix(markers, cp[0])
	}
	e.Markers = parseMarkerList(markers)
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
			return nil, parseErrf(toolSpaces, i+1, m[4], "unparseable entry count")
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
		return stored{}, parseErrf(toolPut, 0, m[2], "unparseable version")
	}
	name, err := strconv.Unquote(m[3])
	if err != nil {
		return stored{}, parseErrf(toolPut, 0, m[3], "unparseable quoted space name")
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
		return copied{}, parseErrf(toolShare, 0, m[2], "unparseable quoted space name")
	}
	from, err := strconv.Unquote(m[4])
	if err != nil {
		return copied{}, parseErrf(toolShare, 0, m[4], "unparseable quoted space name")
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
//
// lore wraps a marker in brackets unless it already starts with one, so a clean
// run of bracketed tokens is split on the brackets: that keeps a marker holding a
// space, or a " | ", whole. Anything else falls back to whitespace fields, which
// is the best guess available for a marker lore did not bracket.
func parseMarkerList(s string) []string {
	var out []string
	rest := s
	for strings.HasPrefix(rest, "[") {
		i := strings.IndexByte(rest, ']')
		if i < 0 {
			break
		}
		out = append(out, rest[:i+1])
		rest = strings.TrimPrefix(rest[i+1:], " ")
	}
	if rest == "" {
		return out
	}
	return strings.Fields(s)
}

// parseLoreTime parses one of lore's timestamps.
//
// lore writes RFC3339 with nine fractional digits in UTC. When a new version is
// written faster than the clock advances, lore appends literal "0" characters to
// force the string strictly greater than the previous value, which makes the
// timestamp no longer valid RFC3339. Those are stripped before giving up — one
// per same-tick write, with no ceiling, because the number of writes in a tick is
// lore's business and a capped strip turns a burst into a parse failure.
func parseLoreTime(s string) (time.Time, error) {
	cur := s
	var err error
	for {
		var t time.Time
		t, err = time.Parse(time.RFC3339Nano, cur)
		if err == nil {
			return t.UTC(), nil
		}
		if !strings.HasSuffix(cur, "0") {
			return time.Time{}, err
		}
		cur = cur[:len(cur)-1]
	}
}
