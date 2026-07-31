package setup

import (
	"strings"
	"testing"
)

func TestNewShareCodeIsValidAndFormatted(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		code, err := NewShareCode()
		if err != nil {
			t.Fatalf("NewShareCode: %v", err)
		}
		if err := ValidateShareCode(code); err != nil {
			t.Fatalf("minted code %q failed its own validation: %v", code, err)
		}
		if got, want := code, "XXXX-XXXX-XXXX-XXXX"; len(got) != len(want) {
			t.Fatalf("code %q has length %d, want %d", got, len(got), len(want))
		}
		if strings.Count(code, "-") != 3 {
			t.Fatalf("code %q is not grouped in fours", code)
		}
		if seen[code] {
			t.Fatalf("minted a duplicate code %q within 200 draws", code)
		}
		seen[code] = true
	}
}

// The alphabet exists to keep confusable glyphs out of a code that gets read
// aloud. If a minted code can contain them, normalisation would rewrite it into
// something that no longer matches what is stored.
func TestNewShareCodeExcludesConfusableGlyphs(t *testing.T) {
	for i := 0; i < 200; i++ {
		code, err := NewShareCode()
		if err != nil {
			t.Fatalf("NewShareCode: %v", err)
		}
		data := NormalizeShareCode(code)[:shareCodeDataLen]
		if idx := strings.IndexAny(data, "ILOU"); idx >= 0 {
			t.Fatalf("code %q contains excluded glyph %q", code, data[idx])
		}
	}
}

func TestNormalizeShareCode(t *testing.T) {
	cases := []struct{ in, want string }{
		{"K7M2-9XQP-4B8W-3NRT", "K7M29XQP4B8W3NRT"},
		{"k7m2-9xqp-4b8w-3nrt", "K7M29XQP4B8W3NRT"},
		{"K7M2 9XQP 4B8W 3NRT", "K7M29XQP4B8W3NRT"},
		{" K7M29XQP4B8W3NRT\n", "K7M29XQP4B8W3NRT"},
		// Crockford's own folding: the glyphs a human substitutes anyway.
		{"I", "1"},
		{"l", "1"},
		{"O", "0"},
		{"o", "0"},
		{"", ""},
	}
	for _, c := range cases {
		if got := NormalizeShareCode(c.in); got != c.want {
			t.Errorf("NormalizeShareCode(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// Every form a human might produce from one code has to reach the same
// comparison, or a correct code is rejected as wrong.
func TestNormalizeIsStableAcrossTranscriptions(t *testing.T) {
	code, err := NewShareCode()
	if err != nil {
		t.Fatalf("NewShareCode: %v", err)
	}
	canonical := NormalizeShareCode(code)

	for _, variant := range []string{
		code,
		strings.ToLower(code),
		strings.ReplaceAll(code, "-", ""),
		strings.ReplaceAll(code, "-", " "),
		" " + code + " ",
	} {
		if got := NormalizeShareCode(variant); got != canonical {
			t.Errorf("variant %q normalised to %q, want %q", variant, got, canonical)
		}
	}
}

func TestValidateShareCodeRejectsBadInput(t *testing.T) {
	good, err := NewShareCode()
	if err != nil {
		t.Fatalf("NewShareCode: %v", err)
	}
	norm := NormalizeShareCode(good)

	cases := []struct{ name, code string }{
		{"empty", ""},
		{"too short", norm[:len(norm)-1]},
		{"too long", norm + "7"},
		{"invalid character", "K7M29XQP4B8W3N!"},
	}
	for _, c := range cases {
		if err := ValidateShareCode(c.code); err == nil {
			t.Errorf("%s: expected an error for %q", c.name, c.code)
		}
	}
}

// The point of the check symbol: a single mistyped character is caught locally,
// so it never reaches the server and never lands in the generic "this bundle
// does not open anything" message.
func TestValidateShareCodeCatchesSingleCharacterTypos(t *testing.T) {
	code, err := NewShareCode()
	if err != nil {
		t.Fatalf("NewShareCode: %v", err)
	}
	norm := NormalizeShareCode(code)

	caught, total := 0, 0
	for i := 0; i < shareCodeDataLen; i++ {
		for _, sub := range crockfordAlphabet {
			if byte(sub) == norm[i] {
				continue
			}
			total++
			typo := norm[:i] + string(sub) + norm[i+1:]
			if ValidateShareCode(typo) != nil {
				caught++
			}
		}
	}
	// Crockford's mod-37 check detects every single-symbol substitution.
	if caught != total {
		t.Errorf("caught %d of %d single-character typos, want all", caught, total)
	}
}

func TestShareCodeMatches(t *testing.T) {
	code, err := NewShareCode()
	if err != nil {
		t.Fatalf("NewShareCode: %v", err)
	}

	if !ShareCodeMatches(code, code) {
		t.Error("a code must match itself")
	}
	if !ShareCodeMatches(strings.ToLower(strings.ReplaceAll(code, "-", " ")), code) {
		t.Error("a retyped code must match the stored one")
	}

	other, err := NewShareCode()
	if err != nil {
		t.Fatalf("NewShareCode: %v", err)
	}
	if ShareCodeMatches(other, code) {
		t.Error("a different code must not match")
	}
}

// Sharing is off exactly when the stored code is absent. An empty supplied code
// must not open it.
func TestShareCodeMatchesRefusesEmptyStored(t *testing.T) {
	for _, supplied := range []string{"", "K7M2-9XQP-4B8W-3NRT"} {
		if ShareCodeMatches(supplied, "") {
			t.Errorf("supplied %q matched an empty stored code", supplied)
		}
	}
}
