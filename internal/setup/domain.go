package setup

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"strings"

	"golang.org/x/net/idna"
)

// Normalisation and validation for a domain a user wants to attach. Kept here
// beside ValidateName and FixedHosts because the two are cousins: one decides
// what may become a label, the other what may become a zone.

// challengePrefix is the label the TXT challenge lives at. Underscore-prefixed
// so it can never collide with a real host in the user's zone.
const challengePrefix = "_vtafarm-challenge"

// verifyTokenPrefix keeps the TXT value self-describing in a zone that may hold
// a dozen other vendors' tokens.
const verifyTokenPrefix = "vtafarm-verify="

// maxFQDNLength is the DNS limit for a full name. The longest label we prepend
// is "dev-mediator", so a domain has to leave room for that plus the dot.
const maxFQDNLength = 253

// longestComponentLabel is "dev-mediator" — the widest of the four fixed labels
// with the development prefix applied.
const longestComponentLabel = "dev-mediator"

// Names that can never be issued a publicly trusted certificate, so attaching
// one could only ever end in a failed session.
var reservedTLDs = map[string]bool{
	"local": true, "internal": true, "test": true,
	"invalid": true, "example": true, "localhost": true, "onion": true,
}

var (
	ErrDomainInvalid    = errors.New("invalid domain")
	ErrDomainNotPublic  = errors.New("this domain can't be issued a public TLS certificate")
	ErrDomainTooLong    = errors.New("domain too long")
	ErrDomainIsOurs     = errors.New("managed by VTA Farm")
	ErrDomainIsHostname = errors.New("looks like a component hostname")
)

// NormalizeDomain turns whatever the user pasted into the canonical form
// stored in domains.domain: lowercase, no scheme, no path, no trailing dot,
// punycode for internationalised names.
//
// Users paste URLs constantly — "https://aaa.com/" is the single most common
// input — so normalising is worth more than rejecting.
func NormalizeDomain(input string) (string, error) {
	s := strings.TrimSpace(input)
	if s == "" {
		return "", ErrDomainInvalid
	}

	// Strip a scheme and anything after the authority. Doing this textually
	// avoids url.Parse's habit of reading a bare "aaa.com/x" as a path.
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	// Credentials and port, if the paste came from a browser URL bar.
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if h, _, err := net.SplitHostPort(s); err == nil {
		s = h
	}
	s = strings.Trim(s, ".")
	s = strings.ToLower(s)
	if s == "" {
		return "", ErrDomainInvalid
	}

	// Punycode, so "bücher.de" and "xn--bcher-kva.de" are the same row and the
	// same certificate. idna.Lookup is the profile for names being resolved.
	ascii, err := idna.Lookup.ToASCII(s)
	if err != nil {
		return "", ErrDomainInvalid
	}
	return ascii, nil
}

// ValidateDomain reports whether a normalised domain may be attached.
//
// clusterDomain and every subdomain of it are refused for every caller,
// including admins — the only way a row for our own zone comes into existence
// is POST /admin/platform-stack, which always writes kind=platform. Enforcing
// that at the route rather than by role matters because the two paths produce
// different objects: a custom row would drag in TXT verification, an ACME
// certificate and per-user ownership, none of which apply to a zone we already
// control.
func ValidateDomain(domain, clusterDomain string) error {
	if domain == "" {
		return ErrDomainInvalid
	}
	if net.ParseIP(domain) != nil {
		return ErrDomainNotPublic
	}

	labels := strings.Split(domain, ".")
	if len(labels) < 2 {
		// Single-label names are either reserved ("localhost") or unresolvable
		// on the public internet.
		return ErrDomainNotPublic
	}
	for _, l := range labels {
		if !validDNSLabel(l) {
			return ErrDomainInvalid
		}
	}
	if reservedTLDs[labels[len(labels)-1]] {
		return ErrDomainNotPublic
	}

	if clusterDomain != "" {
		if domain == clusterDomain || strings.HasSuffix(domain, "."+clusterDomain) {
			return ErrDomainIsOurs
		}
	}

	// "vta.aaa.com" is what we will *create*; attaching it would produce
	// vta.vta.aaa.com. Catching it here beats letting the CNAME check fail
	// with a mismatch the user can't interpret.
	switch labels[0] {
	case "vta", "vtc", "mediator", "dids":
		return ErrDomainIsHostname
	}

	if len(longestComponentLabel)+1+len(domain) > maxFQDNLength {
		return ErrDomainTooLong
	}
	return nil
}

// validDNSLabel reports whether l is a legal label in a domain the user
// already owns.
//
// Deliberately laxer than ValidateName, which governs names *we* mint: labels
// here may run to the full 63 characters and may contain consecutive hyphens,
// because "xn--" is exactly how every internationalised domain is spelled once
// NormalizeDomain has punycoded it. Rejecting "--" would refuse every IDN.
func validDNSLabel(l string) bool {
	if len(l) == 0 || len(l) > 63 {
		return false
	}
	if l[0] == '-' || l[len(l)-1] == '-' {
		return false
	}
	for i := range len(l) {
		c := l[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

// ChallengeName is where the TXT token belongs. The apex is accepted too (some
// DNS panels can't create underscore labels), but this is what we advertise.
func ChallengeName(domain string) string {
	return challengePrefix + "." + domain
}

// MintVerifyToken returns a fresh TXT value.
//
// A new token on every attach is what closes the dangling-DNS takeover: if a
// user leaves their CNAMEs behind after deleting a session, the next person to
// attach that domain inherits records pointing at us — but not the ability to
// write a new token into a zone they don't control.
func MintVerifyToken() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand does not fail in practice; a panic here is preferable to
		// minting a predictable challenge.
		panic("dnscheck: cannot read random bytes: " + err.Error())
	}
	return verifyTokenPrefix + hex.EncodeToString(b)
}

// CustomHosts returns the four FQDNs a fixed-label domain serves, in the same
// component order everything else uses: vta, mediator, dids, vtc.
func CustomHosts(env, domain string) (vtaFQDN, mediatorFQDN, didsFQDN, vtcFQDN string) {
	vtaSub, mediatorSub, didsSub, vtcSub := FixedHosts(env)
	return vtaSub + "." + domain,
		mediatorSub + "." + domain,
		didsSub + "." + domain,
		vtcSub + "." + domain
}
