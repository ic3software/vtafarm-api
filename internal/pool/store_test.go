package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

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
	result, err := Admit(context.Background(), db, Admission{
		AdminID:  admin.ID,
		Key:      "create-event-one",
		Digest:   "digest-one",
		Label:    "Event One",
		Count:    count,
		Capacity: available,
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
	if err != nil {
		return nil, false, err
	}
	return result.Batch, result.Created, nil
}

func holdWorkerLock(t *testing.T, db *gorm.DB, sessionID uint) func() {
	t.Helper()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := sqlDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(context.Background(), "SELECT pg_advisory_lock($1, $2)", workerLockClass, int64(sessionID)); err != nil {
		conn.Close()
		t.Fatal(err)
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1, $2)", workerLockClass, int64(sessionID))
			_ = conn.Close()
		})
	}
	t.Cleanup(release)
	return release
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
		if len(session.VtaName) != 12 {
			t.Fatalf("pool VTA name %q has length %d, want 12", session.VtaName, len(session.VtaName))
		}
		var user model.User
		if err := db.First(&user, session.UserID).Error; err != nil {
			t.Fatal(err)
		}
		if len(user.UniqueId) != 8 {
			t.Fatalf("pool user ID %q has length %d, want 8", user.UniqueId, len(user.UniqueId))
		}
	}

	retried, created, err := admission(t, db, 2, 0)
	if err != nil || created || retried.ID != batch.ID {
		t.Fatalf("idempotent Admit() = %#v, %v, %v", retried, created, err)
	}
}

