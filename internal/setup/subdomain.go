package setup

import (
	"crypto/rand"
	"math/big"
)

const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
const idLength = 8

// GenerateID returns a random 8-char alphanumeric ID, with no prefix.
func GenerateID() string {
	id := make([]byte, idLength)
	for i := range id {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		id[i] = alphabet[n.Int64()]
	}
	return string(id)
}

// GenerateSubdomain returns a random subdomain prefix using an 8-char alphanumeric ID.
// In development it uses "fpp-local-<id>" to distinguish local DNS records from production.
func GenerateSubdomain(env string) string {
	id := GenerateID()
	if env == "development" {
		return "fpp-local-" + id
	}
	return "fpp-" + id
}

// FullStackHosts derives the three full_stack subdomains (vta, mediator,
// dids) under domain: fpp[-local]-xxxx / mediator[-local]-xxxx /
// dids[-local]-xxxx, reusing the same random ID across all three so they're
// recognizable as one session and multiple full_stack VTIs can share the
// same cluster domain. In development each gets a "-local-" infix (matching
// GenerateSubdomain) to distinguish local DNS records from production.
func FullStackHosts(env string) (vtaSub, mediatorSub, didsSub string) {
	id := GenerateID()
	mid := "-"
	if env == "development" {
		mid = "-local-"
	}
	return "fpp" + mid + id, "mediator" + mid + id, "dids" + mid + id
}

// FullStackWithVtcHosts derives the four full_stack_with_vtc subdomains —
// the same three FullStackHosts produces plus vtc[-local]-xxxx, all sharing
// one random ID (design §3).
func FullStackWithVtcHosts(env string) (vtaSub, mediatorSub, didsSub, vtcSub string) {
	id := GenerateID()
	mid := "-"
	if env == "development" {
		mid = "-local-"
	}
	return "fpp" + mid + id, "mediator" + mid + id, "dids" + mid + id, "vtc" + mid + id
}
