package setup

import (
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/pelletier/go-toml/v2"
)

func TestRenderVtcSetupTOMLEnablesSingleAdminMode(t *testing.T) {
	session := &model.SetupSession{
		Domain:            "firstperson.dev",
		MediatorSubdomain: "mediator-community",
		VtcSubdomain:      "vtc-community",
		VtcName:           "community",
		VtaDid:            "did:webvh:vta",
		MediatorDid:       "did:webvh:mediator",
	}

	rendered, err := RenderVtcSetupTOML(session, VtcVaultSecrets{})
	if err != nil {
		t.Fatalf("RenderVtcSetupTOML() error = %v", err)
	}

	var recipe struct {
		SingleAdminMode bool `toml:"single_admin_mode"`
	}
	if err := toml.Unmarshal([]byte(rendered), &recipe); err != nil {
		t.Fatalf("rendered VTC setup TOML is invalid: %v\n%s", err, rendered)
	}
	if !recipe.SingleAdminMode {
		t.Fatalf("single_admin_mode = false, want true\n%s", rendered)
	}
}

func TestVtcSetupPublishesOnlyProvisionedRegistryReferral(t *testing.T) {
	for _, did := range []string{"", "did:webvh:registry"} {
		s := &model.SetupSession{VtcName: "community", TrustRegistryEnabled: did != "", TrustRegistryDid: did}
		rendered, err := RenderVtcSetupTOML(s, VtcVaultSecrets{})
		if err != nil {
			t.Fatal(err)
		}
		var recipe map[string]any
		if err := toml.Unmarshal([]byte(rendered), &recipe); err != nil {
			t.Fatal(err)
		}
		value, exists := recipe["registry_did"]
		if did == "" && exists || did != "" && value != did {
			t.Fatalf("registry_did = %v, expected %q (omitted when empty)", value, did)
		}
	}
}
