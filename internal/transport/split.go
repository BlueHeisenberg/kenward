package transport

import (
	"strings"
	"unicode/utf8"
)

// defaultMaxMessageLen is Telegram's limit for one text message, counted in
// UTF-16 code units as the API counts it.
const defaultMaxMessageLen = 4096

// utf16Len returns the length of s in UTF-16 code units, which is the unit
// Telegram measures messages in. Anything outside the basic multilingual plane —
// most emoji — counts as two.
func utf16Len(s string) int {
	n := 0
	for _, r := range s {
		if r > 0xFFFF {
			n += 2
		} else {
			n++
		}
	}
	return n
}

// prefixEnd returns the byte index just past the longest prefix of s that fits
// in limit UTF-16 code units, always on a rune boundary.
func prefixEnd(s string, limit int) int {
	n := 0
	for i, r := range s {
		w := 1
		if r > 0xFFFF {
			w = 2
		}
		if n+w > limit {
			return i
		}
		n += w
	}
	return len(s)
}

// splitMessage breaks s into pieces that each fit within limit, preferring to
// break where the text already breaks: a blank line first, then a line ending,
// then a space, and only as a last resort mid-word on a rune boundary.
//
// Nothing is ever dropped except the whitespace at the break itself. A reply too
// long for one message arrives as several, in order — it is never truncated,
// because a silently shortened answer is worse than a long one.
//
// s is Telegram HTML, so a piece has to be well-formed on its own and not merely
// short enough. A cut never lands inside a tag, inside a character entity or
// inside a rune, and any tags open across it are closed at the end of the piece
// and reopened at the head of the next — see cutState, which is where a parse
// mode turns a formatting question into a delivery one. Splitting blind was safe
// only while nothing was formatted; the day a parse mode was set it became a 400
// on every reply long enough to need two messages, which is to say on exactly the
// replies a member most wants to receive.
//
// A limit below 2 is raised to 2: a single rune can be two UTF-16 units wide and
// content is never dropped, so no smaller limit can be honoured. Every emitted
// piece fits within the effective limit, closing tags included.
func splitMessage(s string, limit int) []string {
	if limit <= 0 {
		limit = defaultMaxMessageLen
	}
	if limit < 2 {
		limit = 2
	}

	var out []string
	for {
		if utf16Len(s) <= limit {
			if strings.TrimSpace(PlainText(s)) != "" {
				out = append(out, s)
			}
			return out
		}

		// The closing tags appended below are part of the piece and count against
		// the limit, so the budget the cut is chosen within is reduced by the most
		// they could come to anywhere in reach. On unformatted text this reserves
		// nothing and the budget is the limit exactly.
		end := prefixEnd(s, limit)
		if reserve := maxClosersWithin(s, end); reserve > 0 {
			budget := limit - reserve
			if budget < 2 {
				budget = 2
			}
			end = prefixEnd(s, budget)
		}
		if end == 0 {
			// A single rune wider than the limit. Emit it rather than spin.
			_, size := utf8.DecodeRuneInString(s)
			end = size
		}

		cut, next := breakPoint(s, end)
		open, _ := cutState(s, cut)
		chunk := strings.TrimRight(s[:cut], " \t\n") + closeAll(open)
		if strings.TrimSpace(PlainText(chunk)) != "" {
			out = append(out, chunk)
		}
		s = openAll(open) + s[next:]
	}
}

// breakPoint chooses where to cut s given that at most end bytes fit. It returns
// the end of this piece and the start of the next; the gap between them is the
// separator that gets consumed.
//
// Only positions scanCuts vouched for are considered, so the preference order —
// blank line, line ending, space, anywhere — is a preference among legal cuts
// rather than among textual ones. A space inside a tag is not a place to break a
// message, however much it looks like one.
func breakPoint(s string, end int) (cut, next int) {
	c := scanCuts(s, end)
	if c.para > 0 {
		return c.para, skipNewlines(s, c.para)
	}
	if c.line > 0 {
		return c.line, skipNewlines(s, c.line)
	}
	if c.word > 0 {
		return c.word, c.word + 1
	}
	return c.any, c.any
}

// maxClosersWithin returns the widest run of closing tags that could be needed
// for a cut at or before end: the deepest the tag stack gets within reach,
// measured as the text that closes it.
func maxClosersWithin(s string, end int) int {
	if !strings.Contains(s[:min(end+1, len(s))], "<") {
		return 0
	}
	var open []string
	widest := 0
	for j := 0; j < end && j < len(s); {
		if s[j] != '<' {
			_, size := utf8.DecodeRuneInString(s[j:])
			j += size
			continue
		}
		n := strings.IndexByte(s[j:], '>')
		if n < 0 {
			break
		}
		switch name := tagName(s[j+1 : j+n]); {
		case name == "":
		case name[0] == '/':
			if k := len(open); k > 0 {
				open = open[:k-1]
			}
		default:
			open = append(open, name)
			if w := utf16Len(closeAll(open)); w > widest {
				widest = w
			}
		}
		j += n + 1
	}
	return widest
}

// skipNewlines returns the index of the first byte at or after i that is not a
// newline, so a paragraph break is consumed whole rather than left dangling at
// the head of the next piece.
func skipNewlines(s string, i int) int {
	for i < len(s) && s[i] == '\n' {
		i++
	}
	return i
}
