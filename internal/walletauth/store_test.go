package walletauth

import (
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
)

func TestActiveBindingsMaterializesReleasedClaim(t *testing.T) {
	for _, operation := range []string{connection.OperationProvisionVTA, connection.OperationGrantACL} {
		t.Run(operation, func(t *testing.T) {
			db := testutil.Postgres(t)
			session := testutil.SetupSession(t, db, model.ModeVtaOnly)
			loginDID, err := didkey.Generate()
			if err != nil {
				t.Fatal(err)
			}
			digest := connection.ClaimantDigest(loginDID)
			releasedAt := time.Now().UTC()
			claim := model.MobileConnection{
				ID:             uuid.NewString(),
				SessionID:      session.ID,
				VtaDid:         session.VtaDid,
				Operation:      operation,
				AdminDid:       loginDID,
				Status:         "released",
				ClaimantDigest: &digest,
				CreatedAt:      releasedAt.Add(-time.Minute),
				ExpiresAt:      releasedAt.Add(-30 * time.Second),
				ReleasedAt:     &releasedAt,
			}
			if err := db.Create(&claim).Error; err != nil {
				t.Fatal(err)
			}

			bindings, err := ActiveBindings(t.Context(), db, loginDID)
			if err != nil {
				t.Fatal(err)
			}
			if len(bindings) != 1 {
				t.Fatalf("bindings=%d, want 1", len(bindings))
			}
			binding := bindings[0]
			if binding.UserID != session.UserID || binding.SourceSessionID != session.ID || binding.SourceClaimID != claim.ID {
				t.Fatalf("wrong binding: %+v", binding)
			}

			bindings, err = ActiveBindings(t.Context(), db, loginDID)
			if err != nil || len(bindings) != 1 {
				t.Fatalf("idempotent bindings=%d err=%v", len(bindings), err)
			}
		})
	}
}
