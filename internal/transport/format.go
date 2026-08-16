package transport

import (
	"html"
	"strings"
	"unicode/utf8"

	"github.com/go-telegram/bot/models"
)

// Everything kenward sends is Telegram HTML, and this file is the whole of what
// that means: how text is escaped, how the four structural marks are written, and
// how a formatted message is turned back into the words it renders as.
//
// HTML rather than MarkdownV2, for one reason that is not taste. MarkdownV2
// requires eighteen characters to be backslash-escaped everywhere they appear —
// `_*[]()~`>#+-=|{}.!` — a missed one is not a cosmetic defect but a 400 from
// Telegram and a message the member never sees. Telegram's HTML needs `&`, `<`
// and `>` escaped and nothing else — three characters, one strings.Replacer, and
// html.UnescapeString in the standard library to undo it. The parse mode with the
// smaller escaping surface is the one whose escaping cannot be missed.
//
// Hermes chose the other way and paid for it in full: _escape_mdv2 with its
// eighteen-character regex, _strip_mdv2 approximating the inverse with seven more
// regexes, and a 170-line format_message that has to hide code spans behind
// placeholder tokens so the escaper cannot reach into them
// (hermes-agent/plugins/platforms/telegram/adapter.py:463-492, :8263-8433). Its
// own business plugin declines the whole business — "Telegram MarkdownV2 escaping
// is fragile" — and sends plain text (hermes-telegram-business/manager.py:66-68).
// Where Hermes most needs a prompt to render correctly, its command-approval
// gate, it switches to HTML (adapter.py:6096, :6155).
//
// Escaping is not decoration. Entry titles, entry bodies, member names and model
// output are all written by somebody other than kenward, and a member who titles
// a note "<b>" must get a note titled "<b>" rather than a message whose
// remainder is bold. See prompt.go's oneLine for the same rule one layer up: text
// from outside must not be able to forge the structure it sits in.
const parseMode = models.ParseModeHTML

// escaper is the whole of what Telegram's HTML mode requires.
//
// html.EscapeString would also serve — it is a superset — but it renders every
// apostrophe as &#39;, which is five characters against a 4096-unit budget for
// each contraction in a message written in English, and turns every golden file
// and every log line into something that has to be decoded before it can be read.
// Telegram renders a bare ' and a bare " as themselves. The inverse is still
// html.UnescapeString, which reads this and more.
var escaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// Esc renders s as literal text inside a formatted message.
//
// Every value that kenward did not write itself goes through this, or through one
// of the marks below, which apply it.
func Esc(s string) string { return escaper.Replace(s) }

// The four marks. There are four because there are four kinds of thing in
// kenward's messages that are not prose: a title, a quoted body, a name the
// member could type back, and the node annotating itself. Each takes raw text
// and escapes it — there is deliberately no way to pass markup through.

// Bold marks a title: the thing the message is about.
func Bold(s string) string { return "<b>" + Esc(s) + "</b>" }

// Italic marks the node speaking about itself rather than to the member — a
// retrieval line, the outcome appended to a spent question.
func Italic(s string) string { return "<i>" + Esc(s) + "</i>" }

// Code marks an identifier: a tier, a machine, a space, an enrolment code.
// Something that is a name rather than a word, and that a member may need to
// type or compare character by character.
func Code(s string) string { return "<code>" + Esc(s) + "</code>" }

// Quote marks stored words being shown back — an entry's body, quoted so it is
// visibly the entry and not kenward's own sentence.
func Quote(s string) string { return "<blockquote>" + Esc(s) + "</blockquote>" }

// The glyph vocabulary.
//
// Six glyphs, each marking what kind of message this is, and that is the whole
// of the emoji policy. A member scrolling a chat has to be able to tell a
// question from a report of something already done, and a memory that landed
// from one that did not, without reading a word — which is exactly the
// distinction that matters most in a product whose central promise is that you
// always know what it wrote.
//
// This is emoji as structure, not as personality, and the difference is the line
// docs/PROMPT.md draws when it says there is no emoji policy and no persona. That
// paragraph governs the model's register: the assistant does not perform a
// character, and nothing here puts a glyph in the model's prose. These mark the
// node's own announcements, which are not the assistant talking at all.
//
// The vocabulary is Hermes', narrowed. Hermes resolves an emoji per tool from a
// registry — tools/memory_tool.py:1262 registers emoji="🧠",
// tools/clarify_tool.py:318 registers emoji="❓", tools/web_tools.py:1227
// registers emoji="🔍" — and composes "{emoji} {verb} {object}"
// (gateway/run.py:4270-4277). kenward has six kinds of message rather than forty
// tools, so the table is six lines long and lives in one place; Hermes' is spread
// across forty registration calls, one of which registers the literal string
// "video" in place of a glyph (tools/xai_video_tools.py:197).
//
// What was left behind: Hermes' KAWAII_WAITING faces, THINKING_VERBS and spinner
// frames (agent/display.py:1086-1113), and its skin system of per-user emoji
// overrides (hermes_cli/skin_engine.py:107-111). Those are personality, they are
// what PROMPT.md is refusing, and a household assistant that performs a character
// is exhausting by week two.
const (
	// GlyphMemory marks something that is now in memory.
	GlyphMemory = "🧠"
	// GlyphAsk marks a decision being put to the member. It is the difference
	// between "I will" and "I did", and it is the one confusion this product
	// cannot afford.
	GlyphAsk = "❓"
	// GlyphGone marks something taken back. Hermes' settled-draft header uses
	// the same mark: "✕ Discarded" (hermes-telegram-business/manager.py:681).
	GlyphGone = "✕"
	// GlyphHousehold marks the shared memory — what everyone can see.
	GlyphHousehold = "🏠"
	// GlyphRead marks the node reporting what it read this turn.
	GlyphRead = "🔍"
	// GlyphProblem marks a turn that produced nothing and says why. Hermes marks
	// every error and its command-approval header the same way:
	// _EA_HEADER = "⚠️ <b>Command Approval Required</b>"
	// (plugins/platforms/telegram/adapter.py:6096).
	GlyphProblem = "⚠️"
)

