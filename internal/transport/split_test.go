package transport

import (
	"strings"
	"testing"
)

func TestUTF16Len(t *testing.T) {
	cases := []struct {
		in   string
		want int
	}{
		{"", 0},
		{"abc", 3},
		{"café", 4},
		{"日本語", 3},
		{"🔥", 2}, // outside the BMP: two UTF-16 units, as Telegram counts it
		{"a🔥b", 4},
	}
	for _, tc := range cases {
		if got := utf16Len(tc.in); got != tc.want {
			t.Fatalf("utf16Len(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestSplitMessage(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		limit int
		want  []string
	}{
		{
			name:  "fits in one",
			in:    "short enough",
			limit: 100,
			want:  []string{"short enough"},
		},
		{
			name:  "breaks on the blank line",
			in:    "first para\n\nsecond para",
			limit: 15,
			want:  []string{"first para", "second para"},
		},
		{
			name:  "breaks on a line ending when there is no blank line",
			in:    "line one\nline two\nline three",
			limit: 20,
			want:  []string{"line one\nline two", "line three"},
		},
		{
			name:  "breaks on a space when there is no line ending",
			in:    "alpha beta gamma delta",
			limit: 12,
			want:  []string{"alpha beta", "gamma delta"},
		},
		{
			name:  "breaks mid-run only as a last resort",
			in:    strings.Repeat("x", 25),
			limit: 10,
			want:  []string{"xxxxxxxxxx", "xxxxxxxxxx", "xxxxx"},
		},
		{
			name:  "keeps whole runes together",
			in:    strings.Repeat("🔥", 6),
			limit: 5,
			want:  []string{"🔥🔥", "🔥🔥", "🔥🔥"},
		},
		{
			name:  "collapses only the separator it broke on",
			in:    "one\n\n\ntwo",
			limit: 5,
			want:  []string{"one", "two"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := splitMessage(tc.in, tc.limit)
			if len(got) != len(tc.want) {
				t.Fatalf("split into %d parts %q, want %d %q", len(got), got, len(tc.want), tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("part %d = %q, want %q", i, got[i], tc.want[i])
				}
				if utf16Len(got[i]) > tc.limit {
					t.Fatalf("part %d is %d units, over the %d limit", i, utf16Len(got[i]), tc.limit)
				}
			}
		})
	}
}

// Whatever the input, the visible characters all survive the split: a reply is
// delivered in pieces, never shortened.
func TestSplitMessageNeverLosesContent(t *testing.T) {
	inputs := []string{
		strings.Repeat("word ", 500),
		strings.Repeat("paragraph text\n\n", 200),
		strings.Repeat("🔥", 300),
		strings.Repeat("nospacesatallhere", 200),
		"mixed 🔥 content\nwith lines\n\nand paragraphs " + strings.Repeat("z", 5000),
	}
	for _, limit := range []int{7, 40, 4096} {
		for i, in := range inputs {
			var rebuilt strings.Builder
			for _, part := range splitMessage(in, limit) {
				if utf16Len(part) > limit {
					t.Fatalf("input %d limit %d: part over the limit", i, limit)
				}
				rebuilt.WriteString(part)
			}
			if strip(rebuilt.String()) != strip(in) {
				t.Fatalf("input %d limit %d: content changed in splitting", i, limit)
			}
		}
	}
}

// A limit too narrow for a single rune is raised to the smallest honourable one,
// so no piece is ever wider than the effective limit.
func TestSplitMessageTinyLimit(t *testing.T) {
	for _, in := range []string{"🔥", "a🔥b", "abc"} {
		var rebuilt strings.Builder
		for _, part := range splitMessage(in, 1) {
			if utf16Len(part) > 2 {
				t.Fatalf("input %q: part %q is %d units, over the effective limit of 2", in, part, utf16Len(part))
			}
			rebuilt.WriteString(part)
		}
		if strip(rebuilt.String()) != strip(in) {
			t.Fatalf("input %q: content changed in splitting", in)
		}
	}
}

// strip removes the whitespace that a break is allowed to consume.
func strip(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\n', '\t':
			return -1
		}
		return r
	}, s)
}
