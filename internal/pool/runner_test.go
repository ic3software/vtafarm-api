package pool

import (
	"context"
	"testing"
	"time"

	"github.com/ic3software/vtafarm-api/internal/model"
)

func TestRunnerStartsWorkersWhenReconciliationIsDisabled(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	var memberships []model.PoolSession
	if err := db.Where("batch_id = ?", batch.ID).Order("session_id").Find(&memberships).Error; err != nil {
		t.Fatal(err)
	}
	if len(memberships) != 2 {
		t.Fatalf("memberships = %d, want 2", len(memberships))
	}

	built := make(chan uint, 1)
	runner := NewRunner(db,
		func(context.Context, uint) error { return nil },
		func(ctx context.Context, sessionID uint) error {
			if err := db.WithContext(ctx).Model(&model.SetupSession{}).Where("id = ?", sessionID).
				Update("status", model.StatusPoolWaiting).Error; err != nil {
				return err
			}
			built <- sessionID
			return nil
		},
		nil,
	)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	runner.Start(ctx, false)
	runner.Kick(memberships[0].SessionID)

	select {
	case sessionID := <-built:
		if sessionID != memberships[0].SessionID {
			t.Fatalf("built session %d, want %d", sessionID, memberships[0].SessionID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("explicitly queued pool session was not processed")
	}

	select {
	case sessionID := <-built:
		t.Fatalf("unqueued session %d was reconciled while recovery was disabled", sessionID)
	case <-time.After(500 * time.Millisecond):
	}
}

func TestRunnerDispatchesDurablePoolWork(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var membership model.PoolSession
	if err := db.First(&membership, "batch_id = ?", batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	prepared, built, cleaned := 0, 0, 0
	runner := NewRunner(db,
		func(context.Context, uint) error {
			prepared++
			return nil
		},
		func(_ context.Context, sessionID uint) error {
			built++
			return db.Model(&model.SetupSession{}).Where("id = ?", sessionID).
				Update("status", model.StatusPoolWaiting).Error
		},
		func(context.Context, uint) error {
			cleaned++
			return nil
		},
	)

	runner.runOne(context.Background(), membership.SessionID)
	if prepared != 1 || built != 1 || cleaned != 0 {
		t.Fatalf("build dispatch counts = prepare:%d build:%d cleanup:%d", prepared, built, cleaned)
	}
	if err := db.Model(&model.SetupSession{}).Where("id = ?", membership.SessionID).
		Update("status", "deleting").Error; err != nil {
		t.Fatal(err)
	}
	runner.runOne(context.Background(), membership.SessionID)
	if cleaned != 1 {
		t.Fatalf("cleanup calls = %d, want 1", cleaned)
	}
}
