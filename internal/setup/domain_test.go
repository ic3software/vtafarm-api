package setup

import (
	"errors"
	"strings"
	"testing"
)

func TestNormalizeDomain(t *testing.T) {
	cases := map[string]string{
		"aaa.com":                    "aaa.com",
		"AAA.CoM":                    "aaa.com",
		"  aaa.com  ":                "aaa.com",
		"https://aaa.com":            "aaa.com",
		"http://aaa.com/":            "aaa.com",
		"https://aaa.com/some/path":  "aaa.com",
		"https://aaa.com:8443/x":     "aaa.com",
		"https://user@aaa.com":       "aaa.com",
		"aaa.com.":                   "aaa.com",
		"sub.aaa.com":                "sub.aaa.com",
		"https://aaa.com?q=1":        "aaa.com",
		"https://aaa.com#frag":       "aaa.com",
		"bücher.de":                  "xn--bcher-kva.de",
		"xn--bcher-kva.de":           "xn--bcher-kva.de",
		"https://BÜCHER.de/katalog/": "xn--bcher-kva.de",
	}
	for in, want := range cases {
		got, err := NormalizeDomain(in)
		if err != nil {
			t.Errorf("NormalizeDomain(%q) = error %v, want %q", in, err, want)
			continue
		}
		if got != want {
			t.Errorf("NormalizeDomain(%q) = %q, want %q", in, got, want)
		}
	}

	for _, in := range []string{
		"", "   ", "https://", "/path/only", ".",
		// IDNA reserves hyphens in the 3rd and 4th positions for punycode, so
		// a lookalike that isn't one is refused before it can reach a
		// certificate. "a--b.com" is fine — the rule is positional.
		"ab--cd.com",
	} {
		if got, err := NormalizeDomain(in); err == nil {
			t.Errorf("NormalizeDomain(%q) = %q, want error", in, got)
		}
	}
}

func TestValidateDomain(t *testing.T) {
	const cluster = "firstperson.dev"

	// a--b.com is legal: labels in a domain the user already owns may run to 63
	// chars and carry consecutive hyphens, which is how every punycoded IDN is
	// spelled. Only ValidateName — which governs names we mint — is stricter.
	for _, d := range []string{"aaa.com", "sub.aaa.com", "a.b.c.example.org", "xn--bcher-kva.de", "a--b.com",
		strings.Repeat("x", 63) + ".com"} {
		if err := ValidateDomain(d, cluster); err != nil {
			t.Errorf("ValidateDomain(%q) = %v, want nil", d, err)
		}
	}

	cases := []struct {
		domain string
		want   error
	}{
		// Our own zone, for every caller including an admin: the only route
		// that may mint a firstperson.dev row is POST /admin/platform-stack.
		{"firstperson.dev", ErrDomainIsOurs},
		{"anything.firstperson.dev", ErrDomainIsOurs},
		{"deep.nested.firstperson.dev", ErrDomainIsOurs},
		// Not "firstperson.dev" — a different registrable domain that merely
		// ends in the same letters.
		{"notfirstperson.dev", nil},

		// A hostname we would create under the domain, rather than the domain.
		{"vta.aaa.com", ErrDomainIsHostname},
		{"dids.aaa.com", ErrDomainIsHostname},

		{"localhost", ErrDomainNotPublic},
		{"aaa.local", ErrDomainNotPublic},
		{"aaa.internal", ErrDomainNotPublic},
		{"aaa.test", ErrDomainNotPublic},
		{"aaa.example", ErrDomainNotPublic},
		{"aaa.invalid", ErrDomainNotPublic},
		{"192.0.2.1", ErrDomainNotPublic},
		{"com", ErrDomainNotPublic},

		{"aaa_bad.com", ErrDomainInvalid},
		{"-aaa.com", ErrDomainInvalid},
		{"aaa-.com", ErrDomainInvalid},
		{strings.Repeat("x", 64) + ".com", ErrDomainInvalid},

		// dev-mediator. + 241 chars + ".com" tips the longest FQDN past 253.
		{strings.Repeat("a", 60) + "." + strings.Repeat("b", 60) + "." +
			strings.Repeat("c", 60) + "." + strings.Repeat("d", 55) + ".com", ErrDomainTooLong},
	}
	for _, tc := range cases {
		err := ValidateDomain(tc.domain, cluster)
		if tc.want == nil {
			if err != nil {
				t.Errorf("ValidateDomain(%q) = %v, want nil", tc.domain, err)
			}
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("ValidateDomain(%q) = %v, want %v", tc.domain, err, tc.want)
		}
	}
}

func TestMintVerifyToken(t *testing.T) {
	a, b := MintVerifyToken(), MintVerifyToken()
	if a == b {
		// A repeated token would reopen the dangling-DNS takeover the per-attach
		// mint exists to close.
		t.Error("MintVerifyToken returned the same value twice")
	}
	for _, tok := range []string{a, b} {
		if !strings.HasPrefix(tok, "vtafarm-verify=") {
			t.Errorf("MintVerifyToken() = %q, want the vtafarm-verify= prefix", tok)
		}
		if len(tok) != len("vtafarm-verify=")+32 {
			t.Errorf("MintVerifyToken() = %q, want 32 hex chars of entropy", tok)
		}
	}
}

func TestChallengeNameAndCustomHosts(t *testing.T) {
	if got := ChallengeName("aaa.com"); got != "_vtafarm-challenge.aaa.com" {
		t.Errorf("ChallengeName = %q", got)
	}

	vta, mediator, dids, vtc := CustomHosts("production", "aaa.com")
	if vta != "vta.aaa.com" || mediator != "mediator.aaa.com" || dids != "dids.aaa.com" || vtc != "vtc.aaa.com" {
		t.Errorf("CustomHosts(production) = %q %q %q %q", vta, mediator, dids, vtc)
	}

	// Development and production may attach the same domain and run at once —
	// the dev- prefix is one of the three things keeping them apart.
	vta, mediator, dids, vtc = CustomHosts("development", "aaa.com")
	if vta != "dev-vta.aaa.com" || mediator != "dev-mediator.aaa.com" ||
		dids != "dev-dids.aaa.com" || vtc != "dev-vtc.aaa.com" {
		t.Errorf("CustomHosts(development) = %q %q %q %q", vta, mediator, dids, vtc)
	}
}
