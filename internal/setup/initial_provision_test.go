package setup

import (
	"context"
	"sync/atomic"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
)

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
