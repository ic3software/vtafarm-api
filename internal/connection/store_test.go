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
		var saved model.SetupSession
		db.First(&saved, s.ID)
		var accepted model.MobileConnection
		db.First(&accepted, "id = ?", r.ID)
		if saved.AdminDid == "" || accepted.Operation != OperationProvisionVTA || accepted.AdminDid != saved.AdminDid {
			t.Fatalf("claim not stored on session/request: %+v %+v", saved, accepted)
		}
		if _, err := AcceptMobile(ctx, db, r.ID, saved.AdminDid); err != nil {
			t.Fatalf("duplicate: %v", err)
		}
		db.Model(r).Update("expires_at", time.Now().Add(-time.Minute))
		if _, err := AcceptMobile(ctx, db, r.ID, saved.AdminDid); err != nil {
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
		go func() { defer wg.Done(); <-start; e2 = AcceptManual(ctx, db, s.ID, did2, false) }()
		close(start)
		wg.Wait()
		if (e1 == nil) == (e2 == nil) {
			t.Fatalf("both or neither accepted: %v / %v", e1, e2)
		}
		var saved model.SetupSession
		db.First(&saved, s.ID)
		if saved.AdminDid == "" || saved.Status != "step_import_admin_did" {
			t.Fatalf("claim not durable: %+v", saved)
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
	if err := AcceptManual(ctx, db, s.ID, newDID(t), false); !errors.Is(err, ErrConflict) {
		t.Fatalf("manual after teardown: %v", err)
	}
	if _, err := Change(ctx, db, s.ID, "create", ""); !errors.Is(err, ErrConflict) {
		t.Fatalf("QR after teardown: %v", err)
	}
}

func TestAdditionalMobileConnection(t *testing.T) {
	db := testutil.Postgres(t)
	ctx := context.Background()
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	if err := db.Model(&s).Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}

	r, err := Change(ctx, db, s.ID, "create", "")
	if err != nil {
		t.Fatal(err)
	}
	did := newDID(t)
	accepted, err := AcceptMobile(ctx, db, r.ID, did)
	if err != nil {
		t.Fatal(err)
	}
	if accepted.Operation != OperationGrantACL || accepted.AdminDid != did {
		t.Fatalf("unexpected accepted request: %+v", accepted)
	}
	var saved model.SetupSession
	db.First(&saved, s.ID)
	if saved.AdminDid != "" {
		t.Fatal("running-session connection replaced the setup Admin DID")
	}
	if err := Complete(ctx, db, r.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("completed before ACL grant: %v", err)
	}
	now := time.Now()
	if err := db.Model(accepted).Update("provisioned_at", now).Error; err != nil {
		t.Fatal(err)
	}
	if err := Complete(ctx, db, r.ID); err != nil {
		t.Fatal(err)
	}

	if err := db.Model(accepted).Update("created_at", time.Now().Add(-10*time.Second)).Error; err != nil {
		t.Fatal(err)
	}
	next, err := Change(ctx, db, s.ID, "create", "")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == r.ID || next.Status != "pending" {
		t.Fatalf("new device did not receive a fresh request: %+v", next)
	}
}

func TestAdditionalAwaitingMobileCanBeRevoked(t *testing.T) {
	db := testutil.Postgres(t)
	ctx := context.Background()

	newAccepted := func(t *testing.T) (model.SetupSession, *model.MobileConnection, string) {
		t.Helper()
		s := testutil.SetupSession(t, db, model.ModeVtaOnly)
		if err := db.Model(&s).Update("status", "running").Error; err != nil {
			t.Fatal(err)
		}
		r, err := Change(ctx, db, s.ID, "create", "")
		if err != nil {
			t.Fatal(err)
		}
		did := newDID(t)
		if _, err := AcceptMobile(ctx, db, r.ID, did); err != nil {
			t.Fatal(err)
		}
		return s, r, did
	}

	t.Run("refresh replaces the attempt after ACL provisioning", func(t *testing.T) {
		s, r, did := newAccepted(t)
		current, err := Change(ctx, db, s.ID, "refresh", r.ID)
		if err != nil || current.ID != r.ID || current.Status != "accepted" {
			t.Fatalf("provisioning attempt was replaced: %+v %v", current, err)
		}
		if err := db.Model(r).Update("provisioned_at", time.Now()).Error; err != nil {
			t.Fatal(err)
		}
		next, err := Change(ctx, db, s.ID, "refresh", r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if next.ID == r.ID || next.Status != "pending" {
			t.Fatalf("awaiting attempt was not replaced: %+v", next)
		}
		if _, err := AcceptMobile(ctx, db, r.ID, did); !errors.Is(err, ErrExpired) {
			t.Fatalf("replaced callback remained valid: %v", err)
		}
		if err := Complete(ctx, db, r.ID); !errors.Is(err, ErrExpired) {
			t.Fatalf("replaced progress credential remained valid: %v", err)
		}
	})

	t.Run("cancel invalidates the attempt after ACL provisioning", func(t *testing.T) {
		s, r, did := newAccepted(t)
		if err := db.Model(r).Update("provisioned_at", time.Now()).Error; err != nil {
			t.Fatal(err)
		}
		cancelled, err := Change(ctx, db, s.ID, "cancel", r.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cancelled.ID != r.ID || cancelled.Status != "cancelled" {
			t.Fatalf("unexpected cancelled attempt: %+v", cancelled)
		}
		if _, err := AcceptMobile(ctx, db, r.ID, did); !errors.Is(err, ErrExpired) {
			t.Fatalf("cancelled callback remained valid: %v", err)
		}
		if err := Complete(ctx, db, r.ID); !errors.Is(err, ErrExpired) {
			t.Fatalf("cancelled progress credential remained valid: %v", err)
		}
	})
}
