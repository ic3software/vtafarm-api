package passkey

import (
	"sync"
	"time"

	"github.com/go-webauthn/webauthn/webauthn"
)

const sessionTTL = 5 * time.Minute

type entry struct {
	data      *webauthn.SessionData
	expiresAt time.Time
}

// SessionStore is a thread-safe, TTL-based in-memory store for WebAuthn session data.
// Each entry is consumed (deleted) on first Get to prevent replay.
type SessionStore struct {
	mu      sync.Mutex
	entries map[string]entry
}

func NewSessionStore() *SessionStore {
	s := &SessionStore{entries: make(map[string]entry)}
	go s.cleanup()
	return s
}

func (s *SessionStore) Set(key string, data *webauthn.SessionData) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.entries[key] = entry{data: data, expiresAt: time.Now().Add(sessionTTL)}
}

// Get retrieves and deletes the session (one-time use). Returns false if missing or expired.
func (s *SessionStore) Get(key string) (*webauthn.SessionData, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.entries[key]
	delete(s.entries, key)
	if !ok || time.Now().After(e.expiresAt) {
		return nil, false
	}
	return e.data, true
}

func (s *SessionStore) cleanup() {
	ticker := time.NewTicker(time.Minute)
	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		for k, v := range s.entries {
			if now.After(v.expiresAt) {
				delete(s.entries, k)
			}
		}
		s.mu.Unlock()
	}
}
