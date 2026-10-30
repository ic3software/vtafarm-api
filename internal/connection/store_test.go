package connection

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
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
	if r.Status != "open" || r.ClaimHandle == nil || len(*r.ClaimHandle) != 22 {
		t.Fatalf("invalid reservation: %+v", r)
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

func TestClaimCheckVector(t *testing.T) {
	const did = "did:key:z6MkpTHR8VNsBxYAAWHut2Geadd9jSwuBV8xRoAnwWsdvktH"
	if got := ClaimCheck(did); got != "WNLAZG" {
		t.Fatalf("claim check = %s", got)
	}
}

func TestClaimRequiresOwnerConfirmation(t *testing.T) {
	db := testutil.Postgres(t)
	ctx := context.Background()
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	r := mustRequest(t, db, s)
	dids := []string{newDID(t), newDID(t)}
	errValues := make([]error, len(dids))
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := range dids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errValues[i] = Claim(ctx, db, *r.ClaimHandle, dids[i])
		}(i)
	}
	close(start)
	wg.Wait()

	successes := 0
	for _, err := range errValues {
		if err == nil {
			successes++
		} else if !errors.Is(err, ErrAlreadyClaimed) {
			t.Fatal(err)
		}
	}
	if successes != 1 {
		t.Fatalf("successful claims=%d errors=%v", successes, errValues)
	}

	var claimed model.MobileConnection
	if err := db.First(&claimed, "id = ?", r.ID).Error; err != nil {
		t.Fatal(err)
	}
	var before model.SetupSession
	db.First(&before, s.ID)
	if claimed.Status != "claimed" || claimed.AdminDid == "" || before.AdminDid != "" {
		t.Fatalf("claim crossed confirmation boundary: request=%+v session=%+v", claimed, before)
	}

	confirmed, err := Confirm(ctx, db, s.ID, r.ID)
	if err != nil || confirmed.Status != "confirmed" {
		t.Fatalf("confirm: %+v %v", confirmed, err)
	}
	db.First(&before, s.ID)
	if before.AdminDid != "" {
		t.Fatal("confirmation started provisioning inside its transaction")
	}
	started, err := StartProvisioning(ctx, db, r.ID)
	if err != nil || started.Status != "provisioning" {
		t.Fatalf("start provisioning: %+v %v", started, err)
	}
	var after model.SetupSession
	db.First(&after, s.ID)
	if after.AdminDid != claimed.AdminDid || after.Status != "provisioning" {
		t.Fatalf("confirmed key not installed in setup session: %+v", after)
	}
	if err := db.Model(&after).Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}
	after.Status = "running"
	if err := Reconcile(ctx, db, started, &after); err != nil || started.Status != "released" {
		t.Fatalf("release: %+v %v", started, err)
	}
	var binding model.WalletLoginBinding
	if err := db.First(&binding, "source_claim_id = ?", r.ID).Error; err != nil {
		t.Fatalf("ordinary setup did not create wallet login binding: %v", err)
	}
	if binding.UserID != s.UserID || binding.LoginDID != claimed.AdminDid || binding.SourceSessionID != s.ID {
		t.Fatalf("ordinary setup created the wrong wallet login binding: %+v", binding)
	}
}

func TestRejectDiscardsCandidate(t *testing.T) {
	db := testutil.Postgres(t)
	ctx := context.Background()
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	r := mustRequest(t, db, s)
	if _, err := Claim(ctx, db, *r.ClaimHandle, newDID(t)); err != nil {
		t.Fatal(err)
	}
	rejected, err := Reject(ctx, db, s.ID, r.ID)
	if err != nil || rejected.Status != "rejected" || rejected.AdminDid != "" {
		t.Fatalf("reject: %+v %v", rejected, err)
	}
	if _, err := Confirm(ctx, db, s.ID, r.ID); !errors.Is(err, ErrExpired) {
		t.Fatalf("rejected reservation confirmed: %v", err)
	}
}

