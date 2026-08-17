package transport

import (
	"strings"
	"testing"
)

// hostile is an entry title made of the parse mode's own metacharacters. A member
// is entitled to title a note this and read it back unchanged; nothing in it may
// reach Telegram as markup.
const hostile = `<b>bold</b> & <i>italic</i> </blockquote><code>`

func TestEscNeutralisesMarkup(t *testing.T) {
	got := Esc(hostile)
	for _, bad := range []string{"<b>", "</b>", "<i>", "</blockquote>", "<code>"} {
		if strings.Contains(got, bad) {
			t.Errorf("Esc(%q) still contains the tag %q: %q", hostile, bad, got)
		}
	}
	if strings.ContainsAny(got, "<>") {
		t.Errorf("Esc left an angle bracket unescaped: %q", got)
	}
	// The ampersand must be escaped first, or "&" becomes "&amp;amp;" and the
	// member reads their own escaping back.
	if want := `&lt;b&gt;bold&lt;/b&gt; &amp; &lt;i&gt;italic&lt;/i&gt; &lt;/blockquote&gt;&lt;code&gt;`; got != want {
		t.Errorf("Esc(%q)\n = %q\nwant %q", hostile, got, want)
	}
	if back := PlainText(got); back != hostile {
		t.Errorf("PlainText(Esc(x)) = %q, want the original %q", back, hostile)
	}
}

// The marks escape what they wrap. A title is a title, never a way to open a tag
// that the rest of the message finishes.
func TestMarksEscapeTheirContent(t *testing.T) {
	for name, got := range map[string]string{
		"Bold":   Bold(hostile),
		"Italic": Italic(hostile),
		"Code":   Code(hostile),
		"Quote":  Quote(hostile),
		"Strike": Strike(hostile),
	} {
		if n := strings.Count(got, "<"); n != 2 {
			t.Errorf("%s(hostile) = %q: %d angle brackets, want exactly the two of its own tags", name, got, n)
		}
		if PlainText(got) != hostile {
			t.Errorf("%s(hostile) renders as %q, want %q", name, PlainText(got), hostile)
		}
	}
}

func TestPlainTextStripsBeforeUnescaping(t *testing.T) {
	// "&lt;b&gt;" is a member who wrote "<b>". Unescaping first would turn it
	// into a tag and then strip the words they typed.
	if got, want := PlainText("<b>"+Esc("<b>")+"</b>"), "<b>"; got != want {
		t.Errorf("PlainText = %q, want %q", got, want)
	}
	if got, want := PlainText(Bold("salt & pepper")), "salt & pepper"; got != want {
		t.Errorf("PlainText = %q, want %q", got, want)
	}
}

// Splitting formatted text is the failure the external review predicted: a cut
// through a tag or an entity is a 400 from Telegram, not a cosmetic defect.
//
// Every case here fails with the format-blind splitter this replaced.
func TestSplitMessageKeepsFormattingWellFormed(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		limit int
	}{
		{"a bold run wider than the limit", Bold(strings.Repeat("bin day ", 20)), 40},
		{"a quoted body over several breaks", Quote(strings.Repeat("line of the body\n", 30)), 60},
		// No spaces and no newlines, so every cut is a last-resort one and must
		// land between entities rather than inside one. "&am" + "p;" renders as
		// neither an ampersand nor an error.
		{"an entity at the break", Esc(strings.Repeat("a&b", 60)), 20},
		{"tags and prose together", "before " + Bold(strings.Repeat("title ", 30)) + " after", 30},
		{"an escaped hostile title", Bold(hostile) + "\n" + Quote(strings.Repeat(hostile, 4)), 50},
		{"code spans in a refusal", strings.Repeat(Code("monster")+", ", 40), 45},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts := splitMessage(tc.in, tc.limit)
			var rebuilt strings.Builder
			for i, p := range parts {
				if utf16Len(p) > tc.limit {
					t.Errorf("part %d is %d units, over the %d limit: %q", i, utf16Len(p), tc.limit, p)
				}
				if err := wellFormed(p); err != "" {
					t.Errorf("part %d is not well-formed (%s): %q", i, err, p)
				}
				rebuilt.WriteString(PlainText(p))
			}
			// The words survive. strip drops the whitespace a break is allowed to
			// consume, as it does for the unformatted cases in split_test.go; the
			// tags closed and reopened across a cut render as nothing either way.
			if got, want := strip(rebuilt.String()), strip(PlainText(tc.in)); got != want {
				t.Errorf("content changed in splitting\n got %q\nwant %q", got, want)
			}
		})
	}
}

// A cut is never offered inside a tag, inside an entity or inside a rune.
func TestCutStateRefusesUnsafePositions(t *testing.T) {
	// a <b> c </b> &amp; 🔥 — the safe cuts are exactly the boundaries between
	// those six spans, so every other index must be refused.
	spans := []string{"a", "<b>", "c", "</b>", "&amp;", "🔥"}
	s := strings.Join(spans, "")
	safe := map[int]bool{}
	for at, n := 0, 0; n < len(spans); n++ {
		at += len(spans[n])
		safe[at] = true
	}
	for i := 1; i < len(s); i++ {
		if _, ok := cutState(s, i); ok != safe[i] {
			t.Errorf("cutState(%q, %d) = %v, want %v — index %d falls %s a span",
				s, i, ok, safe[i], i, map[bool]string{true: "between", false: "inside"}[safe[i]])
		}
	}
	// The tag stack is reported at the position asked for, not at the end.
	if open, ok := cutState(s, 4); !ok || len(open) != 1 || open[0] != "b" {
		t.Errorf("cutState(%q, 4) = %v, %v; want the bold tag open", s, open, ok)
	}
}

