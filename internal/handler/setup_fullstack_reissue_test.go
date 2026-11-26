package handler

import (
	"strings"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
)

func TestDidsEnrollmentSubject(t *testing.T) {
	const original = "did:key:z6MkOriginal"

	t.Run("keeps original DID before enrollment link is opened", func(t *testing.T) {
		session := &model.SetupSession{DIDHostingAdminDid: original}
		got, err := didsEnrollmentSubject(session)
		if err != nil {
			t.Fatalf("didsEnrollmentSubject() error = %v", err)
		}
		if got != original {
			t.Fatalf("didsEnrollmentSubject() = %q, want %q", got, original)
		}
	})

	t.Run("creates unique did:key after enrollment link is opened", func(t *testing.T) {
		session := &model.SetupSession{DIDHostingAdminDid: original, DidsEnrollUsed: true}
		first, err := didsEnrollmentSubject(session)
		if err != nil {
			t.Fatalf("didsEnrollmentSubject() error = %v", err)
		}
		second, err := didsEnrollmentSubject(session)
		if err != nil {
			t.Fatalf("didsEnrollmentSubject() error = %v", err)
		}
		const prefix = "did:key:z6Mk"
		if !strings.HasPrefix(first, prefix) {
			t.Fatalf("didsEnrollmentSubject() = %q, want prefix %q", first, prefix)
		}
		if first == second {
			t.Fatalf("generated duplicate DIDs: %q", first)
		}
	})
}
