package vault

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/didresolver"
)

func TestRegistryCredentialIsScopedStableAndCASProtected(t *testing.T) {
	stored := ""
	writes := 0
	client, closeServer := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/secret/data/trust-registry/user-7/session-9/credential" {
			t.Error("wrong secret scope")
			http.Error(w, "not found", 404)
			return
		}
		if r.Method == http.MethodGet {
			if stored == "" {
				http.Error(w, "missing", 404)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"data": map[string]string{"credential": stored}}})
			return
		}
		var request struct {
			Options struct {
				CAS int `json:"cas"`
			} `json:"options"`
			Data map[string]string `json:"data"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		if request.Options.CAS != 0 {
			t.Error("credential creation must use CAS=0")
		}
		stored = request.Data["credential"]
		writes++
		w.WriteHeader(http.StatusNoContent)
	})
	defer closeServer()
	first, err := client.RegistryCredential(context.Background(), 7, 9, "did:webvh:vta", "https://vta.example")
	if err != nil {
		t.Fatal(err)
	}
	second, err := client.RegistryCredential(context.Background(), 7, 9, "did:webvh:vta", "https://vta.example")
	if err != nil {
		t.Fatal(err)
	}
	if first != second || writes != 1 {
		t.Fatal("retry rotated the registry access credential")
	}
	key, err := didresolver.DecodeBase58BTC(strings.TrimPrefix(first.PrivateKeyMultibase, "z"))
	if err != nil {
		t.Fatal(err)
	}
	if len(key) != 34 || key[0] != 0x80 || key[1] != 0x26 {
		t.Fatal("invalid Ed25519 secret multicodec")
	}
	derived, _ := didkey.FromPublicKey(ed25519.NewKeyFromSeed(key[2:]).Public().(ed25519.PublicKey))
	if derived != first.Did {
		t.Fatal("credential DID does not match the private key")
	}
	if _, err = client.RegistryCredential(context.Background(), 7, 9, "did:webvh:other", "https://other.example"); err == nil {
		t.Fatal("accepted credential for another VTA")
	}
}
