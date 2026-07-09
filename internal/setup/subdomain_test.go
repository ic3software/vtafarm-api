package setup

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	valid := []string{"devtest1", "a", "a1", "my-vta", "vtc", "a-b-c", strings.Repeat("x", 48)}
	for _, name := range valid {
		if err := ValidateName(name); err != nil {
			t.Errorf("ValidateName(%q) = %v, want nil", name, err)
		}
	}

	invalid := []string{
		"",
		"-devtest1",
		"devtest1-",
		"Dev",
		"dev_test",
		"dev.test",
		"dev test",
		strings.Repeat("x", 49),
	}
	for _, name := range invalid {
		if err := ValidateName(name); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", name)
		}
	}
}

func TestDidPaths(t *testing.T) {
	if got := VtaDidPath("devtest1"); got != "devtest1-vta" {
		t.Errorf("VtaDidPath = %q, want devtest1-vta", got)
	}
	if got := MediatorDidPath("devtest1"); got != "devtest1-mediator" {
		t.Errorf("MediatorDidPath = %q, want devtest1-mediator", got)
	}
	if got := VtcDidPath("mycommunity"); got != "mycommunity-vtc" {
		t.Errorf("VtcDidPath = %q, want mycommunity-vtc", got)
	}
}

func TestHosts(t *testing.T) {
	if got := VtaHost("production", "devtest1"); got != "vta-devtest1" {
		t.Errorf("VtaHost(production) = %q, want vta-devtest1", got)
	}
	if got := VtaHost("development", "devtest1"); got != "vta-local-devtest1" {
		t.Errorf("VtaHost(development) = %q, want vta-local-devtest1", got)
	}

	vta, med, dids := FullStackHosts("production", "devtest1")
	if vta != "vta-devtest1" || med != "mediator-devtest1" || dids != "dids-devtest1" {
		t.Errorf("FullStackHosts = %q, %q, %q", vta, med, dids)
	}

	vta, med, dids, vtc := FullStackWithVtcHosts("production", "devtest1", "mycommunity")
	if vta != "vta-devtest1" || med != "mediator-devtest1" || dids != "dids-devtest1" || vtc != "vtc-mycommunity" {
		t.Errorf("FullStackWithVtcHosts = %q, %q, %q, %q", vta, med, dids, vtc)
	}

	// The longest prefix + a max-length name must still fit a DNS label.
	_, med, _, _ = FullStackWithVtcHosts("development", strings.Repeat("x", maxNameLength), "vtc")
	if len(med) > 63 {
		t.Errorf("mediator host %q exceeds the 63-char DNS label limit", med)
	}
}
