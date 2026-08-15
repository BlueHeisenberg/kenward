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
func splitMessage(s string, limit int) []string {
	if limit <= 0 {
		limit = defaultMaxMessageLen
	}

	var out []string
	for {
		if utf16Len(s) <= limit {
			if strings.TrimSpace(s) != "" {
				out = append(out, s)
			}
			return out
		}

		end := prefixEnd(s, limit)
		if end == 0 {
			// A single rune wider than the limit. Emit it rather than spin.
			_, size := utf8.DecodeRuneInString(s)
			end = size
		}

		cut, next := breakPoint(s, end)
		chunk := strings.TrimRight(s[:cut], " \t\n")
		if strings.TrimSpace(chunk) != "" {
			out = append(out, chunk)
		}
		s = s[next:]
	}
}

// breakPoint chooses where to cut s given that at most end bytes fit. It returns
// the end of this piece and the start of the next; the gap between them is the
// separator that gets consumed.
func breakPoint(s string, end int) (cut, next int) {
	if i := strings.LastIndex(s[:end], "\n\n"); i > 0 {
		return i, skipNewlines(s, i)
	}
	if i := strings.LastIndex(s[:end], "\n"); i > 0 {
		return i, skipNewlines(s, i)
	}
	if i := strings.LastIndex(s[:end], " "); i > 0 {
		return i, i + 1
	}
	return end, end
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
