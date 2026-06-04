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

	// Seed cluster settings (placeholder ingress IP for local dev)
	setting := model.ClusterSetting{Name: "dev-cluster", IngressIP: "1.2.3.4"}
	var existing model.ClusterSetting
	if db.First(&existing).Error != nil {
		if err := db.Create(&setting).Error; err != nil {
			log.Fatalf("seed cluster_settings: %v", err)
		}
		fmt.Printf("seeded cluster_settings: ingress_ip=%s (id=%d)\n", setting.IngressIP, setting.ID)
	} else {
		fmt.Printf("cluster_settings already exists: ingress_ip=%s (id=%d)\n", existing.IngressIP, existing.ID)
	}

	fmt.Println("seed complete")
}
