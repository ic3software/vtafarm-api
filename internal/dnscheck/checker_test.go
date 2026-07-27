package dnscheck

import "testing"

func TestAllCloudflare(t *testing.T) {
	// A fully proxied record resolves to Cloudflare's anycast edge and never
	// reaches us — worth naming precisely, because "resolves to 104.21.x" tells
	// the user nothing about what to change.
	if !allCloudflare([]string{"104.21.94.190", "172.67.139.142"}) {
		t.Error("allCloudflare = false for two Cloudflare edge addresses")
	}
	if !allCloudflare([]string{"162.159.0.1"}) {
		t.Error("allCloudflare = false for 162.159.0.1")
	}

	// All of them, not any: one Cloudflare address alongside ours is a
	// different misconfiguration and deserves the generic mismatch message.
	if allCloudflare([]string{"104.21.94.190", "157.180.68.139"}) {
		t.Error("allCloudflare = true for a mixed answer")
	}
	if allCloudflare([]string{"157.180.68.139"}) {
		t.Error("allCloudflare = true for the cluster ingress")
	}
	if allCloudflare(nil) {
		t.Error("allCloudflare = true for no addresses")
	}
	if allCloudflare([]string{"not-an-ip"}) {
		t.Error("allCloudflare = true for a non-address")
	}
}

func TestCheckTXTMatchesAnyValue(t *testing.T) {
	// No resolvers, so nothing is found — the point here is the shape of the
	// failure, which is what the portal renders under the record row.
	c := NewWithResolvers()
	got := c.CheckTXT(t.Context(), "_vtafarm-challenge.aaa.com", "aaa.com", "vtafarm-verify=abc")
	if got.OK {
		t.Error("CheckTXT OK with no resolvers")
	}
	if got.Name != "_vtafarm-challenge.aaa.com" || got.Expected != "vtafarm-verify=abc" {
		t.Errorf("CheckTXT echoed %q / %q", got.Name, got.Expected)
	}
	if got.Detail == "" {
		t.Error("CheckTXT gave no detail — the UI has nothing to show the user")
	}
}

func TestCheckHostsWithoutResolvers(t *testing.T) {
	c := NewWithResolvers()
	results := c.CheckHosts(t.Context(), []string{"vta.aaa.com", "vtc.aaa.com"}, "157.180.68.139")
	if len(results) != 2 {
		t.Fatalf("CheckHosts returned %d results, want 2", len(results))
	}
	// Order must match the input: the portal pairs results with the component
	// rows positionally.
	if results[0].FQDN != "vta.aaa.com" || results[1].FQDN != "vtc.aaa.com" {
		t.Errorf("CheckHosts reordered results: %q, %q", results[0].FQDN, results[1].FQDN)
	}
	for _, r := range results {
		if r.OK {
			t.Errorf("%s reported OK with no resolvers configured", r.FQDN)
		}
	}
}
