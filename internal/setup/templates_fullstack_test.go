package setup

import (
	"strings"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
)

func TestRenderMediatorRecipeLeavesVaultTrustToEnvironment(t *testing.T) {
	got, err := RenderMediatorRecipeTOML(&model.SetupSession{}, MediatorVaultSecrets{
		HostPort: "vault.vault.svc:8200",
		KVMount:  "secret",
		Prefix:   "mediator/user-7/session-9",
		K8sRole:  "vta-user-7",
	})
	if err != nil {
		t.Fatalf("RenderMediatorRecipeTOML() error = %v", err)
	}
	want := "vault://vault.vault.svc:8200/secret/mediator/user-7/session-9?auth=kubernetes&role=vta-user-7"
	if !strings.Contains(got, want) {
		t.Fatalf("Vault backend URL is missing:\n%s", got)
	}
	if strings.Contains(got, "insecure=") || strings.Contains(got, "ca_cert=") {
		t.Fatalf("version-specific Vault parameter remains:\n%s", got)
	}
}
