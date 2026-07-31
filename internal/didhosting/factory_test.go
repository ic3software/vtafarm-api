package didhosting

import (
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// testKeypair returns a did:key and the base64 seed New expects. The DID does
// not have to be a real multibase encoding — nothing in the paths under test
// resolves it.
func testKeypair() (did, privKeyB64 string) {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i)
	}
	return "did:key:z6MkTestClient", base64.StdEncoding.EncodeToString(seed)
}

// serverInfoStub serves /api/server-info with the given DID and counts hits, so
// a test can tell a cache hit from a re-fetch.
func serverInfoStub(t *testing.T, serverDid string, hits *int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/server-info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		atomic.AddInt32(hits, 1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"server_did":"` + serverDid + `"}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestForAcceptsMatchingAudience(t *testing.T) {
	var hits int32
	srv := serverInfoStub(t, "did:webvh:dids.example", &hits)
	did, key := testKeypair()

	c, err := NewFactory(did, key).For(srv.URL, "did:webvh:dids.example")
	if err != nil {
		t.Fatalf("For: %v", err)
	}
	if c.ServerDid() != "did:webvh:dids.example" {
		t.Fatalf("ServerDid = %q", c.ServerDid())
	}
}

// The check is the whole point of the expected-audience parameter: a daemon
// claiming somebody else's DID would otherwise receive an id_token minted for
// that somebody else, signed with the farm's admin key.
func TestForRefusesMismatchedAudience(t *testing.T) {
	var hits int32
	srv := serverInfoStub(t, "did:webvh:attacker.example", &hits)
	did, key := testKeypair()

	_, err := NewFactory(did, key).For(srv.URL, "did:webvh:dids.example")
	if err == nil {
		t.Fatal("expected a mismatch error, got nil")
	}
	for _, want := range []string{"did:webvh:attacker.example", "did:webvh:dids.example"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
}

// Empty means "no expectation on record" — the state of every vta_only session
// until did_hosting_did is populated. It must not start failing them.
func TestForWithoutExpectationAcceptsAnything(t *testing.T) {
	var hits int32
	srv := serverInfoStub(t, "did:webvh:whatever.example", &hits)
	did, key := testKeypair()

	if _, err := NewFactory(did, key).For(srv.URL, ""); err != nil {
		t.Fatalf("For with no expectation: %v", err)
	}
}

// A cached client must still be checked. Otherwise one unverified call would
// permanently disarm the check for that URL.
func TestForChecksCachedClients(t *testing.T) {
	var hits int32
	srv := serverInfoStub(t, "did:webvh:dids.example", &hits)
	did, key := testKeypair()
	f := NewFactory(did, key)

	if _, err := f.For(srv.URL, ""); err != nil {
		t.Fatalf("priming call: %v", err)
	}
	if _, err := f.For(srv.URL, "did:webvh:someone-else.example"); err == nil {
		t.Fatal("expected the cached client to be checked, got nil")
	}
	if _, err := f.For(srv.URL, "did:webvh:dids.example"); err != nil {
		t.Fatalf("matching expectation after a mismatch: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("server-info fetched %d times, want 1 — a mismatch must not evict the cache", got)
	}
}

func TestNilFactoryFails(t *testing.T) {
	var f *Factory
	if _, err := f.For("https://dids.example", ""); err == nil {
		t.Fatal("expected an error from a nil factory")
	}
}

func TestForRejectsEmptyURL(t *testing.T) {
	did, key := testKeypair()
	if _, err := NewFactory(did, key).For("", ""); err == nil {
		t.Fatal("expected an error for an empty control URL")
	}
}
