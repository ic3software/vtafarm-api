package setup

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/pelletier/go-toml/v2"
)

func TestRegistryConfigPreservesCustomizedSettings(t *testing.T) {
	for _, content := range []string{
		"# custom site\npublic_url = \"https://community.example\"\n[rooms]\nmax_files = 9\n",
		"public_url = \"https://community.example\"\n[registry] # keep this\nhttp_timeout_seconds = 17\n[rooms]\nmax_files = 9\n",
		"[registry]\ndid = \"\"\nhealth_probe_interval_seconds = 42\n",
	} {
		const did = "did:webvh:abc:host.example:registry"
		merged, err := MergeRegistryConfig(content, did)
		if err != nil {
			t.Fatal(err)
		}
		var before, after map[string]any
		if err = toml.Unmarshal([]byte(content), &before); err != nil {
			t.Fatal(err)
		}
		if err = toml.Unmarshal([]byte(merged), &after); err != nil {
			t.Fatal(err)
		}
		registry := after["registry"].(map[string]any)
		if registry["did"] != did {
			t.Fatal("registry DID missing")
		}
		delete(registry, "did")
		if old, ok := before["registry"].(map[string]any); ok {
			delete(old, "did")
		} else {
			delete(after, "registry")
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatal("an unrelated configuration value changed")
		}
		again, err := MergeRegistryConfig(merged, did)
		if err != nil || again != merged {
			t.Fatal("merge is not idempotent")
		}
		if strings.Contains(content, "# keep this") && !strings.Contains(merged, "# keep this") {
			t.Fatal("comment was discarded")
		}
	}
}

func TestRegistryConfigRefusesConflictsAndMalformedInputs(t *testing.T) {
	for _, content := range []string{"[registry]\ndid=\"did:webvh:other\"\n", "[registry]\nurl=\"https://other.example\"\n", "registry = { health_probe_interval_seconds = 42 }\n", "[invalid", "welcome = '''\n[registry]\ndid = \"\"\n'''\n"} {
		if _, err := MergeRegistryConfig(content, "did:webvh:ours"); err == nil {
			t.Fatal("accepted conflicting or malformed config")
		}
	}
}

func TestRegistryReferralRefusesMultipleOrArrayTypedRegistryServices(t *testing.T) {
	for _, services := range []string{
		`[{"type":["TrustRegistry"],"serviceEndpoint":{"uri":"did:webvh:other"}}]`,
		`[{"type":"TrustRegistry","serviceEndpoint":{"uri":"did:webvh:registry"}},{"type":"TrustRegistry","serviceEndpoint":{"uri":"did:webvh:other"}}]`,
	} {
		if _, _, err := RegistryReferral([]byte(`{"id":"did:webvh:community","service":`+services+`}`), "did:webvh:community", "did:webvh:registry"); err == nil {
			t.Fatal("accepted conflicting registry referrals")
		}
	}
}

func TestRegistryReferralPreservesIdentityKeysAndServices(t *testing.T) {
	const did = "did:webvh:community"
	document := []byte(`{"id":"did:webvh:community","verificationMethod":[{"id":"did:webvh:community#key-0","publicKeyMultibase":"zPublic"}],"authentication":["did:webvh:community#key-0"],"service":[{"id":"did:webvh:community#didcomm","type":"DIDCommMessaging","serviceEndpoint":{"uri":"did:webvh:mediator"}}],"custom":{"name":"existing"}}`)
	updated, changed, err := RegistryReferral(document, did, "did:webvh:registry")
	if err != nil || !changed {
		t.Fatalf("referral: %v", err)
	}
	var before, after map[string]any
	_ = json.Unmarshal(document, &before)
	_ = json.Unmarshal(updated, &after)
	services := after["service"].([]any)
	if len(services) != 2 {
		t.Fatal("original services were replaced")
	}
	after["service"] = services[:1]
	if !reflect.DeepEqual(before, after) {
		t.Fatal("existing DID document fields changed")
	}
	_, changed, err = RegistryReferral(updated, did, "did:webvh:registry")
	if err != nil || changed {
		t.Fatal("existing referral was not idempotent")
	}
	if _, _, err = RegistryReferral(updated, did, "did:webvh:other"); err == nil {
		t.Fatal("silently replaced another registry")
	}
	if _, _, err = RegistryReferral(document, "did:webvh:wrong", "did:webvh:registry"); err == nil {
		t.Fatal("accepted another identity")
	}
}
