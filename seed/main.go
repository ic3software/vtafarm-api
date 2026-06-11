package main

import (
	"fmt"
	"log"

	"github.com/joho/godotenv"

	"github.com/ic3software/cipherportal-api/internal/config"
	"github.com/ic3software/cipherportal-api/internal/database"
)

func main() {
	_ = godotenv.Load()
	cfg := config.Load()

	_, err := database.Connect(cfg)
	if err != nil {
		log.Fatalf("db connect: %v", err)
	}

	fmt.Println("No seed data to insert.")
	fmt.Println("To create the first admin, run:  go run ./cmd/enroll")
}
