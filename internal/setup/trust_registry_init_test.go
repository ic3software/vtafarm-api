package setup

import (
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/vault"
	"testing"
)

func TestRegistryAvailableWithoutHelperImageConfiguration(t *testing.T) {
	o := &Orchestrator{k8s: &k8s.Client{}, vault: &vault.Client{}}
	if !o.RegistryAvailable() {
		t.Fatal("Registry must not require a separately configured helper image")
	}
	o.vault = nil
	if o.RegistryAvailable() {
		t.Fatal("Registry must still require Vault")
	}
}
