package setup

import (
	"crypto/rand"
	"crypto/subtle"
	"fmt"
	"strings"
)

// A share code is the grant that lets somebody else's vta_only session connect
// to a full_stack. Design: docs/custom-stack-connection-design.md §4.1.
//
// Crockford base32 rather than raw base32 because this code is meant to survive
// being read aloud and retyped, not only pasted: it excludes I, L, O and U, and
// defines how the glyphs people confuse anyway should be folded back. A format
// for oral transmission without a check symbol is half-designed, so the last
// character is one.
//
// The check symbol is not security — an attacker computes it as easily as we
// do. It exists so the overwhelmingly common failure, a hand-copied character,
// is diagnosed as itself rather than landing in the same answer as "the owner
// rotated this code".
const (
	// crockfordAlphabet is the canonical 32-symbol set: digits, then A–Z minus
	// I, L, O and U.
	crockfordAlphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	// crockfordCheckAlphabet extends it with the five check-only symbols, so a
	// check value of 32–36 has a representation the data alphabet cannot
	// produce. Standard Crockford.
	crockfordCheckAlphabet = crockfordAlphabet + "*~$=U"

	// shareCodeDataLen is the number of random symbols before the check symbol.
	// 15 × 5 bits = 75 bits, which is not brute-forceable through an
	// authenticated, rate-limited route.
	shareCodeDataLen = 15
	// shareCodeGroup is the display grouping — K7M2-9XQP-4B8W-3NRT.
	shareCodeGroup = 4
)

// NewShareCode mints a share code, formatted for display. Every code it returns
// is 16 alphanumerics grouped in fours.
//
// One byte per symbol masked to 5 bits, rather than arithmetic on a larger
// word: simpler to see as unbiased, and this runs once per share.
//
// The loop is what keeps a minted code alphanumeric. Crockford's check alphabet
// carries five extra symbols — `*~$=U` — for remainders 32–36, so 5/37 of
// otherwise fine codes end in punctuation. Such a code is valid and always will
// be (ValidateShareCode still accepts them, and codes minted before this loop
// existed keep working), but it is unreadable down a phone and awkward on
// keyboards that bury those glyphs, which is the entire reason this format was
// chosen over raw base32. Rerolling costs 37/32 ≈ 1.16 attempts on average and
// 0.2 bits of the 75.
func NewShareCode() (string, error) {
	for {
		buf := make([]byte, shareCodeDataLen)
		if _, err := rand.Read(buf); err != nil {
			return "", fmt.Errorf("generate share code: %w", err)
		}

		data := make([]byte, shareCodeDataLen)
		for i, b := range buf {
			data[i] = crockfordAlphabet[b&0x1f]
		}

		check := crockfordCheckSymbol(data)
		if strings.IndexByte(crockfordAlphabet, check) < 0 {
			continue
		}
		return GroupShareCode(string(data) + string(check)), nil
	}
}

// GroupShareCode inserts the display dashes. Purely cosmetic — every comparison
// runs on the normalised form, which has none.
func GroupShareCode(code string) string {
	var b strings.Builder
	for i, r := range code {
		if i > 0 && i%shareCodeGroup == 0 {
			b.WriteByte('-')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// NormalizeShareCode folds a code to its canonical comparable form: dashes and
// whitespace removed, uppercased, and the ambiguous glyphs mapped the way
// Crockford specifies — I and L to 1, O to 0.
//
// It does NOT validate. A string that could not possibly be a share code
// normalises to something that will simply fail to match, which is the correct
// outcome for a credential comparison; ValidateShareCode is what turns a
// mistyped code into its own diagnosis.
func NormalizeShareCode(code string) string {
	var b strings.Builder
	b.Grow(len(code))
	for _, r := range strings.ToUpper(code) {
		switch r {
		case '-', ' ', '\t', '\n', '\r':
			// Grouping and whatever whitespace survived a copy-paste.
		case 'I', 'L':
			b.WriteByte('1')
		case 'O':
			b.WriteByte('0')
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// ValidateShareCode reports whether code is well-formed: right length, only
// symbols from the alphabet, and a check symbol that matches its data.
//
// This is what lets "you mistyped this" be a different answer from "this code
// does not open anything here" — two problems needing two different actions
// from whoever is holding the code. Both would otherwise collapse into the
// second, which is the vaguest message in the flow.
func ValidateShareCode(code string) error {
	n := NormalizeShareCode(code)
	if len(n) != shareCodeDataLen+1 {
		return fmt.Errorf("share code must be %d characters, got %d", shareCodeDataLen+1, len(n))
	}

	data := n[:shareCodeDataLen]
	for i := 0; i < len(data); i++ {
		if strings.IndexByte(crockfordAlphabet, data[i]) < 0 {
			return fmt.Errorf("share code contains %q, which is not a valid character", data[i])
		}
	}
	if n[shareCodeDataLen] != crockfordCheckSymbol([]byte(data)) {
		return fmt.Errorf("share code check character does not match — it looks mistyped")
	}
	return nil
}

// ShareCodeMatches compares a supplied code against a stored one in constant
// time, after normalising both. It is a credential.
//
// A stored code that is empty never matches, so a stack with sharing off cannot
// be opened by supplying an empty code.
func ShareCodeMatches(supplied, stored string) bool {
	s := NormalizeShareCode(stored)
	if s == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(NormalizeShareCode(supplied)), []byte(s)) == 1
}

// crockfordCheckSymbol computes the standard Crockford check symbol: the value
// of the data interpreted as a base-32 integer, modulo 37, rendered from the
// extended alphabet.
//
// Taken modulo as we go rather than building a 75-bit integer — 37 is prime and
// coprime with 32, so the running remainder is exact.
func crockfordCheckSymbol(data []byte) byte {
	rem := 0
	for _, c := range data {
		v := strings.IndexByte(crockfordAlphabet, c)
		if v < 0 {
			// Only reachable if a caller hands this un-normalised or invalid
			// data; ValidateShareCode checks the alphabet before calling.
			return 0
		}
		rem = (rem*32 + v) % 37
	}
	return crockfordCheckAlphabet[rem]
}
