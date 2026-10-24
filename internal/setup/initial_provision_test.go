package setup

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
)

func TestProvisionAdminLabelRecognizesWalletClaimLifecycle(t *testing.T) {
	db := testutil.Postgres(t)
	now := time.Now()
	tests := []struct {
		status string
		want   string
	}{
		{status: "accepted", want: connection.MobileAdminLabel},
		{status: "confirmed", want: connection.MobileAdminLabel},
		{status: "provisioning", want: connection.MobileAdminLabel},
		{status: "released", want: connection.MobileAdminLabel},
		{status: "claimed", want: "fallback"},
	}
	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			s := testutil.SetupSession(t, db, model.ModeVtaOnly)
			r := model.MobileConnection{
				ID:        uuid.NewString(),
				SessionID: s.ID,
				VtaDid:    s.VtaDid,
				Operation: connection.OperationProvisionVTA,
				AdminDid:  "did:key:z6MkAdmin",
				Status:    tt.status,
				CreatedAt: now,
				ExpiresAt: now.Add(time.Minute),
			}
			if tt.status == "claimed" {
				r.ClaimedAt = &now
				r.ConfirmBy = &now
			}
			if tt.status == "confirmed" {
				r.ConfirmedAt = &now
			}
			if tt.status == "released" {
				r.ReleasedAt = &now
			}
			if err := db.Create(&r).Error; err != nil {
				t.Fatal(err)
			}
			o := &Orchestrator{db: db}
			if got := o.provisionAdminLabel(s.ID, r.AdminDid, "fallback"); got != tt.want {
				t.Fatalf("label = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProvisionRecoveryAndReplicaExclusion(t *testing.T) {
	db := testutil.Postgres(t)
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	did, err := didkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if err := connection.AcceptManual(context.Background(), db, s.ID, did, false); err != nil {
		t.Fatal(err)
	}
	// No worker was started before this new orchestrator instance recovers.
	first, second := &Orchestrator{db: db}, &Orchestrator{db: db}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	execute := func(ctx context.Context, s *model.SetupSession, got string) {
		calls.Add(1)
		if got != did {
			t.Errorf("recovery changed DID")
		}
		close(entered)
		<-release
		if err := db.WithContext(ctx).Model(s).Update("status", "running").Error; err != nil {
			t.Error(err)
		}
	}
	go func() { defer close(done); first.runProvisionOperation(context.Background(), s.ID, execute) }()
	<-entered
	second.runProvisionOperation(context.Background(), s.ID, func(context.Context, *model.SetupSession, string) { calls.Add(1) })
	if calls.Load() != 1 {
		t.Fatal("two replicas executed the same operation")
	}
	close(release)
	<-done
	second.runProvisionOperation(context.Background(), s.ID, func(context.Context, *model.SetupSession, string) { calls.Add(1) })
	if calls.Load() != 1 {
		t.Fatal("completed work was repeated")
	}
}

func TestProvisionResumesAfterWorkerCancellation(t *testing.T) {
	db := testutil.Postgres(t)
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	did, _ := didkey.Generate()
	if err := connection.AcceptManual(context.Background(), db, s.ID, did, false); err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{db: db}
	ctx, cancel := context.WithCancel(context.Background())
	o.runProvisionOperation(ctx, s.ID, func(context.Context, *model.SetupSession, string) { cancel() })
	recovered := false
	o.runProvisionOperation(context.Background(), s.ID, func(ctx context.Context, s *model.SetupSession, got string) {
		recovered = got == did
		db.WithContext(ctx).Model(s).Update("status", "running")
	})
	if !recovered {
		t.Fatal("work was not recovered")
	}
}

func TestTeardownStopsWorkerOnAnotherReplica(t *testing.T) {
	db := testutil.Postgres(t)
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	did, _ := didkey.Generate()
	if err := connection.AcceptManual(context.Background(), db, s.ID, did, false); err != nil {
		t.Fatal(err)
	}
	worker, deleter := &Orchestrator{db: db}, &Orchestrator{db: db}
	entered, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		worker.runProvisionOperation(context.Background(), s.ID, func(ctx context.Context, _ *model.SetupSession, _ string) {
			close(entered)
			<-ctx.Done()
		})
	}()
	<-entered
	if err := deleter.Cancel(s.ID); err != nil {
		t.Fatal(err)
	}
	<-done
	worker.runProvisionOperation(context.Background(), s.ID, func(context.Context, *model.SetupSession, string) {
		t.Error("teardown work restarted")
	})
}

func TestTeardownClearsAbandonedRegistryMaintenanceLease(t *testing.T) {
	db := testutil.Postgres(t)
	s := testutil.SetupSession(t, db, model.ModeFullStack)
	started := time.Now().Add(-time.Minute)
	if err := db.Model(&s).Updates(map[string]any{
		"status":                 "running",
		"trust_registry_enabled": true,
		"trust_registry_state":   "enabling",
	}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.VtaAclSnapshot{
		SessionID: s.ID, MaintenanceStartedAt: &started,
	}).Error; err != nil {
		t.Fatal(err)
	}

	o := &Orchestrator{db: db}
	if err := o.Cancel(s.ID); err != nil {
		t.Fatal(err)
	}

	var got model.SetupSession
	if err := db.First(&got, s.ID).Error; err != nil {
		t.Fatal(err)
	}
	if got.Status != "deleting" {
		t.Fatalf("status = %q, want deleting", got.Status)
	}
	var snapshot model.VtaAclSnapshot
	if err := db.First(&snapshot, "session_id = ?", s.ID).Error; err != nil {
		t.Fatal(err)
	}
	if snapshot.MaintenanceStartedAt != nil {
		t.Fatalf("maintenance lease was not cleared: %v", snapshot.MaintenanceStartedAt)
	}
}
