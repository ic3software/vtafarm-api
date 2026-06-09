package main

import (
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/joho/godotenv"

	"github.com/ic3software/cipherportal-api/internal/config"
)

func main() {
	_ = godotenv.Load()
	cfg := config.Load()

	m, err := migrate.New("file://migrations", cfg.DB.URL())
	if err != nil {
		log.Fatalf("migrate.New: %v", err)
	}
	defer m.Close()

	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	switch cmd {
	case "up":
		if err := m.Up(); err != nil && !errors.Is(err, migrate.ErrNoChange) {
			log.Fatalf("migrate up: %v", err)
		}
		fmt.Println("migrate: up — done")

	case "down":
		// Roll back exactly one migration step
		if err := m.Steps(-1); err != nil {
			log.Fatalf("migrate down: %v", err)
		}
		fmt.Println("migrate: down 1 step — done")

	case "drop":
		if err := m.Drop(); err != nil {
			log.Fatalf("migrate drop: %v", err)
		}
		fmt.Println("migrate: drop — done")

	case "force":
		if len(os.Args) < 3 {
			fmt.Fprintf(os.Stderr, "usage: force <version>\n")
			os.Exit(1)
		}
		var version int
		if _, err := fmt.Sscanf(os.Args[2], "%d", &version); err != nil {
			fmt.Fprintf(os.Stderr, "invalid version %q\n", os.Args[2])
			os.Exit(1)
		}
		if err := m.Force(version); err != nil {
			log.Fatalf("migrate force: %v", err)
		}
		fmt.Printf("migrate: forced version %d — done\n", version)

	default:
		fmt.Fprintf(os.Stderr, "unknown command %q — use: up | down | drop | force <version>\n", cmd)
		os.Exit(1)
	}
}
