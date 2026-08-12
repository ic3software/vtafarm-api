package setup

import (
	"strings"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
)

// Four recipes advertise TSP in four different vocabularies and only work if
// all four agree. Two of them default to something else when the key goes
// missing (`identity.transport` reads absent as `both`, `deployment.protocols`
// as `["didcomm"]`), so a dropped line renders fine and ships a stack that
// advertises a transport nobody serves — or serves one nobody was told about.

func tspTestSession() *model.SetupSession {
	return &model.SetupSession{
		VtaName:           "alice",
		VtcName:           "alice",
		Subdomain:         "vta-alice",
		MediatorSubdomain: "mediator-alice",
		DidsSubdomain:     "dids-alice",
		VtcSubdomain:      "vtc-alice",
		Domain:            "firstperson.dev",
		MediatorDid:       "did:webvh:scid:dids-alice.firstperson.dev:alice-mediator",
		VtaDid:            "did:webvh:scid:dids-alice.firstperson.dev:alice-vta",
		VtaDidUrl:         "https://dids.firstperson.dev/alice-vta",
		Portable:          true,
		PreRotationCount:  1,
	}
}

func TestVtaOnlySetupTOMLEnablesTSP(t *testing.T) {
	out, err := RenderVtaSetupTOML(tspTestSession(), VaultSecrets{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// TSP requires DIDComm — setup refuses the pair split apart.
	for _, want := range []string{"didcomm = true", "tsp     = true"} {
		if !strings.Contains(out, want) {
			t.Errorf("vta_only setup TOML missing %q:\n%s", want, out)
		}
	}
}

func TestFullStackVtaSetupTOMLEnablesTSP(t *testing.T) {
	out, err := RenderFullStackVtaSetupTOML(tspTestSession(), VaultSecrets{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	for _, want := range []string{"didcomm = true", "tsp     = true"} {
		if !strings.Contains(out, want) {
			t.Errorf("full_stack VTA setup TOML missing %q:\n%s", want, out)
		}
	}
	// Deliberately absent — derived from `[services]`, not a second source of truth.
	if strings.Contains(out, "protocols") {
		t.Errorf("full_stack VTA setup TOML names messaging.protocols; it must stay derived:\n%s", out)
	}
}

func TestMediatorRecipeCarriesTSP(t *testing.T) {
	out, err := RenderMediatorRecipeTOML(tspTestSession(), MediatorVaultSecrets{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, `protocols = ["didcomm", "tsp"]`) {
		t.Errorf("mediator recipe does not carry TSP:\n%s", out)
	}
}

func TestWebvhRecipeStatesTransportExplicitly(t *testing.T) {
	// Both phases render the same [identity] block; only [vta] is phase-dependent.
	for _, phase := range []string{WebvhPhasePrepare, WebvhPhaseComplete} {
		out, err := RenderWebvhRecipeTOML(tspTestSession(), phase, "sha256:deadbeef", WebvhVaultSecrets{})
		if err != nil {
			t.Fatalf("render %s: %v", phase, err)
		}
		if !strings.Contains(out, `transport    = "both"`) {
			t.Errorf("webvh recipe (%s) leaves identity.transport implicit:\n%s", phase, out)
		}
	}
}

func TestVtcSetupTOMLAdvertisesBothTransports(t *testing.T) {
	out, err := RenderVtcSetupTOML(tspTestSession(), VtcVaultSecrets{})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	// Required by the schema, and TSP leads — array order encodes preference.
	if !strings.Contains(out, `transports   = ["tsp", "didcomm"]`) {
		t.Errorf("VTC setup TOML missing messaging.transports:\n%s", out)
	}
}