func TestRedeemPollLeaseIsSharedAndReclaimable(t *testing.T) {
	db := testutil.Postgres(t)
	ctx := context.Background()
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	r := mustRequest(t, db, s)
	if _, err := Claim(ctx, db, *r.ClaimHandle, newDID(t)); err != nil {
		t.Fatal(err)
	}
	acquired, err := AcquireRedeemPoll(ctx, db, r.ID, "poll-1", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("first lease: acquired=%v err=%v", acquired, err)
	}
	acquired, err = AcquireRedeemPoll(ctx, db, r.ID, "poll-2", time.Minute)
	if err != nil || acquired {
		t.Fatalf("concurrent lease: acquired=%v err=%v", acquired, err)
	}
	if err := ReleaseRedeemPoll(ctx, db, r.ID, "poll-1"); err != nil {
		t.Fatal(err)
	}
	acquired, err = AcquireRedeemPoll(ctx, db, r.ID, "poll-2", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("replacement lease: acquired=%v err=%v", acquired, err)
	}
}

func TestManualProvisionConflictsWithClaim(t *testing.T) {
	db := testutil.Postgres(t)
	ctx := context.Background()
	s := testutil.SetupSession(t, db, model.ModeFullStack)
	r := mustRequest(t, db, s)
	if _, err := Claim(ctx, db, *r.ClaimHandle, newDID(t)); err != nil {
		t.Fatal(err)
	}
	if err := AcceptManual(ctx, db, s.ID, newDID(t), false); !errors.Is(err, ErrConflict) {
		t.Fatalf("manual provisioning bypassed claim: %v", err)
	}
}

func TestAdditionalAdministratorRelease(t *testing.T) {
	db := testutil.Postgres(t)
	ctx := context.Background()
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	if err := db.Model(&s).Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}
	r := mustRequest(t, db, s)
	if r.Operation != OperationGrantACL {
		t.Fatalf("operation=%s", r.Operation)
	}
	adminDID := newDID(t)
	if _, err := Claim(ctx, db, *r.ClaimHandle, adminDID); err != nil {
		t.Fatal(err)
	}
	if _, err := Confirm(ctx, db, s.ID, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := StartProvisioning(ctx, db, r.ID); err != nil {
		t.Fatal(err)
	}
	if err := MarkReleased(ctx, db, r.ID); err != nil {
		t.Fatal(err)
	}
	var released model.MobileConnection
	db.First(&released, "id = ?", r.ID)
	if released.Status != "released" || released.ReleasedAt == nil {
		t.Fatalf("not released: %+v", released)
	}
	var binding model.WalletLoginBinding
	if err := db.First(&binding, "source_claim_id = ?", r.ID).Error; err != nil {
		t.Fatalf("additional administrator did not create wallet login binding: %v", err)
	}
	if binding.UserID != s.UserID || binding.LoginDID != adminDID || binding.SourceSessionID != s.ID {
		t.Fatalf("additional administrator created the wrong wallet login binding: %+v", binding)
	}
}

