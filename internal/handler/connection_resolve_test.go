package handler

import (
	"net/http"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/setup"
)

// Every case here is refused before the database is touched, which is why a
// handler with a nil db is a valid fixture — and is itself worth asserting:
// malformed input must not reach a query.
func TestResolveShareCodeRejectsBeforeAnyQuery(t *testing.T) {
	h := &SetupHandler{clusterDomain: "firstperson.dev"}

	good, err := setup.NewShareCode()
	if err != nil {
		t.Fatalf("NewShareCode: %v", err)
	}

	tests := []struct{ name, code string }{
		{"empty", ""},
		{"whitespace only", "   "},
		{"not a code at all", "did:key:z6MkSomething"},
		{"a pasted JSON bundle", `{"v":1,"kind":"vtafarm.stack-connection"}`},
		{"too short", setup.NormalizeShareCode(good)[:10]},
		// The check character earns its keep here: one wrong glyph is diagnosed
		// as a typo instead of falling through to the deliberately vague
		// invalid_bundle, which is the one message a user cannot act on.
		{"one character mistyped", mistype(good)},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			infra, provider, reason, detail := h.resolveShareCode(tc.code)
			if reason != reasonBadBundle {
				t.Fatalf("reason = %q, want %q", reason, reasonBadBundle)
			}
			if detail == "" {
				t.Error("a refusal must carry a sentence for the user")
			}
			if provider != nil || infra != (sharedInfra{}) {
				t.Error("a refusal must not return a provider or partial values")
			}
		})
	}
}

// A well-formed code has to reach the lookup — including in every transcription
// a person might produce, since the code is meant to survive being read aloud.
// A nil db panics there, which is what these assert against.
func TestResolveShareCodeReachesLookupForWellFormedCodes(t *testing.T) {
	code, err := setup.NewShareCode()
	if err != nil {
		t.Fatalf("NewShareCode: %v", err)
	}

	for _, variant := range []string{
		code,
		lower(code),
		strip(code),
	} {
		t.Run(variant, func(t *testing.T) {
			h := &SetupHandler{clusterDomain: "firstperson.dev"}
			defer func() {
				if r := recover(); r == nil {
					t.Error("expected the lookup to be reached (nil db panics); refused earlier instead")
				}
			}()
			_, _, reason, _ := h.resolveShareCode(variant)
			t.Fatalf("expected to reach the database, got reason %q", reason)
		})
	}
}

// The five ways a code can fail to open a stack must not be distinguishable, or
// this becomes a way to discover which stacks exist and which are shared. The
// status has to be uniform too — a different code per case leaks just as much.
func TestConnectionRefusalStatus(t *testing.T) {
	tests := []struct {
		reason string
		want   int
	}{
		{reasonBadBundle, http.StatusBadRequest},
		{reasonInvalidBundle, http.StatusForbidden},
		{reasonStackNotRunning, http.StatusConflict},
		{reasonStackAtConnLimit, http.StatusConflict},
	}
	for _, tc := range tests {
		if got := connectionRefusalStatus(tc.reason); got != tc.want {
			t.Errorf("connectionRefusalStatus(%q) = %d, want %d", tc.reason, got, tc.want)
		}
	}
}

// mistype changes one data character of a share code to a different valid
// symbol, leaving the check character stale.
func mistype(code string) string {
	n := []byte(setup.NormalizeShareCode(code))
	if n[0] == 'A' {
		n[0] = 'B'
	} else {
		n[0] = 'A'
	}
	return string(n)
}

func lower(s string) string {
	out := []rune(s)
	for i, r := range out {
		if r >= 'A' && r <= 'Z' {
			out[i] = r + 32
		}
	}
	return string(out)
}

func strip(s string) string {
	out := ""
	for _, r := range s {
		if r != '-' {
			out += string(r)
		}
	}
	return out
}
