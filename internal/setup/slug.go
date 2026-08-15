package setup

import (
	"fmt"
	"strings"
	"unicode"
)

// Slugify turns a person's name into the id kenward will use for them everywhere
// else: in the configuration, in the state file, on the `kenward run --member`
// command line and in log lines.
//
// The id is derived rather than asked for. Asking somebody to invent a machine
// identifier for their own daughter is the kind of question that makes a setup
// wizard feel like paperwork, and the answer is nearly always the name in
// lower-case anyway.
//
// Letters with diacritics fold to their unaccented forms instead of being dropped.
// "José" becoming "jos" would be a small daily insult in a program that is supposed
// to be a member of the household, and dropping the accent is what everyone does
// already when they type their own name into a login box.
func Slugify(name string) string {
	var b strings.Builder
	lastHyphen := true // leading hyphens are suppressed
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		if folded, ok := foldedLetters[r]; ok {
			r = folded
		}
		switch {
		case r < unicode.MaxASCII && (unicode.IsLetter(r) || unicode.IsDigit(r)):
			b.WriteRune(r)
			lastHyphen = false
		case !lastHyphen:
			b.WriteByte('-')
			lastHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// foldedLetters maps the accented Latin letters a European household's names
// actually contain onto ASCII. It is a table rather than a Unicode normalisation
// pass because the whole of what is needed is a few dozen letters, and pulling in a
// text-normalisation dependency to spell "Müller" is not a trade worth making.
var foldedLetters = map[rune]rune{
	'á': 'a', 'à': 'a', 'â': 'a', 'ä': 'a', 'ã': 'a', 'å': 'a', 'ā': 'a',
	'é': 'e', 'è': 'e', 'ê': 'e', 'ë': 'e', 'ē': 'e', 'ė': 'e',
	'í': 'i', 'ì': 'i', 'î': 'i', 'ï': 'i', 'ī': 'i',
	'ó': 'o', 'ò': 'o', 'ô': 'o', 'ö': 'o', 'õ': 'o', 'ø': 'o', 'ō': 'o',
	'ú': 'u', 'ù': 'u', 'û': 'u', 'ü': 'u', 'ū': 'u',
	'ñ': 'n', 'ń': 'n', 'ç': 'c', 'ć': 'c', 'č': 'c', 'ł': 'l',
	'ś': 's', 'š': 's', 'ż': 'z', 'ź': 'z', 'ž': 'z', 'ý': 'y', 'ÿ': 'y',
	'đ': 'd', 'ð': 'd', 'þ': 't', 'ß': 's',
}

// uniqueSlug returns a slug for name that is not already in taken, and records it.
//
// Two people in one house can share a first name — a father and a son usually do —
// so the second one gets a numbered id rather than a collision or an error. The
// operator sees the result before anything is written and can rename either of them
// by typing a fuller name.
func uniqueSlug(name string, taken map[string]bool, fallback string) string {
	base := Slugify(name)
	if base == "" {
		// A name written entirely in a script this table does not cover still has
		// to produce a usable id. It is a machine identifier; the person keeps
		// their name, which is stored beside it exactly as they typed it.
		base = fallback
	}
	candidate := base
	for n := 2; taken[candidate]; n++ {
		candidate = fmt.Sprintf("%s-%d", base, n)
	}
	taken[candidate] = true
	return candidate
}

// privateSpaceFor names a member's own two-member lore space: them, and the node.
//
// The suffix is what makes it obvious in a lore listing which space belongs to a
// person and which is the household's. It is checked against the shared space, and
// against the other members', by the caller.
func privateSpaceFor(id string) string { return id + "-private" }

// envVarFor names the environment variable holding one member's bot token in
// isolated mode.
//
// Upper snake case, prefixed, and suffixed with the member's id, so that a shell
// with several households' variables in it stays readable and `kenward doctor`'s
// list of what is missing names who it belongs to.
func envVarFor(prefix, id string) string {
	return prefix + "_" + strings.ToUpper(strings.ReplaceAll(id, "-", "_"))
}

// apiKeyEnvFor suggests the variable an endpoint's API key is read from, following
// the convention every provider's own documentation already uses.
func apiKeyEnvFor(endpointName string) string {
	id := Slugify(endpointName)
	if id == "" {
		return "KENWARD_API_KEY"
	}
	return strings.ToUpper(strings.ReplaceAll(id, "-", "_")) + "_API_KEY"
}
