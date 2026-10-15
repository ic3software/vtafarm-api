package publicid

import "crypto/rand"

const userAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"

// NewAccount returns the public identifier format shared by admin, signup,
// invitation, and preallocated pool accounts.
func NewAccount() (string, error) {
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	result := make([]byte, len(random))
	for i, value := range random {
		result[i] = userAlphabet[int(value)%len(userAlphabet)]
	}
	return string(result), nil
}
