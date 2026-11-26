package setup

import (
	"context"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
)

type staticReferralDIDResolver struct {
	document []byte
	calls    int
}

func (r *staticReferralDIDResolver) ResolveDocument(context.Context, string) ([]byte, error) {
	r.calls++
	return r.document, nil
}

func TestPublishRegistryReferralUsesSuppliedResolver(t *testing.T) {
	const vtcDID = "did:webvh:community"
	const registryDID = "did:webvh:registry"
	document, changed, err := RegistryReferral([]byte(`{"id":"did:webvh:community"}`), vtcDID, registryDID)
	if err != nil || !changed {
		t.Fatalf("prepare referral: changed=%t err=%v", changed, err)
	}
	resolver := &staticReferralDIDResolver{document: document}
	session := &model.SetupSession{VtcDid: vtcDID, TrustRegistryDid: registryDID}

	if err := (&Orchestrator{}).publishRegistryReferral(t.Context(), "unused", session, resolver); err != nil {
		t.Fatal(err)
	}
	if resolver.calls != 1 {
		t.Fatalf("resolver calls = %d, want 1", resolver.calls)
	}
}
