package vault

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestDeleteFullStackSecretsDeletesEveryLeaf(t *testing.T) {
	tests := []struct {
		name       string
		delete     func(*Client) error
		listings   map[string][]string
		wantDelete []string
	}{
		{
			name:   "mediator nested keys",
			delete: func(c *Client) error { return c.DeleteMediatorSecrets(context.Background(), 7, 9) },
			listings: map[string][]string{
				"/v1/secret/metadata/mediator/user-7/session-9":        {"mediator_jwt_secret", "nested/"},
				"/v1/secret/metadata/mediator/user-7/session-9/nested": {"operating_secret"},
			},
			wantDelete: []string{
				"/v1/secret/metadata/mediator/user-7/session-9",
				"/v1/secret/metadata/mediator/user-7/session-9/mediator_jwt_secret",
				"/v1/secret/metadata/mediator/user-7/session-9/nested",
				"/v1/secret/metadata/mediator/user-7/session-9/nested/operating_secret",
			},
		},
		{
			name:   "dids key",
			delete: func(c *Client) error { return c.DeleteDidsSecrets(context.Background(), 7, 9) },
			listings: map[string][]string{
				"/v1/secret/metadata/dids/user-7/session-9": {"server-secrets"},
			},
			wantDelete: []string{
				"/v1/secret/metadata/dids/user-7/session-9",
				"/v1/secret/metadata/dids/user-7/session-9/server-secrets",
			},
		},
		{
			name:   "vtc key",
			delete: func(c *Client) error { return c.DeleteVtcSecrets(context.Background(), 7, 9) },
			listings: map[string][]string{
				"/v1/secret/metadata/vtc/user-7/session-9": {"key-bundle"},
			},
			wantDelete: []string{
				"/v1/secret/metadata/vtc/user-7/session-9",
				"/v1/secret/metadata/vtc/user-7/session-9/key-bundle",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var deleted []string
			client, closeServer := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == "LIST" {
					keys, ok := tt.listings[r.URL.Path]
					if !ok {
						http.Error(w, "unexpected list path", http.StatusNotFound)
						return
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": keys}})
					return
				}
				if r.Method == http.MethodDelete {
					deleted = append(deleted, r.URL.Path)
					w.WriteHeader(http.StatusNoContent)
					return
				}
				http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
			})
			defer closeServer()

			if err := tt.delete(client); err != nil {
				t.Fatalf("delete secrets: %v", err)
			}
			slices.Sort(deleted)
			slices.Sort(tt.wantDelete)
			if !slices.Equal(deleted, tt.wantDelete) {
				t.Fatalf("deleted paths = %v, want %v", deleted, tt.wantDelete)
			}
		})
	}
}

func TestDeleteSecretTreeContinuesAfterLeafFailure(t *testing.T) {
	var deleted []string
	client, closeServer := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "LIST":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"keys": []string{"first", "second"}}})
		case http.MethodDelete:
			deleted = append(deleted, r.URL.Path)
			if strings.HasSuffix(r.URL.Path, "/first") {
				http.Error(w, "delete failed", http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})
	defer closeServer()

	err := client.DeleteDidsSecrets(context.Background(), 7, 9)
	if err == nil || !strings.Contains(err.Error(), "delete vault secret") {
		t.Fatalf("error = %v, want leaf deletion failure", err)
	}
	if len(deleted) != 3 {
		t.Fatalf("delete attempts = %v, want both leaves and the prefix attempted", deleted)
	}
}

func TestDeleteSecretTreeFallsBackToExactKey(t *testing.T) {
	var deleted string
	client, closeServer := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case "LIST":
			http.Error(w, "no children", http.StatusNotFound)
		case http.MethodDelete:
			deleted = r.URL.Path
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "unexpected method", http.StatusMethodNotAllowed)
		}
	})
	defer closeServer()

	if err := client.DeleteVtcSecrets(context.Background(), 7, 9); err != nil {
		t.Fatalf("delete exact fallback: %v", err)
	}
	want := "/v1/secret/metadata/vtc/user-7/session-9"
	if deleted != want {
		t.Fatalf("deleted path = %q, want %q", deleted, want)
	}
}

func TestDeleteSeedDeletesExactKeyWithoutListing(t *testing.T) {
	var requestMethod, requestPath string
	client, closeServer := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		requestMethod, requestPath = r.Method, r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	})
	defer closeServer()

	if err := client.DeleteSeed(context.Background(), SeedPath(7, 9)); err != nil {
		t.Fatalf("delete seed: %v", err)
	}
	if requestMethod != http.MethodDelete || requestPath != "/v1/secret/metadata/vta/user-7/session-9/master-seed" {
		t.Fatalf("request = %s %s", requestMethod, requestPath)
	}
}

func testClient(t *testing.T, handleAuthenticated http.HandlerFunc) (*Client, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/auth/approle/login" {
			_ = json.NewEncoder(w).Encode(map[string]any{"auth": map[string]string{"client_token": "test-token"}})
			return
		}
		if r.Header.Get("X-Vault-Token") != "test-token" {
			http.Error(w, "missing token", http.StatusForbidden)
			return
		}
		handleAuthenticated(w, r)
	}))
	client, err := New(Config{Addr: server.URL, RoleID: "role", SecretID: "secret"})
	if err != nil {
		server.Close()
		t.Fatalf("new client: %v", err)
	}
	return client, server.Close
}
