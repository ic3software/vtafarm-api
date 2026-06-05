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

	hash, err := bcrypt.GenerateFromPassword([]byte("admin1234"), bcrypt.DefaultCost)
	if err != nil {
		log.Fatalf("bcrypt: %v", err)
	}
	admin := model.Admin{
		Email:    "admin@example.com",
		Username: "admin",
		Password: string(hash),
	}
	result := db.Where(model.Admin{Email: admin.Email}).FirstOrCreate(&admin)
	if result.Error != nil {
		log.Fatalf("seed admin: %v", result.Error)
	}
	if result.RowsAffected > 0 {
		fmt.Printf("seeded admin: %s (id=%d)\n", admin.Username, admin.ID)
	} else {
		fmt.Printf("admin already exists: %s (id=%d)\n", admin.Username, admin.ID)
	}

	fmt.Println("seed complete")
}
