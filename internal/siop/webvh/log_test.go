package webvh

import (
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

const rustFixtureDID = "did:webvh:Qmetio9KXzDkPXDpSQVyXSTcPVvj5ysHgMZt7y5ffRNDzD:fuzz.example.com"

func TestValidateLogAcceptsRustReferenceFixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/rust-chain-simple.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	document, err := validateLog(fixture, rustFixtureDID, time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), time.Minute)
	if err != nil {
		t.Fatalf("validateLog() error = %v", err)
	}
	if !strings.Contains(string(document), rustFixtureDID) {
		t.Fatalf("resolved document = %s", document)
	}
}

func TestValidateLogAcceptsRustPreRotationFixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/rust-chain-prerotation.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	did := "did:webvh:QmeAxih2DFAh5nep13zBgm7jsczMMfsaR2T8NQrPp6eSyi:fuzz.example.com"
	if _, err := validateLog(fixture, did, time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), time.Minute); err != nil {
		t.Fatalf("validateLog() error = %v", err)
	}
}

func TestValidateLogRejectsTamperingAndTruncationShapes(t *testing.T) {
	fixture, err := os.ReadFile("testdata/rust-chain-simple.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC)
	tests := map[string][]byte{
		"altered state":       []byte(strings.Replace(string(fixture), "fuzz.example.com", "evil.example.com", 1)),
		"altered proof":       []byte(strings.Replace(string(fixture), "zGzCik", "zAzCik", 1)),
		"broken sequence":     []byte(strings.Replace(string(fixture), `"versionId":"2-`, `"versionId":"7-`, 1)),
		"duplicate parameter": []byte(strings.Replace(string(fixture), `"parameters":{"method"`, `"parameters":{"method":"did:webvh:1.0","method"`, 1)),
	}
	for name, content := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := validateLog(content, rustFixtureDID, now, time.Minute); err == nil {
				t.Fatal("tampered DID log was accepted")
			}
		})
	}
}

func TestResolutionURL(t *testing.T) {
	tests := []struct {
		did  string
		want string
	}{
		{"did:webvh:Qmetio9KXzDkPXDpSQVyXSTcPVvj5ysHgMZt7y5ffRNDzD:example.com", "https://example.com/.well-known/did.jsonl"},
		{"did:webvh:Qmetio9KXzDkPXDpSQVyXSTcPVvj5ysHgMZt7y5ffRNDzD:example.com%3A8443:people:alice", "https://example.com:8443/people/alice/did.jsonl"},
	}
	for _, test := range tests {
		resolved, _, err := resolutionURL(test.did)
		if err != nil {
			t.Fatalf("resolutionURL(%q) error = %v", test.did, err)
		}
		if resolved.String() != test.want {
			t.Fatalf("resolutionURL(%q) = %q, want %q", test.did, resolved, test.want)
		}
	}
	for _, invalid := range []string{
		"did:webvh:Qmetio9KXzDkPXDpSQVyXSTcPVvj5ysHgMZt7y5ffRNDzD:localhost",
		"did:webvh:Qmetio9KXzDkPXDpSQVyXSTcPVvj5ysHgMZt7y5ffRNDzD:127.0.0.1",
		"did:webvh:Qmetio9KXzDkPXDpSQVyXSTcPVvj5ysHgMZt7y5ffRNDzD:%5B::1%5D",
		"did:webvh:Qmetio9KXzDkPXDpSQVyXSTcPVvj5ysHgMZt7y5ffRNDzD:example.com:%2e%2e",
		"did:webvh:Qmetio9KXzDkPXDpSQVyXSTcPVvj5ysHgMZt7y5ffRNDzD:example.com#fragment",
		"did:webvh:QmNotAFullHash:example.com",
	} {
		if _, _, err := resolutionURL(invalid); err == nil {
			t.Fatalf("resolutionURL(%q) accepted an unsafe DID", invalid)
		}
	}
}

func TestPublicAddressPolicy(t *testing.T) {
	for _, private := range []string{"127.0.0.1", "10.0.0.1", "169.254.1.1", "192.0.2.1", "::1", "fc00::1", "2001:db8::1"} {
		if isPublicAddress(parseIP(t, private)) {
			t.Fatalf("%s was considered public", private)
		}
	}
	if !isPublicAddress(parseIP(t, "8.8.8.8")) || !isPublicAddress(parseIP(t, "2606:4700:4700::1111")) {
		t.Fatal("public resolver addresses were rejected")
	}
}

func parseIP(t *testing.T, value string) []byte {
	t.Helper()
	parsed := net.ParseIP(value)
	if parsed == nil {
		t.Fatalf("invalid test IP %q", value)
	}
	return parsed
}
