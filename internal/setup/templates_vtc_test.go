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