func TestAdmitPublicSignupStartsEnabled(t *testing.T) {
	db := poolDatabase(t)
	admin := model.Admin{UniqueId: "public-auto"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	result, err := Admit(context.Background(), db, Admission{
		AdminID: admin.ID, Key: "public-auto", Digest: "public-auto", Label: "Public", Purpose: model.PoolPurposePublicSignup,
		Count: 1, Capacity: 1,
		Session: model.SetupSession{
			Mode: model.ModeVtaOnly, Domain: "example.com", Subdomain: "vta-",
			VtaImage: "example/vta:test", DidHostingServerURL: "https://dids.example.com",
		},
		MemoryRequest: "32Mi", MemoryLimit: "64Mi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Batch.PublicSignupEnabledAt == nil {
		t.Fatal("public signup batch was created paused")
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

func TestAdmitBulkCreatesRequestedSizes(t *testing.T) {
	for _, count := range []int{1, 20, 50} {
		t.Run(fmt.Sprintf("count_%d", count), func(t *testing.T) {
			db := poolDatabase(t)
			started := time.Now()
			batch, created, err := admission(t, db, count, count)
			t.Logf("Admit(%d) completed in %s", count, time.Since(started))
			if err != nil || !created {
				t.Fatalf("Admit(%d) created=%v error=%v", count, created, err)
			}
			var sessions, users, memberships, resources int64
			if err := db.Model(&model.PoolSession{}).Where("batch_id = ?", batch.ID).Count(&memberships).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&model.SetupSession{}).
				Where("id IN (SELECT session_id FROM pool_sessions WHERE batch_id = ?)", batch.ID).Count(&sessions).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&model.User{}).
				Where("id IN (SELECT user_id FROM setup_sessions WHERE id IN (SELECT session_id FROM pool_sessions WHERE batch_id = ?))", batch.ID).
				Count(&users).Error; err != nil {
				t.Fatal(err)
			}
			if err := db.Model(&model.WorkloadResource{}).
				Where("setup_session_id IN (SELECT session_id FROM pool_sessions WHERE batch_id = ?)", batch.ID).Count(&resources).Error; err != nil {
				t.Fatal(err)
			}
			if sessions != int64(count) || users != int64(count) || memberships != int64(count) || resources != int64(count) {
				t.Fatalf("rows sessions=%d users=%d memberships=%d resources=%d, want %d each", sessions, users, memberships, resources, count)
			}
		})
	}
}

func TestAdmitBulkWriteFailureRollsBackEverything(t *testing.T) {
	db := poolDatabase(t)
	if err := db.Exec(`
		CREATE FUNCTION reject_pool_workload() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected workload failure';
		END $$;
		CREATE TRIGGER reject_pool_workload BEFORE INSERT ON workload_resources
		FOR EACH STATEMENT EXECUTE FUNCTION reject_pool_workload();`).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := admission(t, db, 20, 20); err == nil {
		t.Fatal("Admit() error = nil, want injected bulk write failure")
	}
	for name, target := range map[string]any{
		"batches": &model.PoolBatch{}, "users": &model.User{}, "sessions": &model.SetupSession{},
		"memberships": &model.PoolSession{}, "resources": &model.WorkloadResource{},
	} {
		var count int64
		if err := db.Model(target).Count(&count).Error; err != nil {
			t.Fatal(err)
		}
		if count != 0 {
			t.Fatalf("rollback left %d %s", count, name)
		}
	}
}

func TestAdmitConcurrentCapacityCannotBeExceeded(t *testing.T) {
	db := poolDatabase(t)
	admin := model.Admin{UniqueId: "pool-concur"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errs := make(chan error, 2)
	for i := range 2 {
		go func(index int) {
			<-start
			_, err := Admit(context.Background(), db, Admission{
				AdminID: admin.ID, Key: fmt.Sprintf("concurrent-%d", index), Digest: fmt.Sprintf("digest-%d", index),
				Label: "Concurrent", Count: 2, Capacity: 3,
				Session: model.SetupSession{
					Mode: model.ModeVtaOnly, Domain: "example.com", Subdomain: "vta-",
					VtaImage: "example/vta:test", DidHostingServerURL: "https://dids.example.com",
				},
				MemoryRequest: "32Mi", MemoryLimit: "64Mi",
			})
			errs <- err
		}(i)
	}
	close(start)
	var accepted, rejected int
	for range 2 {
		err := <-errs
		switch {
		case err == nil:
			accepted++
		case errors.Is(err, ErrCapacity):
			rejected++
		default:
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted=%d rejected=%d, want 1 each", accepted, rejected)
	}
	var memberships int64
	if err := db.Model(&model.PoolSession{}).Count(&memberships).Error; err != nil {
		t.Fatal(err)
	}
	if memberships != 2 {
		t.Fatalf("memberships = %d, want 2", memberships)
	}
}

func TestAdmitReplenishesIdentifierCollisions(t *testing.T) {
	db := poolDatabase(t)
	existingUser := model.User{UniqueId: "abcdefgh"}
	if err := db.Create(&existingUser).Error; err != nil {
		t.Fatal(err)
	}
	existingSession := testutil.SetupSession(t, db, model.ModeVtaOnly)

	accountIDs := []string{"abcdefgh", "ijklmnop", "qrstuvwx"}
	vtaNames := []string{existingSession.VtaName, "aaaaaaaaaaaa", "bbbbbbbbbbbb"}
	originalAccountID, originalVTAName := newAccountID, newVTAName
	newAccountID = func() (string, error) {
		value := accountIDs[0]
		accountIDs = accountIDs[1:]
		return value, nil
	}
	newVTAName = func() (string, error) {
		value := vtaNames[0]
		vtaNames = vtaNames[1:]
		return value, nil
	}
	t.Cleanup(func() {
		newAccountID, newVTAName = originalAccountID, originalVTAName
	})

	admin := model.Admin{UniqueId: "pool-collide"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	result, err := Admit(context.Background(), db, Admission{
		AdminID: admin.ID, Key: "collision", Digest: "collision", Label: "Collision", Count: 2, Capacity: 2,
		Session: model.SetupSession{
			Mode: model.ModeVtaOnly, Domain: "example.com", Subdomain: "vta-",
			VtaImage: "example/vta:test", DidHostingServerURL: "https://dids.example.com",
		},
		MemoryRequest: "32Mi", MemoryLimit: "64Mi",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Sessions) != 2 {
		t.Fatalf("returned sessions = %d, want 2", len(result.Sessions))
	}
	for i := range result.Sessions {
		if result.Sessions[i].VtaName == existingSession.VtaName {
			t.Fatalf("reused colliding VTA name %q", existingSession.VtaName)
		}
	}
}

func TestQueueBatchCleanupIsAtomicAndRetryable(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	var memberships []model.PoolSession
	if err := db.Where("batch_id = ?", batch.ID).Order("session_id").Find(&memberships).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SetupSession{}).
		Where("id IN ?", []uint{memberships[0].SessionID, memberships[1].SessionID}).
		Update("status", model.StatusPoolWaiting).Error; err != nil {
		t.Fatal(err)
	}
	var protected model.SetupSession
	if err := db.First(&protected, memberships[1].SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.User{}).Where("id = ?", protected.UserID).
		Update("email", "claimed@example.com").Error; err != nil {
		t.Fatal(err)
	}
	if _, err := QueueBatchCleanup(context.Background(), db, batch.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("QueueBatchCleanup(protected) error = %v, want ErrConflict", err)
	}
	var deleting int64
	if err := db.Model(&model.SetupSession{}).Where("status = ?", "deleting").Count(&deleting).Error; err != nil {
		t.Fatal(err)
	}
	if deleting != 0 {
		t.Fatalf("atomic rejection left %d deleting sessions", deleting)
	}
	if err := db.Model(&model.User{}).Where("id = ?", protected.UserID).
		Update("email", nil).Error; err != nil {
		t.Fatal(err)
	}
	sessionIDs, err := QueueBatchCleanup(context.Background(), db, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(sessionIDs) != 2 {
		t.Fatalf("queued session IDs = %v, want 2", sessionIDs)
	}
	if err := db.First(batch, batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if batch.CleanupRequestedAt == nil {
		t.Fatal("cleanup_requested_at is nil")
	}
	retried, err := QueueBatchCleanup(context.Background(), db, batch.ID)
	if err != nil || len(retried) != 2 {
		t.Fatalf("QueueBatchCleanup(retry) = %v, %v", retried, err)
	}
}

func TestQueueBatchCleanupSkipsWorkerLockForWaitingSession(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var membership model.PoolSession
	if err := db.First(&membership, "batch_id = ?", batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SetupSession{}).Where("id = ?", membership.SessionID).
		Update("status", model.StatusPoolWaiting).Error; err != nil {
		t.Fatal(err)
	}
	release := holdWorkerLock(t, db, membership.SessionID)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := QueueBatchCleanup(ctx, db, batch.ID); err != nil {
		t.Fatalf("QueueBatchCleanup(waiting) waited for worker lock: %v", err)
	}
}

func TestQueueBatchCleanupRejectsPreparingSessionWithoutWaitingForWorker(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var membership model.PoolSession
	if err := db.First(&membership, "batch_id = ?", batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	release := holdWorkerLock(t, db, membership.SessionID)
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if _, err := QueueBatchCleanup(ctx, db, batch.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("QueueBatchCleanup(preparing) error = %v, want ErrConflict", err)
	}
	var session model.SetupSession
	if err := db.First(&session, membership.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if session.Status != model.StatusPoolCreating {
		t.Fatalf("preparing session status = %q, want %q", session.Status, model.StatusPoolCreating)
	}
}

func TestQueueCleanupSkipsWorkerLockForWaitingSessions(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	var memberships []model.PoolSession
	if err := db.Where("batch_id = ?", batch.ID).Order("session_id").Find(&memberships).Error; err != nil {
		t.Fatal(err)
	}
	sessionIDs := []uint{memberships[0].SessionID, memberships[1].SessionID}
	if err := db.Model(&model.SetupSession{}).Where("id IN ?", sessionIDs).
		Update("status", model.StatusPoolWaiting).Error; err != nil {
		t.Fatal(err)
	}
	release := holdWorkerLock(t, db, sessionIDs[0])
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := QueueCleanup(ctx, db, sessionIDs); err != nil {
		t.Fatalf("QueueCleanup(waiting) waited for worker lock: %v", err)
	}
	var deleting int64
	if err := db.Model(&model.SetupSession{}).
		Where("id IN ? AND status = ?", sessionIDs, "deleting").Count(&deleting).Error; err != nil {
		t.Fatal(err)
	}
	if deleting != 2 {
		t.Fatalf("deleting sessions = %d, want 2", deleting)
	}
}

func TestQueueCleanupRejectsNonPoolAndDuplicateSessions(t *testing.T) {
	db := poolDatabase(t)
	session := testutil.SetupSession(t, db, model.ModeVtaOnly)
	if err := QueueCleanup(context.Background(), db, []uint{session.ID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("QueueCleanup(non-pool) error = %v, want ErrConflict", err)
	}
	var unchanged model.SetupSession
	if err := db.First(&unchanged, session.ID).Error; err != nil {
		t.Fatal(err)
	}
	if unchanged.Status != session.Status {
		t.Fatalf("non-pool status = %q, want %q", unchanged.Status, session.Status)
	}

	batch, _, err := admission(t, db, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var membership model.PoolSession
	if err := db.First(&membership, "batch_id = ?", batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := QueueCleanup(context.Background(), db, []uint{membership.SessionID, membership.SessionID}); !errors.Is(err, ErrConflict) {
		t.Fatalf("QueueCleanup(duplicate) error = %v, want ErrConflict", err)
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

func TestReserveDoesNotReuseActiveClaim(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SetupSession{}).
		Where("id IN (SELECT session_id FROM pool_sessions WHERE batch_id = ?)", batch.ID).
		Updates(map[string]any{"status": model.StatusPoolWaiting, "vta_did": "did:webvh:example.com:test"}).Error; err != nil {
		t.Fatal(err)
	}
	first, err := Reserve(context.Background(), db, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Reserve(context.Background(), db, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if first.SessionID == second.SessionID {
		t.Fatalf("two active claims used session %d", first.SessionID)
	}
}

func TestReservePublicIsExclusiveAndIdempotent(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(batch).Update("purpose", model.PoolPurposePublicSignup).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := SetPublicSignup(context.Background(), db, batch.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SetupSession{}).
		Where("id IN (SELECT session_id FROM pool_sessions WHERE batch_id = ?)", batch.ID).
		Updates(map[string]any{"status": model.StatusPoolWaiting, "vta_did": "did:webvh:example.com:test"}).Error; err != nil {
		t.Fatal(err)
	}

	digestA := make([]byte, 32)
	digestB := make([]byte, 32)
	digestA[0], digestB[0] = 1, 2
	type reservation struct {
		claim   *model.MobileConnection
		created bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan reservation, 2)
	for _, digest := range [][]byte{digestA, digestB} {
		go func(capability []byte) {
			<-start
			claim, created, err := ReservePublic(context.Background(), db, capability)
			results <- reservation{claim: claim, created: created, err: err}
		}(digest)
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || !first.created || second.err != nil || !second.created {
		t.Fatalf("concurrent ReservePublic() = %+v and %+v", first, second)
	}
	if second.claim.SessionID == first.claim.SessionID {
		t.Fatalf("distinct public claims used session %d", first.claim.SessionID)
	}
	replayed, created, err := ReservePublic(context.Background(), db, digestA)
	if err != nil || created {
		t.Fatalf("ReservePublic(replay) = %+v, %v, %v", replayed, created, err)
	}
	if string(replayed.PublicAccessDigest) != string(digestA) {
		t.Fatalf("replayed digest = %x, want %x", replayed.PublicAccessDigest, digestA)
	}
	if _, err := SetPublicSignup(context.Background(), db, batch.ID, false); err != nil {
		t.Fatal(err)
	}
	replayed, created, err = ReservePublic(context.Background(), db, digestA)
	if err != nil || created {
		t.Fatalf("ReservePublic(replay after pause) = %+v, %v, %v", replayed, created, err)
	}
}

func TestReservePublicRequiresEnabledPublicBatch(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SetupSession{}).
		Where("id IN (SELECT session_id FROM pool_sessions WHERE batch_id = ?)", batch.ID).
		Updates(map[string]any{"status": model.StatusPoolWaiting, "vta_did": "did:webvh:example.com:test"}).Error; err != nil {
		t.Fatal(err)
	}
	digest := make([]byte, 32)
	if _, _, err := ReservePublic(context.Background(), db, digest); !errors.Is(err, ErrPublicDisabled) {
		t.Fatalf("ReservePublic(admin batch) error = %v, want ErrPublicDisabled", err)
	}
	if err := db.Model(batch).Update("purpose", model.PoolPurposePublicSignup).Error; err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReservePublic(context.Background(), db, digest); !errors.Is(err, ErrPublicDisabled) {
		t.Fatalf("ReservePublic(paused batch) error = %v, want ErrPublicDisabled", err)
	}
	if _, err := SetPublicSignup(context.Background(), db, batch.ID, true); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReservePublic(context.Background(), db, digest); err != nil {
		t.Fatalf("ReservePublic(enabled batch) error = %v", err)
	}
}

func TestReservePublicConcurrentReplayCreatesOneClaim(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(batch).Update("purpose", model.PoolPurposePublicSignup).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := SetPublicSignup(context.Background(), db, batch.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SetupSession{}).
		Where("id IN (SELECT session_id FROM pool_sessions WHERE batch_id = ?)", batch.ID).
		Updates(map[string]any{"status": model.StatusPoolWaiting, "vta_did": "did:webvh:example.com:test"}).Error; err != nil {
		t.Fatal(err)
	}
	digest := make([]byte, 32)
	digest[0] = 9
	type result struct {
		claim   *model.MobileConnection
		created bool
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for range 2 {
		go func() {
			<-start
			claim, created, err := ReservePublic(context.Background(), db, digest)
			results <- result{claim: claim, created: created, err: err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.claim.ID != second.claim.ID {
		t.Fatalf("concurrent replay = %+v and %+v", first, second)
	}
	if first.created == second.created {
		t.Fatalf("created flags = %v and %v, want exactly one true", first.created, second.created)
	}
	var claims int64
	if err := db.Model(&model.MobileConnection{}).Count(&claims).Error; err != nil {
		t.Fatal(err)
	}
	if claims != 1 {
		t.Fatalf("mobile connections = %d, want 1", claims)
	}
}

func TestAddInventoryGrowsBatchIdempotently(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	var admin model.Admin
	if err := db.First(&admin, "unique_id = ?", "pool-admin").Error; err != nil {
		t.Fatal(err)
	}
	input := Addition{
		AdminID: admin.ID, BatchID: batch.ID, Key: "add-two", Digest: "add-two-digest",
		Count: 2, Capacity: 3,
		Session: model.SetupSession{
			Mode: model.ModeVtaOnly, Domain: "example.com", Subdomain: "vta-",
			VtaImage: "example/vta:test", DidHostingServerURL: "https://dids.example.com",
		},
		MemoryRequest: "32Mi", MemoryLimit: "64Mi",
	}
	added, err := AddInventory(context.Background(), db, input)
	if err != nil || !added.Created || len(added.Sessions) != 2 || added.Batch.RequestedCount != 3 {
		t.Fatalf("AddInventory() = %+v, %v", added, err)
	}
	replayed, err := AddInventory(context.Background(), db, input)
	if err != nil || replayed.Created || replayed.Batch.RequestedCount != 3 {
		t.Fatalf("AddInventory(replay) = %+v, %v", replayed, err)
	}
	var memberships int64
	if err := db.Model(&model.PoolSession{}).Where("batch_id = ?", batch.ID).Count(&memberships).Error; err != nil {
		t.Fatal(err)
	}
	if memberships != 3 {
		t.Fatalf("memberships = %d, want 3", memberships)
	}
}

func TestRejectCancelsOpenAndRejectsClaimedPoolRequests(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SetupSession{}).
		Where("id IN (SELECT session_id FROM pool_sessions WHERE batch_id = ?)", batch.ID).
		Updates(map[string]any{"status": model.StatusPoolWaiting, "vta_did": "did:webvh:example.com:test"}).Error; err != nil {
		t.Fatal(err)
	}

	openRequest, err := Reserve(context.Background(), db, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := Reject(context.Background(), db, openRequest.ID); err != nil {
		t.Fatalf("cancel open request: %v", err)
	}
	if err := db.First(openRequest, "id = ?", openRequest.ID).Error; err != nil {
		t.Fatal(err)
	}
	if openRequest.Status != "lapsed" {
		t.Fatalf("cancelled request status = %q, want lapsed", openRequest.Status)
	}

	claimedRequest, err := Reserve(context.Background(), db, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	did, err := didkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Claim(context.Background(), db, *claimedRequest.ClaimHandle, did); err != nil {
		t.Fatal(err)
	}
	if err := Reject(context.Background(), db, claimedRequest.ID); err != nil {
		t.Fatalf("reject claimed request: %v", err)
	}
	if err := db.First(claimedRequest, "id = ?", claimedRequest.ID).Error; err != nil {
		t.Fatal(err)
	}
	if claimedRequest.Status != "rejected" {
		t.Fatalf("rejected request status = %q, want rejected", claimedRequest.Status)
	}
}

func TestExpiredClaimCanBeReplacedAndCleaned(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var membership model.PoolSession
	if err := db.First(&membership, "batch_id = ?", batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SetupSession{}).Where("id = ?", membership.SessionID).
		Updates(map[string]any{"status": model.StatusPoolWaiting, "vta_did": "did:webvh:example.com:test"}).Error; err != nil {
		t.Fatal(err)
	}
	first, err := Reserve(context.Background(), db, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-10 * time.Minute)
	if err := db.Model(first).Updates(map[string]any{"created_at": past, "expires_at": past}).Error; err != nil {
		t.Fatal(err)
	}
	second, err := Reserve(context.Background(), db, batch.ID)
	if err != nil {
		t.Fatal(err)
	}
	if second.ID == first.ID || second.SessionID != first.SessionID {
		t.Fatalf("replacement claim = %+v, first = %+v", second, first)
	}
	if err := db.Model(second).Updates(map[string]any{"created_at": past, "expires_at": past}).Error; err != nil {
		t.Fatal(err)
	}
	if err := QueueCleanup(context.Background(), db, []uint{membership.SessionID}); err != nil {
		t.Fatalf("QueueCleanup(expired claim) error = %v", err)
	}
	var session model.SetupSession
	if err := db.First(&session, membership.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if session.Status != "deleting" {
		t.Fatalf("status = %q, want deleting", session.Status)
	}
}

func TestRetryBuildProtectsClaimedSession(t *testing.T) {
	db := poolDatabase(t)
	batch, _, err := admission(t, db, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	var membership model.PoolSession
	if err := db.First(&membership, "batch_id = ?", batch.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.SetupSession{}).Where("id = ?", membership.SessionID).
		Updates(map[string]any{"status": "failed", "error_msg": "temporary"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := RetryBuild(context.Background(), db, membership.SessionID); err != nil {
		t.Fatal(err)
	}
	var session model.SetupSession
	if err := db.First(&session, membership.SessionID).Error; err != nil {
		t.Fatal(err)
	}
	if session.Status != model.StatusPoolCreating || session.ErrorMsg != "" {
		t.Fatalf("retried session = %+v", session)
	}
	if err := db.Model(&session).Updates(map[string]any{"status": "failed", "admin_did": "did:key:z6Mktest"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := RetryBuild(context.Background(), db, membership.SessionID); !errors.Is(err, ErrConflict) {
		t.Fatalf("RetryBuild(claimed) error = %v, want ErrConflict", err)
	}
}
