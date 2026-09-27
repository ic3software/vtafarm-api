package handler

import (
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
)

func TestFarmConnectionUsesHostingStackButIndependentMediator(t *testing.T) {
	stack := runningProvider()
	stack.ID = 42
	stack.VtaName = "hosting-stack"
	target, err := farmConnectionTarget(stack, "did:webvh:another-mediator")
	if err != nil {
		t.Fatal(err)
	}
	if target.source != model.ConnectionInFarm || target.providerID == nil || *target.providerID != 42 {
		t.Fatalf("hosting source = %q, provider = %v", target.source, target.providerID)
	}
	if target.infra.MediatorDid != "did:webvh:another-mediator" ||
		target.infra.DaemonDid != stack.DIDHostingDid || target.infra.ServerURL != stack.DidsURL() {
		t.Fatalf("connection = %+v", target.infra)
	}

	stack.DomainType = model.DomainPlatform
	platform, err := farmConnectionTarget(stack, "did:webvh:another-mediator")
	if err != nil || platform.source != model.ConnectionPlatform || platform.providerID != nil {
		t.Fatalf("platform connection = %+v, %v", platform, err)
	}
}

func TestFarmConnectionRejectsUnreadyHosting(t *testing.T) {
	stack := runningProvider()
	stack.DIDHostingDid = ""
	if _, err := farmConnectionTarget(stack, "did:webvh:mediator"); err == nil {
		t.Fatal("hosting without a daemon DID was accepted")
	}
}