func TestInitialReleaseRequiresClaimantKeyInstalled(t *testing.T) {
	db := testutil.Postgres(t)
	ctx := context.Background()
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	r := mustRequest(t, db, s)
	did := newDID(t)
	if _, err := Claim(ctx, db, *r.ClaimHandle, did); err != nil {
		t.Fatal(err)
	}
	if _, err := Confirm(ctx, db, s.ID, r.ID); err != nil {
		t.Fatal(err)
	}
	started, err := StartProvisioning(ctx, db, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&s).Updates(map[string]any{"status": "running", "admin_did": newDID(t)}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.First(&s, s.ID).Error; err != nil {
		t.Fatal(err)
	}
	if err := Reconcile(ctx, db, started, &s); err != nil {
		t.Fatal(err)
	}
	if started.Status != "provisioning" {
		t.Fatalf("released with another administrator installed: %+v", started)
	}
	if err := db.Model(&s).Update("admin_did", did).Error; err != nil {
		t.Fatal(err)
	}
	s.AdminDid = did
	if err := Reconcile(ctx, db, started, &s); err != nil || started.Status != "released" {
		t.Fatalf("did not release after claimant was installed: %+v %v", started, err)
	}
}

func TestTeardownRejectsClaim(t *testing.T) {
	db := testutil.Postgres(t)
	ctx := context.Background()
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	r := mustRequest(t, db, s)
	if err := Stop(ctx, db, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Claim(ctx, db, *r.ClaimHandle, newDID(t)); !errors.Is(err, ErrExpired) {
		t.Fatalf("claim after teardown: %v", err)
	}
}

func TestFailedGrantFreesSession(t *testing.T) {
	db := testutil.Postgres(t)
	ctx := context.Background()
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	if err := db.Model(&s).Update("status", "running").Error; err != nil {
		t.Fatal(err)
	}
	r := mustRequest(t, db, s)
	if _, err := Claim(ctx, db, *r.ClaimHandle, newDID(t)); err != nil {
		t.Fatal(err)
	}
	if _, err := Confirm(ctx, db, s.ID, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := StartProvisioning(ctx, db, r.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := Change(ctx, db, s.ID, "refresh", r.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("refresh during grant: %v", err)
	}
	if err := MarkFailed(ctx, db, r.ID, "grant failed"); err != nil {
		t.Fatal(err)
	}
	var failed model.MobileConnection
	db.First(&failed, "id = ?", r.ID)
	if failed.Status != "failed" || failed.ProvisionError != "grant failed" {
		t.Fatalf("not failed: %+v", failed)
	}
	if err := db.Model(&failed).Update("created_at", failed.CreatedAt.Add(-time.Minute)).Error; err != nil {
		t.Fatal(err)
	}
	next, err := Change(ctx, db, s.ID, "refresh", r.ID)
	if err != nil {
		t.Fatalf("refresh after failure: %v", err)
	}
	if next.ID == r.ID || next.Status != "open" || next.Operation != OperationGrantACL {
		t.Fatalf("no fresh reservation: %+v", next)
	}
}

func TestLegacyReservationsSettled(t *testing.T) {
	db := testutil.Postgres(t)
	ctx := context.Background()
	pending := testutil.SetupSession(t, db, model.ModeVtaOnly)
	accepted := testutil.SetupSession(t, db, model.ModeVtaOnly)
	connectedAt := time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
	for _, row := range []struct {
		id        string
		session   model.SetupSession
		status    string
		connected *time.Time
	}{
		{uuid.NewString(), pending, "pending", nil},
		{uuid.NewString(), accepted, "accepted", &connectedAt},
	} {
		if err := db.Model(&row.session).Update("status", "running").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec(`INSERT INTO mobile_connections (id, session_id, vta_did, status, created_at, expires_at, connected_at)
			VALUES (?, ?, ?, ?, now() - interval '2 days', now() - interval '1 day', ?)`, row.id, row.session.ID, row.session.VtaDid, row.status, row.connected).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := Change(ctx, db, row.session.ID, "create", ""); !errors.Is(err, ErrConflict) {
			t.Fatalf("%s row before migration: %v", row.status, err)
		}
	}

	up, err := os.ReadFile("../../migrations/000044_settle_legacy_mobile_connections.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(string(up)).Error; err != nil {
		t.Fatal(err)
	}

	for _, want := range []struct {
		session model.SetupSession
		status  string
	}{{pending, "lapsed"}, {accepted, "released"}} {
		legacy, err := Current(db, want.session.ID)
		if err != nil {
			t.Fatal(err)
		}
		if legacy.Status != want.status {
			t.Fatalf("legacy row = %s, want %s", legacy.Status, want.status)
		}
		if want.status == "released" && (legacy.ReleasedAt == nil || !legacy.ReleasedAt.Equal(connectedAt)) {
			t.Fatalf("released_at = %v, want %v", legacy.ReleasedAt, connectedAt)
		}
		if _, err := Change(ctx, db, want.session.ID, "create", ""); err != nil {
			t.Fatalf("%s row after migration: %v", want.status, err)
		}
	}
}
