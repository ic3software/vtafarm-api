package main

import (
	"fmt"
	"log"

	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"

	"github.com/ic3software/cipherportal-api/internal/config"
	"github.com/ic3software/cipherportal-api/internal/database"
	"github.com/ic3software/cipherportal-api/internal/model"
)

func main() {
	_ = godotenv.Load()
	cfg := config.Load()

	db, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}

	// Seed users
	hash, err := bcrypt.GenerateFromPassword([]byte("demo1234"), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("bcrypt: %v", err)
	}
	user := model.User{
		Email:    "demo@example.com",
		Username: "demo",
		Password: string(hash),
	}
	result := db.Where(model.User{Email: user.Email}).FirstOrCreate(&user)
	if result.Error != nil {
		log.Fatalf("seed user: %v", result.Error)
	}
	if result.RowsAffected > 0 {
		fmt.Printf("seeded user: %s (id=%d)\n", user.Username, user.ID)
	} else {
		fmt.Printf("user already exists: %s (id=%d)\n", user.Username, user.ID)
	}

	fmt.Println("seed complete")
}
