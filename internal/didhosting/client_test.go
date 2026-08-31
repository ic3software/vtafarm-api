package didhosting

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
)

func newClientTestServer(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	seed := make([]byte, 32)
	client, err := New(server.URL, "did:key:z6MkLoadTest", base64.StdEncoding.EncodeToString(seed))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return client
}

func writeJSON(t *testing.T, w http.ResponseWriter, value any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		t.Errorf("encode response: %v", err)
	}
}

func TestConcurrentRegisterDidReusesOneAuthentication(t *testing.T) {
	var authCount atomic.Int32
	var registerCount atomic.Int32
	client := newClientTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/server-info":
			writeJSON(t, w, map[string]any{"server_did": "did:key:z6MkServer"})
		case "/api/auth/challenge":
			writeJSON(t, w, map[string]any{"challenge": "nonce", "sessionId": "challenge-session"})
		case "/api/auth/":
			authCount.Add(1)
			writeJSON(t, w, map[string]any{"tokens": map[string]any{
				"accessToken": "shared-token", "expiresIn": 300,
			}})
		case "/api/dids/register":
			if got := r.Header.Get("Authorization"); got != "Bearer shared-token" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			registerCount.Add(1)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	})

	const requests = 10
	start := make(chan struct{})
	errs := make(chan error, requests)
	var wg sync.WaitGroup
	for i := 0; i < requests; i++ {
		wg.Add(1)
		go func(sequence int) {
			defer wg.Done()
			<-start
			errs <- client.RegisterDid(context.Background(), fmt.Sprintf("load-%d", sequence), "did log")
		}(i)
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("RegisterDid: %v", err)
		}
	}
	if got := authCount.Load(); got != 1 {
		t.Fatalf("authenticate calls = %d, want 1", got)
	}
	if got := registerCount.Load(); got != requests {
		t.Fatalf("register calls = %d, want %d", got, requests)
	}
}

func TestRegisterDidRetriesOnceAfterUnauthorized(t *testing.T) {
	var authCount atomic.Int32
	var registerCount atomic.Int32
	client := newClientTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/server-info":
			writeJSON(t, w, map[string]any{"server_did": "did:key:z6MkServer"})
		case "/api/auth/challenge":
			writeJSON(t, w, map[string]any{"challenge": "nonce", "sessionId": "challenge-session"})
		case "/api/auth/":
			n := authCount.Add(1)
			writeJSON(t, w, map[string]any{"tokens": map[string]any{
				"accessToken": fmt.Sprintf("token-%d", n), "expiresIn": 300,
			}})
		case "/api/dids/register":
			registerCount.Add(1)
			if r.Header.Get("Authorization") == "Bearer token-1" {
				http.Error(w, "unauthorized", http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	})

	if err := client.RegisterDid(context.Background(), "load-1", "did log"); err != nil {
		t.Fatalf("RegisterDid: %v", err)
	}
	if got := authCount.Load(); got != 2 {
		t.Fatalf("authenticate calls = %d, want 2", got)
	}
	if got := registerCount.Load(); got != 2 {
		t.Fatalf("register calls = %d, want 2", got)
	}
}
