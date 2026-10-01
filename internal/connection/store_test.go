package connection

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
	"gorm.io/gorm"
)

func newDID(t *testing.T) string {
	t.Helper()
	did, err := didkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	return did
}
func mustRequest(t *testing.T, db *gorm.DB, s model.SetupSession) *model.MobileConnection {
	t.Helper()
	r, err := Change(context.Background(), db, s.ID, "create", "")
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestAdminDIDValidation(t *testing.T) {
	did := newDID(t)
	got, err := ValidateAdminDID(" \n" + did + " ")
	if err != nil || got != did {
		t.Fatalf("valid DID: %q %v", got, err)
	}
	for _, bad := range []string{"", "did:key:z123", did + "#fragment", "did:web:example.com", strings.Repeat("x", 1000)} {
		if _, err := ValidateAdminDID(bad); !errors.Is(err, ErrInvalidDID) {
			t.Errorf("accepted invalid DID %q", bad)
		}
	}
}

func TestConnectionTransactions(t *testing.T) {
	db := testutil.Postgres(t)
	ctx := context.Background()
	t.Run("concurrent same token accepts one DID", func(t *testing.T) {
		s := testutil.SetupSession(t, db, model.ModeVtaOnly)
		r := mustRequest(t, db, s)
		dids := []string{newDID(t), newDID(t)}
		start := make(chan struct{})
		errs := make([]error, 2)
		var wg sync.WaitGroup
		for i := range dids {
			wg.Add(1)
			go func(i int) { defer wg.Done(); <-start; _, errs[i] = AcceptMobile(ctx, db, r.ID, dids[i]) }(i)
		}
		close(start)
		wg.Wait()
		successes := 0
		for _, err := range errs {
			if err == nil {
				successes++
			} else if !errors.Is(err, ErrConflict) {
				t.Fatal(err)
			}
		}
		if successes != 1 {
			t.Fatalf("successful acceptances=%d errors=%v", successes, errs)
		}
		var op model.InitialProvision
		db.First(&op, "session_id = ?", s.ID)
		var count int64
		db.Model(&model.InitialProvision{}).Where("session_id = ?", s.ID).Count(&count)
		if count != 1 || op.AdminDid == "" {
			t.Fatalf("operation count=%d", count)
		}
		if _, err := AcceptMobile(ctx, db, r.ID, op.AdminDid); err != nil {
			t.Fatalf("duplicate: %v", err)
		}
		db.Model(r).Update("expires_at", time.Now().Add(-time.Minute))
		if _, err := AcceptMobile(ctx, db, r.ID, op.AdminDid); err != nil {
			t.Fatalf("accepted retry after QR expiry: %v", err)
		}
	})
	t.Run("manual and mobile compete for same VTA", func(t *testing.T) {
		s := testutil.SetupSession(t, db, model.ModeFullStack)
		r := mustRequest(t, db, s)
		did1, did2 := newDID(t), newDID(t)
		start := make(chan struct{})
		var e1, e2 error
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); <-start; _, e1 = AcceptMobile(ctx, db, r.ID, did1) }()
		go func() { defer wg.Done(); <-start; _, e2 = AcceptManual(ctx, db, s.ID, did2, false) }()
		close(start)
		wg.Wait()
		if (e1 == nil) == (e2 == nil) {
			t.Fatalf("both or neither accepted: %v / %v", e1, e2)
		}
		var op model.InitialProvision
		db.First(&op, "session_id = ?", s.ID)
		var saved model.SetupSession
		db.First(&saved, s.ID)
		if saved.AdminDid != op.AdminDid || saved.Status != "step_import_admin_did" {
			t.Fatalf("claim not durable: %+v", op)
		}
	})
	t.Run("refresh invalidates old token and stale tab", func(t *testing.T) {
		s := testutil.SetupSession(t, db, model.ModeVtaOnly)
		r := mustRequest(t, db, s)
		db.Model(r).Update("created_at", time.Now().Add(-10*time.Second))
		next, err := Change(ctx, db, s.ID, "refresh", r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if next.ID == r.ID {
			t.Fatal("token reused")
		}
		if _, err := AcceptMobile(ctx, db, r.ID, newDID(t)); !errors.Is(err, ErrExpired) {
			t.Fatalf("old token: %v", err)
		}
		if _, err := Change(ctx, db, s.ID, "cancel", r.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("stale cancellation: %v", err)
		}
	})
	t.Run("expiry is checked after lock acquisition", func(t *testing.T) {
		s := testutil.SetupSession(t, db, model.ModeVtaOnly)
		r := mustRequest(t, db, s)
		tx := db.Begin()
		if _, err := lockSession(tx, s.ID); err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		did := newDID(t)
		go func() { _, err := AcceptMobile(ctx, db, r.ID, did); done <- err }()
		if err := tx.Model(r).Update("expires_at", time.Now().Add(-time.Second)).Error; err != nil {
			t.Fatal(err)
		}
		tx.Commit()
		if err := <-done; !errors.Is(err, ErrExpired) {
			t.Fatalf("accepted expired request: %v", err)
		}
	})
	t.Run("transaction rollback leaves token usable", func(t *testing.T) {
		s := testutil.SetupSession(t, db, model.ModeVtaOnly)
		r := mustRequest(t, db, s)
		// Simulate a durable-work write conflict after the readiness check.
		op := model.InitialProvision{SessionID: s.ID, AdminDid: newDID(t), CreatedAt: time.Now()}
		db.Create(&op)
		if _, err := AcceptMobile(ctx, db, r.ID, newDID(t)); err == nil {
			t.Fatal("expected transaction failure")
		}
		var stored model.MobileConnection
		db.First(&stored, "id = ?", r.ID)
		var saved model.SetupSession
		db.First(&saved, s.ID)
		if stored.Status != "pending" || saved.AdminDid != "" {
			t.Fatal("partial acceptance survived rollback")
		}
	})
	t.Run("mobile completion requires readiness and live credential", func(t *testing.T) {
		s := testutil.SetupSession(t, db, model.ModeVtaOnly)
		r := mustRequest(t, db, s)
		did := newDID(t)
		if _, err := AcceptMobile(ctx, db, r.ID, did); err != nil {
			t.Fatal(err)
		}
		if err := Complete(ctx, db, r.ID); !errors.Is(err, ErrConflict) {
			t.Fatalf("early completion: %v", err)
		}
		db.Model(&s).Update("status", "running")
		if err := Complete(ctx, db, r.ID); err != nil {
			t.Fatal(err)
		}
		if err := Complete(ctx, db, r.ID); err != nil {
			t.Fatal("completion retry", err)
		}
		db.Model(r).Update("accepted_at", time.Now().Add(-2*ProgressLifetime))
		if err := Complete(ctx, db, r.ID); !errors.Is(err, ErrExpired) {
			t.Fatalf("expired completion: %v", err)
		}
		db.Model(r).Update("accepted_at", time.Now().Add(-2*RetryLifetime))
		if _, err := AcceptMobile(ctx, db, r.ID, did); !errors.Is(err, ErrExpired) {
			t.Fatalf("expired receipt retry: %v", err)
		}
	})
}

func TestTeardownRejectsNewAcceptance(t *testing.T) {
	db := testutil.Postgres(t)
	ctx := context.Background()
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	r := mustRequest(t, db, s)
	if err := Stop(ctx, db, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := AcceptMobile(ctx, db, r.ID, newDID(t)); !errors.Is(err, ErrExpired) {
		t.Fatalf("callback after teardown: %v", err)
	}
	if _, err := AcceptManual(ctx, db, s.ID, newDID(t), false); !errors.Is(err, ErrConflict) {
		t.Fatalf("manual after teardown: %v", err)
	}
	if _, err := Change(ctx, db, s.ID, "create", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("QR after teardown: %v", err)
	}
}