// wellFormed reports why a piece would not survive Telegram's HTML parser, or ""
// if it would. Tags must be balanced and properly nested, and no bare angle
// bracket or half-written entity may be left behind.
func wellFormed(s string) string {
	var stack []string
	for i := 0; i < len(s); {
		switch s[i] {
		case '<':
			end := strings.IndexByte(s[i:], '>')
			if end < 0 {
				return "unterminated tag"
			}
			name := tagName(s[i+1 : i+end])
			if strings.HasPrefix(name, "/") {
				if len(stack) == 0 || stack[len(stack)-1] != name[1:] {
					return "closing tag " + name + " with no matching open"
				}
				stack = stack[:len(stack)-1]
			} else {
				stack = append(stack, name)
			}
			i += end + 1
		case '&':
			end := strings.IndexByte(s[i:], ';')
			if end < 0 || end > maxEntityLen {
				return "bare ampersand"
			}
			i += end + 1
		case '>':
			return "bare closing bracket"
		default:
			i++
		}
	}
	if len(stack) > 0 {
		return "unclosed " + strings.Join(stack, ", ")
	}
	return ""
}

// TestMarkdownRendersWhatTheModelMeant is D1 from the second live run.
//
// The prompt asks for plain prose and the frequency fell, but a member was still
// shown "- **Mains water** — the stopcock" in a turn where they asked for no
// formatting at all. Markdown reaching a member is a defect whether it arrives
// once or six times, so the reply — and only the reply — is converted.
func TestMarkdownRendersWhatTheModelMeant(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"bold", "- **Mains water** — the stopcock",
			"- <b>Mains water</b> — the stopcock"},
		{"italic", "that's *yours* specifically",
			"that's <i>yours</i> specifically"},
		{"inline code", "run `kenward status` first",
			"run <code>kenward status</code> first"},
		{"fence", "like this:\n```sh\nkenward status\n```\nand that is all",
			"like this:\n<pre>kenward status</pre>\nand that is all"},
		{"fence with no info string", "```\na & b\n```",
			"<pre>a &amp; b</pre>"},
		{"heading", "# Bins\nThursday.", "<b>Bins</b>\nThursday."},
		{"markup inside a fence is still literal", "```\n<b>x</b>\n```",
			"<pre>&lt;b&gt;x&lt;/b&gt;</pre>"},
		{"markup inside bold is still literal", "**<b>x</b>**",
			"<b>&lt;b&gt;x&lt;/b&gt;</b>"},
		{"code inside bold", "**the `x` one**", "<b>the <code>x</code> one</b>"},
		// Nothing that is not a pair is touched. A model writing prose about
		// multiplication, a bullet list, or a lone asterisk must reach the member
		// exactly as it wrote them.
		{"arithmetic", "2 * 3 * 4 = 24", "2 * 3 * 4 = 24"},
		{"bullet", "* milk\n* bread", "* milk\n* bread"},
		{"lone asterisk", "a lone * stays", "a lone * stays"},
		{"unclosed fence", "```sh\nnever closed", "```sh\nnever closed"},
		{"unclosed backtick", "a ` stays", "a ` stays"},
		{"escaping still happens", "5 > 3 && true", "5 &gt; 3 &amp;&amp; true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Markdown(tc.in)
			if got != tc.want {
				t.Errorf("Markdown(%q)\n = %q\nwant %q", tc.in, got, tc.want)
			}
			if bad := wellFormed(got); bad != "" {
				t.Errorf("Markdown(%q) = %q: %s", tc.in, got, bad)
			}
		})
	}
}

// TestMemberWrittenTextIsNeverParsed is the other half of D1, and the reason
// conversion was refused the first time round.
//
// Entry titles and bodies are written by members and quoted into kenward's own
// messages. A member whose note says *this* must be shown *this*: the marks escape
// and never parse, and nothing that has been through one of them is ever handed to
// Markdown.
func TestMemberWrittenTextIsNeverParsed(t *testing.T) {
	body := "she wrote *this* and **that**, in `code`, with a ``` fence"
	for name, got := range map[string]string{
		"Quote":  Quote(body),
		"Bold":   Bold(body),
		"Code":   Code(body),
		"Strike": Strike(body),
	} {
		if PlainText(got) != body {
			t.Errorf("%s(body) renders as %q, want the member's own %q", name, PlainText(got), body)
		}
		for _, tag := range []string{"<b>", "<i>", "<pre>", "<code>"} {
			// One of its own opening tag is allowed; a second is markup the
			// member's asterisks were parsed into.
			if strings.Count(got, tag) > 1 {
				t.Errorf("%s(body) = %q: member text was parsed into %s", name, got, tag)
			}
		}
	}
}
