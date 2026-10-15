package handler

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/model"
	poolstore "github.com/ic3software/vtafarm-api/internal/pool"
	"github.com/ic3software/vtafarm-api/internal/testutil"
	"gorm.io/gorm"
)

func TestCleanupPoolBatchDeletesDedicatedOwnerAndBatch(t *testing.T) {
	db := testutil.Postgres(t)
	admin := model.Admin{UniqueId: "pool-cleanup"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	admitted, err := poolstore.Admit(context.Background(), db, poolstore.Admission{
		AdminID: admin.ID, Key: "cleanup", Digest: "cleanup", Label: "Cleanup", Count: 1,
		Capacity: 1,
		Session: model.SetupSession{
			Mode: model.ModeVtaOnly, Domain: "example.com", Subdomain: "vta-",
			VtaImage: "example/vta:test", DidHostingServerURL: "https://dids.example.com",
		},
		MemoryRequest: "32Mi", MemoryLimit: "64Mi",
	})
	if err != nil {
		t.Fatal(err)
	}
	batch := admitted.Batch
	var membership model.PoolSession
	if err := db.First(&membership, "batch_id = ?", batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	var session model.SetupSession
	if err := db.First(&session, membership.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&session).Update("status", model.StatusPoolWaiting).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := poolstore.QueueBatchCleanup(context.Background(), db, batch.ID); err != nil {
		t.Fatal(err)
	}
	handler := &PoolHandler{db: db, setup: &SetupHandler{db: db}}
	if err := handler.cleanupSession(context.Background(), session.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.First(&model.SetupSession{}, session.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("setup session lookup error = %v, want not found", err)
	}
	if err := db.First(&model.User{}, session.UserID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("dedicated user lookup error = %v, want not found", err)
	}
	if err := db.First(&model.PoolBatch{}, batch.ID).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("pool batch lookup error = %v, want not found", err)
	}
}

func TestPoolBatchViewTreatsExpiredClaimAsLapsedWithoutWriting(t *testing.T) {
	db := testutil.Postgres(t)
	admin := model.Admin{UniqueId: "pool-read"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	admitted, err := poolstore.Admit(context.Background(), db, poolstore.Admission{
		AdminID: admin.ID, Key: "read", Digest: "read", Label: "Read", Count: 1, Capacity: 1,
		Session: model.SetupSession{
			Mode: model.ModeVtaOnly, Domain: "example.com", Subdomain: "vta-",
			VtaImage: "example/vta:test", DidHostingServerURL: "https://dids.example.com",
		},
		MemoryRequest: "32Mi", MemoryLimit: "64Mi",
	})
	if err != nil {
		t.Fatal(err)
	}
	session := admitted.Sessions[0]
	if err := db.Model(&session).Update("status", model.StatusPoolWaiting).Error; err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-time.Minute)
	claim := model.MobileConnection{
		ID: uuid.NewString(), SessionID: session.ID, VtaDid: "did:webvh:test",
		Operation: "provision_vta", Status: "open", ExpiresAt: past,
	}
	if err := db.Create(&claim).Error; err != nil {
		t.Fatal(err)
	}
	handler := &PoolHandler{db: db}
	view, err := handler.batchView(context.Background(), admitted.Batch.ID, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Sessions) != 1 || view.Sessions[0].ClaimState != "lapsed" || !view.Sessions[0].Cleanable {
		t.Fatalf("expired claim view = %+v", view.Sessions)
	}
	var stored model.MobileConnection
	if err := db.First(&stored, "id = ?", claim.ID).Error; err != nil {
		t.Fatal(err)
	}
	if stored.Status != "open" {
		t.Fatalf("GET changed claim status to %q, want open", stored.Status)
	}
}

func TestPoolBatchViewDoesNotOfferCleanupWhilePreparing(t *testing.T) {
	preparing := []string{
		model.StatusPoolCreating,
		"vta_setup_running",
		model.StatusPoolPublishing,
		model.StatusPoolPrepared,
	}
	for _, status := range preparing {
		view := buildPoolBatchView(
			model.PoolBatch{ID: 1, RequestedCount: 1},
			[]model.SetupSession{{ID: 1, UserID: 1, Status: status}},
			nil,
			true,
		)
		if len(view.Sessions) != 1 || view.Sessions[0].Cleanable {
			t.Fatalf("status %q cleanable view = %+v, want false", status, view.Sessions)
		}
	}
}
