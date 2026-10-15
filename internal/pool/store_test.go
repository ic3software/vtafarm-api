package pool

import (
	"context"
	"errors"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
	"gorm.io/gorm"
)

func poolDatabase(t *testing.T) *gorm.DB {
	t.Helper()
	return testutil.Postgres(t)
}

func admission(t *testing.T, db *gorm.DB, count, available int) (*model.PoolBatch, bool, error) {
	t.Helper()
	admin := model.Admin{UniqueId: "pool-admin"}
	if err := db.FirstOrCreate(&admin, "unique_id = ?", admin.UniqueId).Error; err != nil {
		t.Fatal(err)
	}
	return Admit(context.Background(), db, Admission{
		AdminID: admin.ID,
		Key:     "create-event-one",
		Digest:  "digest-one",
		Label:   "Event One",
		Count:   count,
		Available: func(context.Context) (int, error) {
			return available, nil
		},
		Session: model.SetupSession{
			Mode:                 model.ModeVtaOnly,
			Domain:               "example.com",
			Subdomain:            "vta-",
			VtaImage:             "example/vta:test",
			DidHostingServerURL:  "https://dids.example.com",
			DidHostingControlURL: "https://dids.example.com",
		},
		MemoryRequest: "32Mi",
		MemoryLimit:   "64Mi",
	})
}

func TestAdmitStoresOnlyBatchMembership(t *testing.T) {
	db := poolDatabase(t)
	batch, created, err := admission(t, db, 2, 2)
	if err != nil || !created {
		t.Fatalf("Admit() = %#v, %v, %v", batch, created, err)
	}

	var memberships []model.PoolSession
	if err := db.Where("batch_id = ?", batch.ID).Order("session_id").Find(&memberships).Error; err != nil {
		t.Fatal(err)
	}
	if len(memberships) != 2 {
		t.Fatalf("memberships = %d, want 2", len(memberships))
	}
	for _, membership := range memberships {
		var session model.SetupSession
		if err := db.First(&session, membership.SessionID).Error; err != nil {
			t.Fatal(err)
		}
		if session.Status != model.StatusPoolCreating || session.UserID == 0 {
			t.Fatalf("session not admitted as a real owned VTA: %+v", session)
		}
	}

	retried, created, err := admission(t, db, 2, 0)
	if err != nil || created || retried.ID != batch.ID {
		t.Fatalf("idempotent Admit() = %#v, %v, %v", retried, created, err)
	}
}

func TestAdmitUsesDashboardCapacity(t *testing.T) {
	db := poolDatabase(t)
	if _, _, err := admission(t, db, 48, 47); !errors.Is(err, ErrCapacity) {
		t.Fatalf("Admit(48 with 47 available) error = %v, want ErrCapacity", err)
	}
	var batches int64
	if err := db.Model(&model.PoolBatch{}).Count(&batches).Error; err != nil {
		t.Fatal(err)
	}
	if batches != 0 {
		t.Fatalf("capacity rejection created %d batches", batches)
	}
	if _, created, err := admission(t, db, 47, 47); err != nil || !created {
		t.Fatalf("Admit(47 with 47 available) created=%v error=%v", created, err)
	}
}

func TestConfirmedFailureCannotReturnToInventory(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var membership model.PoolSession
	if err := db.First(&membership, "batch_id = ?", batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SetupSession{}).Where("id = ?", membership.SessionID).Updates(map[string]any{
		"status":  model.StatusPoolWaiting,
		"vta_did": "did:webvh:example.com:test",
	}).Error; err != nil {
		t.Fatal(err)
	}

	reservation, err := Reserve(context.Background(), db, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	did, err := didkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Claim(context.Background(), db, *reservation.ClaimHandle, did); err != nil {
		t.Fatal(err)
	}
	if _, err := Confirm(context.Background(), db, reservation.ID); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SetupSession{}).Where("id = ?", membership.SessionID).Update("status", "failed").Error; err != nil {
		t.Fatal(err)
	}
	if err := QueueCleanup(context.Background(), db, []uint{membership.SessionID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("QueueCleanup() error = %v, want ErrConflict", err)
	}
}
