package setup

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
)

func TestLoadPoolActivationIsStableAcrossEntryPoints(t *testing.T) {
	for _, test := range []struct {
		name       string
		public     bool
		entryPoint string
	}{
		{name: "admin station", entryPoint: model.PoolPurposeAdminStation},
		{name: "public signup", public: true, entryPoint: model.PoolPurposePublicSignup},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := testutil.Postgres(t)
			session := testutil.SetupSession(t, db, model.ModeVtaOnly)
			now := time.Now()
			claim := model.MobileConnection{
				ID: uuid.NewString(), SessionID: session.ID, VtaDid: session.VtaDid,
				Operation: connection.OperationProvisionVTA, AdminDid: "did:key:z6MkAdmin",
				Status: "provisioning", CreatedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute), ConfirmedAt: &now,
			}
			if test.public {
				claim.PublicAccessDigest = make([]byte, 32)
			}
			if err := db.Create(&claim).Error; err != nil {
				t.Fatal(err)
			}
			o := &Orchestrator{db: db}
			first, err := o.loadPoolActivation(context.Background(), session.ID, claim.AdminDid)
			if err != nil {
				t.Fatal(err)
			}
			second, err := o.loadPoolActivation(context.Background(), session.ID, claim.AdminDid)
			if err != nil {
				t.Fatal(err)
			}
			if first.id != second.id || len(first.id) != 32 {
				t.Fatalf("activation ID is not stable: %q / %q", first.id, second.id)
			}
			if first.requestID != claim.ID || first.entryPoint != test.entryPoint || !first.confirmed.Equal(now) {
				t.Fatalf("activation context = %+v", first)
			}
		})
	}
}

func TestPoolActivationGateURLUsesTrustTaskOrigin(t *testing.T) {
	o := &Orchestrator{poolActivationGateBase: poolActivationGateBase("https://api.example.com/trust-task")}
	activationID := PoolActivationID(42, "claim-id")
	want := "https://api.example.com/api/v1/internal/pool-activations/42/" + activationID
	if got := o.poolActivationGateURL(42, activationID); got != want {
		t.Fatalf("poolActivationGateURL() = %q, want %q", got, want)
	}
	if got := poolActivationGateBase("http://api.example.com/trust-task"); got != "" {
		t.Fatalf("insecure gate base = %q, want disabled", got)
	}
}
