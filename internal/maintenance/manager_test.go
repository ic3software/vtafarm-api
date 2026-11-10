package maintenance

import (
	"context"
	"errors"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
)

func TestMaintenanceAdmissionAndTransitions(t *testing.T) {
	db := testutil.Postgres(t)
	manager, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	admin := model.Admin{UniqueId: "maintenance-admin"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}

	release, err := manager.Acquire(context.Background(), "config_update", "test-vta", false)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.CanEnable || len(snapshot.ActiveOperations) != 1 || snapshot.ActiveOperations[0].Target != "test-vta" {
		t.Fatalf("snapshot while admitted = %+v", snapshot)
	}
	if _, err := manager.Set(context.Background(), true, "upgrade", admin.ID); !errors.Is(err, ErrOperationsInProgress) {
		t.Fatalf("enable with active operation error = %v", err)
	}
	release()

	snapshot, err = manager.Set(context.Background(), true, "upgrade", admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Enabled || snapshot.Message != "upgrade" {
		t.Fatalf("enabled snapshot = %+v", snapshot)
	}
	if _, err := manager.Acquire(context.Background(), "vta_create", "new-vta", false); !errors.Is(err, ErrEnabled) {
		t.Fatalf("user admission during maintenance error = %v", err)
	}

	adminRelease, err := manager.Acquire(context.Background(), "image_upgrade", "Upgrade batch #1", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Set(context.Background(), false, "", admin.ID); !errors.Is(err, ErrOperationsInProgress) {
		t.Fatalf("disable with admin operation error = %v", err)
	}
	adminRelease()

	snapshot, err = manager.Set(context.Background(), false, "", admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Enabled {
		t.Fatalf("disabled snapshot = %+v", snapshot)
	}
}

func TestDurableWorkflowBlocksMaintenance(t *testing.T) {
	db := testutil.Postgres(t)
	manager, err := New(db)
	if err != nil {
		t.Fatal(err)
	}
	admin := model.Admin{UniqueId: "maintenance-admin"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	session := testutil.SetupSession(t, db, model.ModeVtaOnly)
	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.CanEnable || len(snapshot.ActiveOperations) != 0 {
		t.Fatalf("completed setup awaiting admin DID blocked maintenance: %+v", snapshot)
	}
	if err := db.Model(&session).Update("status", "vta_setup_running").Error; err != nil {
		t.Fatal(err)
	}

	snapshot, err = manager.Set(context.Background(), true, "upgrade", admin.ID)
	if !errors.Is(err, ErrOperationsInProgress) {
		t.Fatalf("enable with durable workflow error = %v", err)
	}
	if len(snapshot.ActiveOperations) != 1 || snapshot.ActiveOperations[0].Target != session.VtaName {
		t.Fatalf("active operations = %+v", snapshot.ActiveOperations)
	}

	if err := db.Model(&session).Update("status", "failed").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Set(context.Background(), true, "upgrade", admin.ID); err != nil {
		t.Fatalf("enable after workflow stopped: %v", err)
	}
}
