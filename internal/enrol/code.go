package enrol

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strings"
)

// Alphabet is the symbol set claim codes are drawn from: Crockford's Base32.
//
// It is the digits and the upper-case letters with I, L, O and U removed. I and L
// are dropped because they are read back as 1, O because it is read back as 0, and
// U because its absence keeps generated codes free of the more obvious accidental
// obscenities. A code has to survive being read aloud across a kitchen and retyped
// on a phone keyboard, so the ambiguity matters more than the four lost symbols.
//
// Normalize folds the omitted letters back onto the symbols they are mistaken for,
// so a member who types "l" where the printed code said "1" is still let in.
const Alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// CodeSymbols is the number of alphabet symbols in a claim code, before grouping.
const CodeSymbols = 16

// CodeEntropyBits is the entropy of a minted code, in bits.
//
// Each symbol carries exactly five bits and every symbol is drawn from crypto/rand
// with no modulo bias, so the figure is exact rather than an estimate: 16 x 5 = 80.
// Eighty bits is far beyond guessing at five attempts an hour — an attacker averages
// 2^79 tries, and codes expire in a day — while sixteen characters is still short
// enough to dictate over the phone.
const CodeEntropyBits = CodeSymbols * 5

// codeGroup is how many symbols appear between hyphens in a formatted code.
const codeGroup = 4

// kdfSalt is the fixed, public domain-separation salt for claim-code hashing.
//
// It is deliberately not per-code random. Store.Consume looks a code up *by* its
// hash, so the digest has to be computable from the plaintext alone; a per-record
// salt would force a scan that rehashes every stored code on every attempt. The
// usual reasons for a random salt do not apply here anyway: the codes are 80-bit
// crypto/rand values, so there is no dictionary to precompute against and no two
// installations can collide.
const kdfSalt = "kenward/enrol/claim-code/v1"

// kdfIterations is the PBKDF2-HMAC-SHA256 work factor for claim codes.
//
// Honest reasoning, because the choice is arguable either way. The actual defence
// here is the 80 bits of entropy in the code plus the five-attempts-per-hour limit,
// not the cost of the hash: a fast SHA-256 over an 80-bit random value is already
// out of brute-force reach, and a work factor buys a linear multiplier against an
// exponent. PBKDF2 is used regardless because it costs nothing that matters — a
// claim is a once-per-member event, bounded to five attempts an hour — and because
// it means the file of hashes stays useless if a future change ever shortens the
// code for usability. It is defence in depth over a defence that already holds.
//
// This value is baked into the stored digests. Codes minted under one work factor
// cannot be redeemed under another, so changing it invalidates every outstanding
// code; that is a deliberate migration, not a tuning knob.
const kdfIterations = 210_000

// hashLen is the PBKDF2 output length in bytes.
const hashLen = 32

// generateCode returns CodeSymbols alphabet symbols of crypto/rand entropy.
//
// The bytes are consumed five bits at a time. CodeSymbols*5 is a whole number of
// bytes, so every bit read is used and no symbol is chosen by a modulo, which is
// what makes CodeEntropyBits exact.
func generateCode() (string, error) {
	buf := make([]byte, CodeEntropyBits/8)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("enrol: read entropy: %w", err)
	}
	var (
		out  strings.Builder
		acc  uint32
		bits uint
	)
	out.Grow(CodeSymbols)
	for _, b := range buf {
		acc = acc<<8 | uint32(b)
		bits += 8
		for bits >= 5 {
			bits -= 5
			out.WriteByte(Alphabet[(acc>>bits)&0x1f])
		}
	}
	return out.String(), nil
}

// Format groups a normalized code into hyphenated blocks for printing. The hyphens
// are cosmetic; Normalize removes them again on the way back in.
func Format(code string) string {
	var out strings.Builder
	for i, r := range code {
		if i > 0 && i%codeGroup == 0 {
			out.WriteByte('-')
		}
		out.WriteRune(r)
	}
	return out.String()
}

// Normalize folds a typed or pasted code back to its canonical symbols.
//
// It upper-cases, maps the omitted letters onto their look-alikes (I and L to 1,
// O to 0) and discards everything else, so hyphens, spaces and a leading "/start"
// all fall away. A character that is neither in the alphabet nor a known look-alike
// is dropped rather than replaced, which shortens the result and makes it fail the
// length check instead of silently becoming some other valid code.
func Normalize(s string) string {
	var out strings.Builder
	out.Grow(len(s))
	for _, r := range strings.ToUpper(s) {
		switch r {
		case 'I', 'L':
			out.WriteByte('1')
		case 'O':
			out.WriteByte('0')
		default:
			if strings.ContainsRune(Alphabet, r) {
				out.WriteRune(r)
			}
		}
	}
	return out.String()
}

// extract finds a code-shaped token in a message.
//
// Tokens are split on whitespace and normalized individually. Normalizing the whole
// message instead would let a long enough piece of ordinary prose collapse into
// something the right length, so the scan is per token and requires an exact match
// on CodeSymbols.
func extract(text string) (string, bool) {
	for _, tok := range strings.Fields(text) {
		if n := Normalize(tok); len(n) == CodeSymbols {
			return n, true
		}
	}
	return "", false
}

// hash derives the stored digest of a code. The plaintext is normalized first, so
// the digest is identical however the member typed it.
func hash(plaintext string, iters int) (string, error) {
	key, err := pbkdf2.Key(sha256.New, Normalize(plaintext), []byte(kdfSalt), iters, hashLen)
	if err != nil {
		return "", fmt.Errorf("enrol: derive code hash: %w", err)
	}
	return hex.EncodeToString(key), nil
}

// EqualHash compares two stored code digests in constant time.
//
// Every Store implementation must use this rather than == or a map lookup. Both
// leak, through timing, how much of a guessed code's digest was right, which turns
// an 80-bit search into a symbol-at-a-time one. Digests are fixed length here, so
// the length check inside ConstantTimeCompare reveals nothing.
func EqualHash(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
