package enrol

import (
	"os"
	"strings"
	"testing"
)

func TestAlphabetIsUnambiguous(t *testing.T) {
	if got := len(Alphabet); got != 32 {
		t.Fatalf("alphabet has %d symbols, want 32 (five bits each)", got)
	}
	for _, bad := range "ILOU" {
		if strings.ContainsRune(Alphabet, bad) {
			t.Errorf("alphabet contains ambiguous symbol %q", bad)
		}
	}
	seen := map[rune]bool{}
	for _, r := range Alphabet {
		if seen[r] {
			t.Errorf("alphabet repeats %q", r)
		}
		seen[r] = true
	}
	if CodeEntropyBits != 80 {
		t.Errorf("CodeEntropyBits = %d, want 80", CodeEntropyBits)
	}
}

func TestGenerateCode(t *testing.T) {
	const n = 2000
	seen := make(map[string]bool, n)
	for i := 0; i < n; i++ {
		c, err := generateCode()
		if err != nil {
			t.Fatalf("generateCode: %v", err)
		}
		if len(c) != CodeSymbols {
			t.Fatalf("code %q has %d symbols, want %d", c, len(c), CodeSymbols)
		}
		for _, r := range c {
			if !strings.ContainsRune(Alphabet, r) {
				t.Fatalf("code %q contains %q, outside the alphabet", c, r)
			}
		}
		if seen[c] {
			t.Fatalf("generateCode repeated %q within %d draws", c, n)
		}
		seen[c] = true
	}
}

func TestFormatAndNormalizeRoundTrip(t *testing.T) {
	code, err := generateCode()
	if err != nil {
		t.Fatalf("generateCode: %v", err)
	}
	formatted := Format(code)
	if want := CodeSymbols + CodeSymbols/codeGroup - 1; len(formatted) != want {
		t.Fatalf("Format(%q) = %q, length %d, want %d", code, formatted, len(formatted), want)
	}
	if got := Normalize(formatted); got != code {
		t.Fatalf("Normalize(Format(%q)) = %q", code, got)
	}
	if got := Normalize(strings.ToLower(formatted)); got != code {
		t.Fatalf("lower-cased code did not normalize back: %q", got)
	}
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"hyphens dropped", "ABCD-EFGH", "ABCDEFGH"},
		{"spaces dropped", "ABCD EFGH", "ABCDEFGH"},
		{"lower cased", "abcd-efgh", "ABCDEFGH"},
		{"letter i reads as one", "AiBI", "A1B1"},
		{"letter l reads as one", "AlBL", "A1B1"},
		{"letter o reads as zero", "AoBO", "A0B0"},
		{"unknown symbols dropped, not substituted", "AB!?CD", "ABCD"},
		{"u is not in the alphabet", "ABUCD", "ABCD"},
		{"empty", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := Normalize(tc.in); got != tc.want {
				t.Errorf("Normalize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestExtract(t *testing.T) {
	code := "ABCDEFGHJKMNPQRS"
	tests := []struct {
		name string
		text string
		want string
		ok   bool
	}{
		{"bare code", code, code, true},
		{"formatted code", Format(code), code, true},
		{"after a start command", "/start " + Format(code), code, true},
		{"lower case with prose", "hi here it is: " + strings.ToLower(Format(code)), code, true},
		{"plain greeting", "hello?", "", false},
		{"long prose is never code shaped", strings.Repeat("hello there friend ", 20), "", false},
		{"too short", "ABCD-EFGH", "", false},
		{"too long", Format(code) + "XY", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := extract(tc.text)
			if ok != tc.ok || got != tc.want {
				t.Errorf("extract(%q) = %q, %v; want %q, %v", tc.text, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestHashIsDeterministicAndNormalizing(t *testing.T) {
	code := "ABCDEFGHJKMNPQRS"
	a, err := hash(code, testIters)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	b, err := hash(strings.ToLower(Format(code)), testIters)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if a != b {
		t.Errorf("hash is not stable across formatting: %q vs %q", a, b)
	}
	if len(a) != hashLen*2 {
		t.Errorf("hash is %d chars, want %d", len(a), hashLen*2)
	}
	if strings.Contains(a, code) {
		t.Errorf("hash %q leaks the plaintext", a)
	}
	c, err := hash("ABCDEFGHJKMNPQRT", testIters)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if a == c {
		t.Error("different codes hashed to the same digest")
	}
	// Work factor is baked into the digest; a different one must not verify.
	d, err := hash(code, testIters+1)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if a == d {
		t.Error("work factor does not affect the digest")
	}
}

func TestEqualHash(t *testing.T) {
	a := strings.Repeat("a", hashLen*2)
	tests := []struct {
		name string
		x, y string
		want bool
	}{
		{"identical", a, a, true},
		{"differs in last char", a, strings.Repeat("a", hashLen*2-1) + "b", false},
		{"differs in first char", a, "b" + strings.Repeat("a", hashLen*2-1), false},
		{"different lengths", a, a[:10], false},
		{"both empty", "", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EqualHash(tc.x, tc.y); got != tc.want {
				t.Errorf("EqualHash(%q, %q) = %v, want %v", tc.x, tc.y, got, tc.want)
			}
		})
	}
}

// TestComparisonIsConstantTime asserts, at the source level, that digests are
// compared through crypto/subtle and never with == or a map lookup.
//
// A timing measurement would be the direct test and would also be flaky on a shared
// CI machine, so this checks the property that actually matters and is stable: that
// no code path in the package compares a stored digest by any other means. A map
// keyed by digest would be the tempting mistake — it is faster, and it leaks.
func TestComparisonIsConstantTime(t *testing.T) {
	src, err := os.ReadFile("code.go")
	if err != nil {
		t.Fatalf("read code.go: %v", err)
	}
	for _, want := range []string{`"crypto/subtle"`, "subtle.ConstantTimeCompare"} {
		if !strings.Contains(string(src), want) {
			t.Errorf("code.go does not contain %q; EqualHash must use crypto/subtle", want)
		}
	}

	for _, name := range []string{"store.go", "filestore.go", "enrol.go"} {
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		body := string(b)
		for _, bad := range []string{"map[string]Code", ".Hash ==", ".Hash !=", "== digest", "== c.Hash"} {
			if strings.Contains(body, bad) {
				t.Errorf("%s contains %q: digests must be compared with EqualHash", name, bad)
			}
		}
	}
}
