package memory

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// golden loads a fixture. Line endings are normalised so that a checkout with
// autocrlf still exercises the parser the way lore feeds it.
func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	return strings.ReplaceAll(string(b), "\r\n", "\n")
}

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("bad test timestamp %q: %v", s, err)
	}
	return ts.UTC()
}

func TestParseSearchGolden(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    []Excerpt
	}{
		{
			name:    "two results",
			fixture: "search_basic.txt",
			want: []Excerpt{
				{
					Entry: Entry{
						ID:         "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f",
						Domain:     "home/routine",
						Title:      "Bin day is Tuesday",
						Confidence: "validated",
						Body:       "the green bin goes out Tuesday night, recycling…",
					},
					Snippet: "the green [bin] goes out Tuesday night, recycling…",
				},
				{
					Entry: Entry{
						ID:         "7b2d4c11-9e88-4d31-b0aa-1f2e3d4c5b6a",
						Domain:     "home/routine",
						Title:      "Recycling collection",
						Confidence: "provisional",
						Body:       "…recycling is fortnightly on the same day",
					},
					Snippet: "…[recycling] is fortnightly on the same [day]",
				},
			},
		},
		{
			name:    "markers are joined with no separator",
			fixture: "search_markers.txt",
			want: []Excerpt{{
				Entry: Entry{
					ID:         "a1b2c3d4-0000-4000-8000-000000000001",
					Domain:     "home/heating",
					Title:      "Boiler service window",
					Confidence: "hardened",
					Markers:    []string{"[CONTEXT]", "[NON-NEGOTIABLE]"},
					Body:       "the boiler must be serviced before…",
				},
				Snippet: "the [boiler] must be serviced before…",
			}},
		},
		{
			name:    "an empty snippet is still a line",
			fixture: "search_empty_snippet.txt",
			want: []Excerpt{
				{
					Entry: Entry{
						ID:         "a1b2c3d4-0000-4000-8000-000000000002",
						Domain:     "home/kitchen",
						Title:      "Kettle",
						Confidence: "experimental",
					},
				},
				{
					Entry: Entry{
						ID:         "a1b2c3d4-0000-4000-8000-000000000003",
						Domain:     "home/kitchen",
						Title:      "Kettle descaling",
						Confidence: "validated",
						Markers:    []string{"[UPDATED]"},
						Body:       "descale the kettle monthly",
					},
					Snippet: "descale the [kettle] monthly",
				},
			},
		},
		{
			name:    "a parenthesised title does not steal the confidence",
			fixture: "search_title_parens.txt",
			want: []Excerpt{{
				Entry: Entry{
					ID:         "a1b2c3d4-0000-4000-8000-000000000004",
					Domain:     "home/network",
					Title:      "Router reset (the one in the hall)",
					Confidence: "provisional",
					Markers:    []string{"[IMPORTANT]"},
					Body:       "hold the router reset pin for ten…",
				},
				Snippet: "hold the [router] reset pin for ten…",
			}},
		},
		{
			name:    "no matches is not an error",
			fixture: "search_none.txt",
			want:    nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseSearch(golden(t, tc.fixture))
			if err != nil {
				t.Fatalf("parseSearch: %v", err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("got %d excerpts, want %d: %+v", len(got), len(tc.want), got)
			}
			for i := range got {
				g, w := got[i].Entry, tc.want[i].Entry
				if g.ID != w.ID || g.Domain != w.Domain || g.Title != w.Title ||
					g.Confidence != w.Confidence || g.Body != w.Body ||
					!slices.Equal(g.Markers, w.Markers) {
					t.Errorf("excerpt %d:\n got %+v\nwant %+v", i, g, w)
				}
				if got[i].Snippet != tc.want[i].Snippet {
					t.Errorf("excerpt %d raw snippet:\n got %q\nwant %q", i, got[i].Snippet, tc.want[i].Snippet)
				}
				if strings.ContainsAny(g.Body, "[]") {
					t.Errorf("excerpt %d body still carries FTS5 highlighting: %q", i, g.Body)
				}
				if !g.UpdatedAt.IsZero() || !g.CreatedAt.IsZero() || g.Origin != "" {
					t.Errorf("excerpt %d: lore_search reports no origin or timestamps, got %+v", i, g)
				}
				if !g.Partial {
					t.Errorf("excerpt %d must be marked Partial: %+v", i, g)
				}
			}
		})
	}
}

