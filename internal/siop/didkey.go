package siop

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"math/big"
	"strings"
)

const base58BTCAlphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// DIDKeyResolver resolves the canonical Ed25519 verification method of a
// did:key without performing network I/O.
type DIDKeyResolver struct{}

func (DIDKeyResolver) ResolveAuthenticationKey(_ context.Context, did, kid string) (ed25519.PublicKey, error) {
	key, multibase, err := ed25519KeyFromDIDKey(did)
	if err != nil {
		return nil, err
	}
	if kid != did+"#"+multibase {
		return nil, errors.New("kid is not the canonical did:key authentication method")
	}
	return key, nil
}

func ed25519KeyFromDIDKey(did string) (ed25519.PublicKey, string, error) {
	multibase, ok := strings.CutPrefix(did, "did:key:")
	if !ok || multibase == "" {
		return nil, "", errors.New("DID is not a did:key")
	}
	key, err := decodeEd25519Multikey(multibase)
	if err != nil {
		return nil, "", err
	}
	return key, multibase, nil
}

func decodeEd25519Multikey(multibase string) (ed25519.PublicKey, error) {
	if len(multibase) < 2 || multibase[0] != 'z' {
		return nil, errors.New("key is not base58btc multibase")
	}
	decoded, err := DecodeBase58BTC(multibase[1:])
	if err != nil {
		return nil, err
	}
	if EncodeBase58BTC(decoded) != multibase[1:] {
		return nil, errors.New("key is not canonical base58btc")
	}
	if len(decoded) != 2+ed25519.PublicKeySize || decoded[0] != 0xed || decoded[1] != 0x01 {
		return nil, errors.New("key is not a 32-byte Ed25519 multikey")
	}
	key := make(ed25519.PublicKey, ed25519.PublicKeySize)
	copy(key, decoded[2:])
	return key, nil
}

// DecodeBase58BTC decodes an unprefixed base58btc value.
func DecodeBase58BTC(value string) ([]byte, error) {
	if value == "" {
		return nil, errors.New("empty base58btc value")
	}
	alphabetIndexes := [256]int16{}
	for i := range alphabetIndexes {
		alphabetIndexes[i] = -1
	}
	for i, char := range []byte(base58BTCAlphabet) {
		alphabetIndexes[char] = int16(i)
	}

	number := new(big.Int)
	base := big.NewInt(58)
	for i := 0; i < len(value); i++ {
		char := value[i]
		if alphabetIndexes[char] < 0 {
			return nil, fmt.Errorf("invalid base58btc character at offset %d", i)
		}
		number.Mul(number, base)
		number.Add(number, big.NewInt(int64(alphabetIndexes[char])))
	}
	decoded := number.Bytes()
	for i := 0; i < len(value) && value[i] == base58BTCAlphabet[0]; i++ {
		decoded = append([]byte{0}, decoded...)
	}
	return decoded, nil
}

// EncodeBase58BTC encodes bytes without adding the multibase z prefix.
func EncodeBase58BTC(value []byte) string {
	number := new(big.Int).SetBytes(value)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	remainder := new(big.Int)
	encoded := make([]byte, 0, len(value)*2)
	for number.Cmp(zero) > 0 {
		number.DivMod(number, base, remainder)
		encoded = append(encoded, base58BTCAlphabet[remainder.Int64()])
	}
	for _, b := range value {
		if b != 0 {
			break
		}
		encoded = append(encoded, base58BTCAlphabet[0])
	}
	for left, right := 0, len(encoded)-1; left < right; left, right = left+1, right-1 {
		encoded[left], encoded[right] = encoded[right], encoded[left]
	}
	return string(encoded)
}
