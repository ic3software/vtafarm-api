package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"log"

	"github.com/ic3software/vtafarm-api/internal/didkey"
)

func main() {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		log.Fatal(err)
	}

	didKey, err := didkey.FromPublicKey(pub)
	if err != nil {
		log.Fatal(err)
	}

	// Store only the 32-byte seed (private key), not the full 64-byte Go representation
	privB64 := base64.StdEncoding.EncodeToString(priv.Seed())

	fmt.Println("=== VTA Farm Service Keypair ===")
	fmt.Println()
	fmt.Printf("DID_HOSTING_PRIVATE_KEY=%s\n", privB64)
	fmt.Printf("DID_HOSTING_DID=%s\n", didKey)
	fmt.Println()
	fmt.Println("Steps:")
	fmt.Println("1. Copy the two lines above into your .env file")
	fmt.Printf("2. In did-hosting Access Control → Add Entry:\n")
	fmt.Printf("   DID:   %s\n", didKey)
	fmt.Println("   Role:  Service")
	fmt.Println("   Label: vtafarm")
	fmt.Println("   Domain scope: All")
}
