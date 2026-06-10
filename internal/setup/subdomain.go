package setup

import (
	"crypto/rand"
	"math/big"
)

const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
const idLength = 8

// GenerateSubdomain returns a random subdomain prefix using an 8-char alphanumeric ID.
// In development it uses "cp-local-<id>" to distinguish local DNS records from production.
func GenerateSubdomain(env string) string {
	id := make([]byte, idLength)
	for i := range id {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		id[i] = alphabet[n.Int64()]
	}
	if env == "development" {
		return "cp-local-" + string(id)
	}
	return "cp-" + string(id)
}