func TestStripHighlights(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"", ""},
		{"plain text", "plain text"},
		{"the green [bin] goes out", "the green bin goes out"},
		// The elision marker says text was dropped and must survive.
		{"…[recycling] is fortnightly…", "…recycling is fortnightly…"},
		{"[a] and [b]", "a and b"},
		// A phrase match arrives as one bracketed span.
		{"the [bin day] rule", "the bin day rule"},
		// Unbalanced brackets are left alone rather than guessed at.
		{"an unclosed [bracket", "an unclosed [bracket"},
	}
	for _, tc := range tests {
		if got := stripHighlights(tc.in); got != tc.want {
			t.Errorf("stripHighlights(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestPartialIsSetByTheParsers pins the flag at the point it is written: every
// search hit is partial, every rendered entry is not.
func TestPartialIsSetByTheParsers(t *testing.T) {
	for _, fixture := range []string{"search_basic.txt", "search_markers.txt", "search_empty_snippet.txt"} {
		xs, err := parseSearch(golden(t, fixture))
		if err != nil {
			t.Fatalf("parseSearch(%s): %v", fixture, err)
		}
		for _, x := range xs {
			if !x.Entry.Partial {
				t.Errorf("%s: a search hit must be Partial: %+v", fixture, x.Entry)
			}
			if !IsExcerpt(x.Entry) {
				t.Errorf("%s: IsExcerpt must agree with Partial: %+v", fixture, x.Entry)
			}
		}
	}
	for _, fixture := range []string{"get_minimal.txt", "get_full.txt", "get_empty_body.txt", "get_copy.txt"} {
		r, err := parseEntry(golden(t, fixture))
		if err != nil {
			t.Fatalf("parseEntry(%s): %v", fixture, err)
		}
		if r.Entry.Partial {
			t.Errorf("%s: a rendered entry must not be Partial: %+v", fixture, r.Entry)
		}
		if IsExcerpt(r.Entry) {
			t.Errorf("%s: IsExcerpt must agree with Partial: %+v", fixture, r.Entry)
		}
	}
}

func TestParseEntryGolden(t *testing.T) {
	tests := []struct {
		name    string
		fixture string
		want    rendered
	}{
		{
			name:    "minimal",
			fixture: "get_minimal.txt",
			want: rendered{
				Entry: Entry{
					ID:         "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f",
					Domain:     "home/routine",
					Title:      "Bin day is Tuesday",
					Body:       "The green bin goes out Tuesday night.",
					Confidence: "provisional",
					Origin:     "evidence",
					UpdatedAt:  mustTime(t, "2026-08-01T09:15:00.000000000Z"),
				},
				SpaceName: "hearth-private",
				Version:   1,
			},
		},
		{
			name:    "markers, provenance and a body that looks like metadata",
			fixture: "get_full.txt",
			want: rendered{
				Entry: Entry{
					ID:         "a1b2c3d4-0000-4000-8000-000000000001",
					Domain:     "home/heating",
					Title:      "Boiler service window",
					Body:       "The boiler must be serviced before the first frost.\n\nTwo paragraphs, and a line that looks like metadata: id x (v1) | space y.",
					Confidence: "hardened",
					Origin:     "constraint",
					Markers:    []string{"[CONTEXT]", "[NON-NEGOTIABLE]"},
					UpdatedAt:  mustTime(t, "2026-08-14T22:04:11.123456789Z"),
				},
				SpaceName:   "household",
				Version:     7,
				SourceEntry: "9f8e7d6c-0000-4000-8000-00000000000a",
			},
		},
		{
			name:    "empty body",
			fixture: "get_empty_body.txt",
			want: rendered{
				Entry: Entry{
					ID:         "a1b2c3d4-0000-4000-8000-000000000005",
					Domain:     "home/misc",
					Title:      "Just a title",
					Confidence: "experimental",
					Origin:     "directive",
					UpdatedAt:  mustTime(t, "2026-01-02T03:04:05.000000000Z"),
				},
				SpaceName: "household",
				Version:   2,
			},
		},
		{
			// A marker is a free-form string, so a member or a model can put
			// lore's own field separator inside one. That used to shift every
			// later field and make the entry unreadable for good.
			name:    "a marker containing the field separator",
			fixture: "get_marker_pipe.txt",
			want: rendered{
				Entry: Entry{
					ID:         "a1b2c3d4-0000-4000-8000-000000000007",
					Domain:     "home/misc",
					Title:      "Marker vocabulary",
					Body:       "A marker lore normalised with a pipe in it.",
					Confidence: "validated",
					Origin:     "convention",
					Markers:    []string{"[CONTEXT | STAGING]", "[NON-NEGOTIABLE]"},
					UpdatedAt:  mustTime(t, "2026-05-06T07:08:09.000000000Z"),
				},
				SpaceName:   "household",
				Version:     2,
				SourceEntry: "9f8e7d6c-0000-4000-8000-00000000000b",
			},
		},
		{
			// An ordinary note about lore's own output: a horizontal rule, a
			// blank line, and then something shaped like an entry head. A get by
			// id renders one entry, so all of it is body.
			name:    "a body containing a horizontal rule and an entry head",
			fixture: "get_body_rule.txt",
			want: rendered{
				Entry: Entry{
					ID:     "a1b2c3d4-0000-4000-8000-000000000008",
					Domain: "home/notes",
					Title:  "How lore renders an entry",
					Body: "Notes on the format, with a horizontal rule and then a worked example:\n\n---\n\n" +
						"# Second entry\nid a1b2c3d4-0000-4000-8000-000000000009 (v1) | space household | " +
						"domain home/notes | validated | origin directive | updated 2026-05-06T07:08:09.000000000Z",
					Confidence: "provisional",
					Origin:     "evidence",
					UpdatedAt:  mustTime(t, "2026-05-06T07:08:09.000000000Z"),
				},
				SpaceName: "hearth-private",
				Version:   1,
			},
		},
		{
			name:    "clock-skew padding on updated_at",
			fixture: "get_clock_skew.txt",
			want: rendered{
				Entry: Entry{
					ID:         "a1b2c3d4-0000-4000-8000-000000000006",
					Domain:     "home/misc",
					Title:      "Rewritten twice in the same nanosecond",
					Body:       "Body.",
					Confidence: "validated",
					Origin:     "convention",
					UpdatedAt:  mustTime(t, "2026-03-04T05:06:07.000000000Z"),
				},
				SpaceName: "household",
				Version:   3,
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseEntry(golden(t, tc.fixture))
			if err != nil {
				t.Fatalf("parseEntry: %v", err)
			}
			if got.SpaceName != tc.want.SpaceName || got.Version != tc.want.Version || got.SourceEntry != tc.want.SourceEntry {
				t.Errorf("envelope:\n got %+v\nwant %+v", got, tc.want)
			}
			g, w := got.Entry, tc.want.Entry
			if g.ID != w.ID || g.Domain != w.Domain || g.Title != w.Title || g.Body != w.Body ||
				g.Confidence != w.Confidence || g.Origin != w.Origin ||
				!g.UpdatedAt.Equal(w.UpdatedAt) || !slices.Equal(g.Markers, w.Markers) {
				t.Errorf("entry:\n got %+v\nwant %+v", g, w)
			}
			if !g.CreatedAt.IsZero() {
				t.Errorf("lore never reports created_at, got %v", g.CreatedAt)
			}
			if g.Space != "" {
				t.Errorf("the parser must not invent a space id, got %q", g.Space)
			}
		})
	}
}

func TestParseRenderedDomainMode(t *testing.T) {
	got, err := parseRendered(golden(t, "get_domain_multi.txt"))
	if err != nil {
		t.Fatalf("parseRendered: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d entries, want 2", len(got))
	}
	if got[0].Entry.Body != "Body of the first." || got[1].Entry.Body != "Body of the second." {
		t.Errorf("bodies: %q / %q", got[0].Entry.Body, got[1].Entry.Body)
	}
	if got[0].SpaceName != "household" || got[1].SpaceName != "hearth-private" {
		t.Errorf("space names: %q / %q", got[0].SpaceName, got[1].SpaceName)
	}

	// Domain mode cuts at lore's rule only where an entry head follows it, so a
	// body that merely contains a rule stays one entry. Where a body forges a
	// head as well, domain mode cannot tell — which is exactly why a get by id,
	// the path Get, Share and Put's read-back use, does not split at all.
	withRule := strings.Replace(golden(t, "get_minimal.txt"),
		"The green bin goes out Tuesday night.",
		"The green bin goes out Tuesday night.\n\n---\n\nAnd the black one on Friday.", 1)
	one, err := parseRendered(withRule)
	if err != nil {
		t.Fatalf("parseRendered on a body with a horizontal rule: %v", err)
	}
	if len(one) != 1 {
		t.Errorf("a horizontal rule in a body split the entry into %d", len(one))
	}

	empty, err := parseRendered(golden(t, "get_domain_none.txt"))
	if err != nil {
		t.Fatalf("parseRendered on an empty domain: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("got %d entries, want none", len(empty))
	}
}

func TestParseStoredGolden(t *testing.T) {
	got, err := parseStored(golden(t, "put_stored.txt"))
	if err != nil {
		t.Fatalf("parseStored: %v", err)
	}
	want := stored{
		ID: "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f", Version: 1, SpaceName: "hearth-private",
		Domain: "home/routine", Confidence: "provisional", Origin: "evidence",
	}
	if got != want {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}

	got, err = parseStored(golden(t, "put_stored_version.txt"))
	if err != nil {
		t.Fatalf("parseStored: %v", err)
	}
	if got.Version != 7 || got.Confidence != "hardened" || got.Origin != "constraint" {
		t.Errorf("got %+v", got)
	}
}

func TestParseCopiedGolden(t *testing.T) {
	got, err := parseCopied(golden(t, "share_copied.txt"))
	if err != nil {
		t.Fatalf("parseCopied: %v", err)
	}
	want := copied{
		NewID:     "5f5f0000-0000-4000-8000-000000000000",
		ToSpace:   "household",
		SourceID:  "3f1c9e2a-6d0b-4a52-9f0e-8c1d2b3a4e5f",
		FromSpace: "hearth-private",
	}
	if got != want {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}
}

// TestParseCopiedRejectsPreview guards the two-phase share: a preview means the
// call was built without confirm, and must never be mistaken for a completed copy.
func TestParseCopiedRejectsPreview(t *testing.T) {
	if _, err := parseCopied(golden(t, "share_preview.txt")); err == nil {
		t.Fatal("preview output parsed as a completed copy")
	} else if pe := (*ParseError)(nil); !errors.As(err, &pe) {
		t.Fatalf("want a *ParseError, got %T: %v", err, err)
	}
}

func TestParseSpacesGolden(t *testing.T) {
	got, err := parseSpaces(golden(t, "spaces_list.txt"))
	if err != nil {
		t.Fatalf("parseSpaces: %v", err)
	}
	want := []spaceRow{
		{Name: "personal", Kind: "personal", Entries: 42, ID: "1c0a0000-0000-4000-8000-000000000000"},
		{Name: "hearth-private", Kind: "shared", Entries: 7, ID: "2d1b0000-0000-4000-8000-000000000000"},
		{Name: "household", Kind: "shared", Entries: 19, Pinned: true, ID: "3e2c0000-0000-4000-8000-000000000000"},
		{Name: "kenward", Kind: "shared", Entries: 3, Project: true, Pinned: true, ID: "4f3d0000-0000-4000-8000-000000000000"},
		// Longer than the width `lore spaces` pads its name column to. The MCP
		// tool does not pad — every field is separated by exactly two spaces
		// whatever the name's length — and this row is what says so.
		{Name: "kenward-test-household", Kind: "shared", Entries: 0, ID: "5a4e0000-0000-4000-8000-000000000000"},
	}
	if !slices.Equal(got, want) {
		t.Errorf("\n got %+v\nwant %+v", got, want)
	}

	empty, err := parseSpaces(golden(t, "spaces_empty.txt"))
	if err != nil {
		t.Fatalf("parseSpaces on an empty store: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("got %d rows, want none", len(empty))
	}
}

// TestParseRejectsChangedFormat is the safety net the whole design rests on: when
// lore's output stops matching, every parser must say so rather than return an
// Entry with holes in it.
func TestParseRejectsChangedFormat(t *testing.T) {
	tests := []struct {
		fixture string
		parse   func(string) error
		reason  string
	}{
		{"bad_search_header.txt", func(s string) error { _, err := parseSearch(s); return err }, "header line"},
		{"bad_search_title.txt", func(s string) error { _, err := parseSearch(s); return err }, "title line"},
		{"bad_get_meta.txt", func(s string) error { _, err := parseEntry(s); return err }, "metadata fields"},
		{"bad_get_confidence.txt", func(s string) error { _, err := parseEntry(s); return err }, "confidence enum"},
		{"bad_get_origin.txt", func(s string) error { _, err := parseEntry(s); return err }, "origin enum"},
		{"bad_get_timestamp.txt", func(s string) error { _, err := parseEntry(s); return err }, "timestamp"},
		{"bad_put_line.txt", func(s string) error { _, err := parseStored(s); return err }, "put receipt"},
		{"bad_spaces_row.txt", func(s string) error { _, err := parseSpaces(s); return err }, "space row"},
	}
	for _, tc := range tests {
		t.Run(tc.fixture, func(t *testing.T) {
			err := tc.parse(golden(t, tc.fixture))
			if err == nil {
				t.Fatalf("%s parsed cleanly; a format change must be an error", tc.reason)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("want a *ParseError for %s, got %T: %v", tc.reason, err, err)
			}
			if pe.Tool == "" || pe.Reason == "" {
				t.Errorf("a ParseError must name the tool and the expectation: %+v", pe)
			}
		})
	}
}

func TestParseMarkers(t *testing.T) {
	tests := []struct {
		name string
		run  string
		want []string
	}{
		{"none", "", nil},
		{"one", "[CONTEXT]", []string{"[CONTEXT]"}},
		{"several", "[CONTEXT][UPDATED][NON-NEGOTIABLE]", []string{"[CONTEXT]", "[UPDATED]", "[NON-NEGOTIABLE]"}},
		// Markers are free-form strings in lore; the eight-marker set is a
		// convention, so an unbracketed marker is carried through whole rather
		// than rejected or split on a guess.
		{"unbracketed", "custom-marker", []string{"custom-marker"}},
		{"partly bracketed", "[CONTEXT]trailing", []string{"[CONTEXT]trailing"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseMarkerRun(tc.run); !slices.Equal(got, tc.want) {
				t.Errorf("parseMarkerRun(%q) = %v, want %v", tc.run, got, tc.want)
			}
		})
	}
}

// TestParseMarkerList covers lore_get's space-joined marker field, where the
// bracket — not the space — is the delimiter, because a marker can contain a
// space or lore's own field separator.
func TestParseMarkerList(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"none", "", nil},
		{"one", "[CONTEXT]", []string{"[CONTEXT]"}},
		{"several", "[CONTEXT] [UPDATED]", []string{"[CONTEXT]", "[UPDATED]"}},
		{"a marker with a space", "[NEEDS REVIEW] [CONTEXT]", []string{"[NEEDS REVIEW]", "[CONTEXT]"}},
		{"a marker with the field separator", "[A | B] [C]", []string{"[A | B]", "[C]"}},
		{"unbracketed falls back to fields", "custom marker", []string{"custom", "marker"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseMarkerList(tc.in); !slices.Equal(got, tc.want) {
				t.Errorf("parseMarkerList(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestParseLoreTime(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "2026-08-01T09:15:00.000000000Z", want: "2026-08-01T09:15:00Z"},
		{in: "2026-08-01T09:15:00.123456789Z", want: "2026-08-01T09:15:00.123456789Z"},
		// lore appends literal "0"s to force updated_at strictly greater than
		// the previous version when the clock has not advanced.
		{in: "2026-08-01T09:15:00.000000000Z0", want: "2026-08-01T09:15:00Z"},
		{in: "2026-08-01T09:15:00.000000000Z000", want: "2026-08-01T09:15:00Z"},
		{in: "2026-08-01T11:15:00.000000000+02:00", want: "2026-08-01T09:15:00Z"},
		{in: "last Tuesday", wantErr: true},
		{in: "", wantErr: true},
	}
	for _, tc := range tests {
		t.Run(tc.in, func(t *testing.T) {
			got, err := parseLoreTime(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("want an error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLoreTime: %v", err)
			}
			if got.Format(time.RFC3339Nano) != tc.want {
				t.Errorf("got %s, want %s", got.Format(time.RFC3339Nano), tc.want)
			}
		})
	}
}
