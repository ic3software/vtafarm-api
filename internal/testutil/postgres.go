package testutil

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/model"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func Postgres(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("VTAFARM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set VTAFARM_TEST_DATABASE_URL to an isolated PostgreSQL database")
	}
	root, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	schema := "mobile_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if err := root.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	q := parsed.Query()
	q.Set("search_path", schema)
	parsed.RawQuery = q.Encode()
	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool, _ := db.DB()
		pool.Close()
		root.Exec("DROP SCHEMA " + schema + " CASCADE")
		pool, _ = root.DB()
		pool.Close()
	})
	files, err := filepath.Glob("../../migrations/*.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		body, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(string(body)).Error; err != nil {
			t.Fatalf("migration %s: %v", filepath.Base(file), err)
		}
	}
	return db
}

func SetupSession(t *testing.T, db *gorm.DB, mode string) model.SetupSession {
	t.Helper()
	u := model.User{UniqueId: strings.ReplaceAll(uuid.NewString(), "-", "")[:12]}
	if err := db.Create(&u).Error; err != nil {
		t.Fatal(err)
	}
	status := "vta_setup_complete"
	if mode == model.ModeFullStack {
		status = "awaiting_admin_did"
	}
	s := model.SetupSession{UserID: u.ID, VtaName: "test-" + uuid.NewString(), Mode: mode, Status: status, VtaDid: "did:webvh:test", Domain: "example.com", Subdomain: "test"}
	if err := db.Create(&s).Error; err != nil {
		t.Fatal(err)
	}
	return s
}