// PlainText renders formatted text as the words it displays: tags removed,
// entities resolved.
//
// It has two jobs and they are the same job. It is what a rejected message is
// re-sent as, so a formatting fault costs the member their styling and never
// their message; and it is how the splitter decides whether a piece has any
// content in it, since a piece holding nothing but reopened tags is empty to a
// reader and a 400 to Telegram.
//
// Tags are stripped before entities are resolved, never after: "&lt;b&gt;" is a
// member who wrote "<b>", and unescaping first would strip the words they typed.
func PlainText(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>' && inTag:
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return html.UnescapeString(b.String())
}

// --- splitting formatted text ----------------------------------------------

// maxEntityLen bounds how far past an '&' a ';' may sit and still be a character
// entity. Telegram's own vocabulary is &amp; &lt; &gt; &quot; and &#39;; the
// bound exists so that an ampersand in ordinary prose — "salt & pepper, and
// then; …" — is treated as the literal it is rather than swallowing the sentence.
const maxEntityLen = 10

// cutState reports the tags left open at byte index i and whether a cut there is
// legal — outside every tag, outside every character entity, and on a rune
// boundary.
//
// This is the whole of what a parse mode costs the splitter. Cutting "<b>bin
// day</b>" at the space yields "<b>bin" and "day</b>", and Telegram answers both
// with a 400 rather than rendering either; cutting "&amp;" in the middle yields
// "&am" and "p;", which renders as neither an ampersand nor an error. So a cut
// must land outside both, and the tags still open across it are closed at the end
// of one piece and reopened at the start of the next.
func cutState(s string, i int) (open []string, ok bool) {
	for j := 0; j < i; {
		switch s[j] {
		case '<':
			end := strings.IndexByte(s[j:], '>')
			if end < 0 || i <= j+end {
				return open, false // the cut falls inside this tag
			}
			end += j
			switch name := tagName(s[j+1 : end]); {
			case name == "":
			case name[0] == '/':
				if n := len(open); n > 0 {
					open = open[:n-1]
				}
			default:
				open = append(open, name)
			}
			j = end + 1

		case '&':
			end := strings.IndexByte(s[j:], ';')
			if end < 0 || end > maxEntityLen {
				j++ // a bare ampersand, not an entity
				continue
			}
			if i <= j+end {
				return open, false // the cut falls inside this entity
			}
			j += end + 1

		default:
			_, size := utf8.DecodeRuneInString(s[j:])
			if i < j+size {
				return open, false // the cut falls inside this rune
			}
			j += size
		}
	}
	return open, true
}

// tagName is the element name in a tag body, without its attributes and without
// the trailing slash of a self-closing tag. "/b" keeps its slash: the caller
// distinguishes an opening tag from a closing one by it.
func tagName(body string) string {
	name, _, _ := strings.Cut(strings.TrimSuffix(strings.TrimSpace(body), "/"), " ")
	return name
}

// closeAll writes the closing tags for open, innermost first.
func closeAll(open []string) string {
	var b strings.Builder
	for i := len(open) - 1; i >= 0; i-- {
		b.WriteString("</")
		b.WriteString(open[i])
		b.WriteString(">")
	}
	return b.String()
}

// openAll writes the opening tags for open, outermost first.
func openAll(open []string) string {
	var b strings.Builder
	for _, t := range open {
		b.WriteString("<")
		b.WriteString(t)
		b.WriteString(">")
	}
	return b.String()
}

// cuts are the last legal cut positions of each kind at or before a budget: after
// a blank line, after a line ending, at a space, and anywhere at all. Gathered in
// one pass so that choosing a break point does not rescan the piece once per
// candidate.
type cuts struct{ para, line, word, any int }

// scanCuts walks s to the byte budget end and records where a cut may legally
// land. Positions inside a tag, inside an entity or inside a rune are never
// offered. Index zero is never offered either: cutting there would emit an empty
// piece and make no progress.
func scanCuts(s string, end int) cuts {
	c := cuts{para: -1, line: -1, word: -1, any: 0}
	for j := 0; j <= end && j < len(s); {
		if j > 0 {
			c.any = j
			switch {
			case strings.HasPrefix(s[j:], "\n\n"):
				c.para = j
			case s[j] == '\n':
				c.line = j
			case s[j] == ' ':
				c.word = j
			}
		}
		switch s[j] {
		case '<':
			n := strings.IndexByte(s[j:], '>')
			if n < 0 {
				return c
			}
			j += n + 1
		case '&':
			n := strings.IndexByte(s[j:], ';')
			if n < 0 || n > maxEntityLen {
				j++
				continue
			}
			j += n + 1
		default:
			_, size := utf8.DecodeRuneInString(s[j:])
			j += size
		}
	}
	return c
}
