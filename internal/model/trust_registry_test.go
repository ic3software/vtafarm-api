package model

import "testing"

func TestRegistryUpgradeInventoryRequiresActiveComponent(t *testing.T) {
	s := SetupSession{Mode: ModeFullStack, TrustRegistryImage: "ghcr.io/ic3software/trust-registry:0.28.0-test"}
	for _, state := range []string{"", "disabled", "enabling", "failed"} {
		s.TrustRegistryState = state
		if s.ComponentImage("trust-registry") != "" {
			t.Fatal("undeployed registry was offered as an upgrade target")
		}
	}
	s.TrustRegistryState = "active"
	if s.ComponentImage("trust-registry") != s.TrustRegistryImage {
		t.Fatal("active registry missing from upgrade inventory")
	}
}
