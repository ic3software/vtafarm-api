package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"
	"math/big"
)

// base58btc alphabet used by multibase
const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func base58Encode(b []byte) string {
	n := new(big.Int).SetBytes(b)
	base := big.NewInt(58)
	zero := big.NewInt(0)
	mod := new(big.Int)

	var result []byte
	for n.Cmp(zero) > 0 {
		n.DivMod(n, base, mod)
		result = append(result, alphabet[mod.Int64()])
	}
	// leading zero bytes → '1'
	for _, byt := range b {
		if byt != 0 {
			break
		}
		result = append(result, alphabet[0])
	}
	// reverse
	for i, j := 0, len(result)-1; i < j; i, j = i+1, j-1 {
		result[i], result[j] = result[j], result[i]
	}
	return string(result)
}

func main() {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}

	// did:key encoding: multibase base58btc of (multicodec ed25519-pub prefix + pubkey)
	// ed25519-pub multicodec varint: 0xed 0x01
	prefixed := append([]byte{0xed, 0x01}, pub...)
	didKey := "did:key:z" + base58Encode(prefixed)

	// Store only the 32-byte seed (private key), not the full 64-byte Go representation
	privB64 := base64.StdEncoding.EncodeToString(priv.Seed())

	fmt.Println("=== CipherPortal Service Keypair ===")
	fmt.Println()
	fmt.Printf("DID_HOSTING_PRIVATE_KEY=%s\n", privB64)
	fmt.Printf("DID_HOSTING_DID=%s\n", didKey)
	fmt.Println()
	fmt.Println("Steps:")
	fmt.Println("1. Copy the two lines above into your .env file")
	fmt.Printf("2. In did-hosting Access Control → Add Entry:\n")
	fmt.Printf("   DID:   %s\n", didKey)
	fmt.Println("   Role:  Service")
	fmt.Println("   Label: cipherportal")
	fmt.Println("   Domain scope: All")
}
