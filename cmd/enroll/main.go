package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"time"

	"github.com/joho/godotenv"

	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/database"
	"github.com/ic3software/vtafarm-api/internal/model"
)

func main() {
	_ = godotenv.Load()
	cfg := config.Load()

	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}

	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		log.Fatalf("rand: %v", err)
	}
	token := hex.EncodeToString(b)
	expires := time.Now().Add(24 * time.Hour)

	tok := model.AdminEnrollmentToken{
		Token:     token,
		ExpiresAt: expires,
	}
	if err := db.Create(&tok).Error; err != nil {
		log.Fatalf("create enrollment token: %v", err)
	}

	fmt.Printf("Enrollment token created (valid 24h)\n")
	fmt.Printf("  Token:   %s\n", token)
	fmt.Printf("  Expires: %s\n", expires.UTC().Format(time.RFC3339))
	fmt.Printf("\nPass the token to your frontend enrollment page, or call:\n")
	fmt.Printf("  POST http://localhost:%s/api/v1/admin/enroll/%s\n", cfg.AppPort, token)
	fmt.Printf("\nThe admin account will be created when the token is consumed.\n")
}
