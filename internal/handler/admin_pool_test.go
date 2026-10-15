package handler

import (
	"context"
	"errors"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
	poolstore "github.com/ic3software/vtafarm-api/internal/pool"
	"github.com/ic3software/vtafarm-api/internal/testutil"
	"gorm.io/gorm"
)

func TestCleanupPoolSessionDeletesDedicatedOwner(t *testing.T) {
	db := testutil.Postgres(t)
	admin := model.Admin{UniqueId: "pool-cleanup"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	batch, _, err := poolstore.Admit(context.Background(), db, poolstore.Admission{
		AdminID: admin.ID, Key: "cleanup", Digest: "cleanup", Label: "Cleanup", Count: 1,
		Available: func(context.Context) (int, error) { return 1, nil },
		Session: model.SetupSession{
			Mode: model.ModeVtaOnly, Domain: "example.com", Subdomain: "vta-",
			VtaImage: "example/vta:test", DidHostingServerURL: "https://dids.example.com",
		},
		MemoryRequest: "32Mi", MemoryLimit: "64Mi",
	})
	if err != nil {
		t.Fatal(err)
	}
	var membership model.PoolSession
	if err := db.First(&membership, "batch_id = ?", batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	var session model.SetupSession
	if err := db.First(&session, membership.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if err := poolstore.QueueCleanup(context.Background(), db, []uint{session.ID}); err != nil {
		t.Fatal(err)
	}
	handler := &SetupHandler{db: db}
	if err := handler.cleanupPoolSession(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&model.SetupSession{}, session.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("setup session lookup error = %v, want not found", err)
	}
	if err := db.First(&model.User{}, session.UserID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("dedicated user lookup error = %v, want not found", err)
	}
	if err := db.First(&model.PoolBatch{}, batch.ID).Error; err != nil {
		t.Fatalf("batch audit row was removed: %v", err)
	}
}
