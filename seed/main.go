package main

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/joho/godotenv"
	"golang.org/x/crypto/bcrypt"
	"sigs.k8s.io/yaml"

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

	// Seed users first
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

	// Seed pod deployments
	nginxYAML := `apiVersion: v1
kind: Pod
metadata:
  name: nginx-demo
spec:
  containers:
  - name: nginx
    image: nginx:alpine
    ports:
    - containerPort: 80`

	nginxJSON, err := yaml.YAMLToJSON([]byte(nginxYAML))
	if err != nil {
		log.Fatalf("yaml to json: %v", err)
	}

	pods := []model.PodDeployment{
		{
			UserID:    user.ID,
			Name:      "nginx-demo",
			Namespace: fmt.Sprintf("cp-user-%d", user.ID),
			Spec:      json.RawMessage(nginxJSON),
			Status:    "seeded",
		},
	}

	for _, s := range pods {
		r := db.Where(model.PodDeployment{Name: s.Name, UserID: s.UserID}).FirstOrCreate(&s)
		if r.Error != nil {
			log.Printf("seed pod %s: %v", s.Name, r.Error)
		} else if r.RowsAffected > 0 {
			fmt.Printf("seeded pod: %s\n", s.Name)
		} else {
			fmt.Printf("pod already exists: %s\n", s.Name)
		}
	}

	fmt.Println("seed complete")
}
