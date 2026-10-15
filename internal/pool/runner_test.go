package pool

import (
	"context"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
)

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
