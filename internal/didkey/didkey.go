package didkey

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"math/big"
)

const base58BTCAlphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// Generate creates a valid did:key and deliberately discards its private key.
func Generate() (string, error) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	return FromPublicKey(publicKey)
}

// FromPublicKey encodes an Ed25519 public key as a did:key identifier.
func FromPublicKey(publicKey ed25519.PublicKey) (string, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return "", errors.New("ed25519 public key must be 32 bytes")
	}

	// ed25519-pub multicodec varint: 0xed 0x01.
	prefixed := make([]byte, 2+len(publicKey))
	prefixed[0], prefixed[1] = 0xed, 0x01
	copy(prefixed[2:], publicKey)
	return "did:key:z" + base58Encode(prefixed), nil
}

func base58Encode(value []byte) string {
	n := new(big.Int).SetBytes(value)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	mod := new(big.Int)

	result := make([]byte, 0, len(value)*2)
	for n.Cmp(zero) > 0 {
		n.DivMod(n, base, mod)
		result = append(result, base58BTCAlphabet[mod.Int64()])
	}
	for _, b := range value {
		if b != 0 {
			break
		}
		result = append(result, base58BTCAlphabet[0])
	}
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return string(result)
}
