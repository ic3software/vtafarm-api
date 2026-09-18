package handler

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/model"
)

func TestValidSIOPDID(t *testing.T) {
	tests := []struct {
		did  string
		want bool
	}{
		{"did:key:z6MkExample", true},
		{"did:webvh:QmExample:identity.example", true},
		{"did:peer:2.EzExample", false},
		{"did:webvh:QmExample:localhost#key-1", false},
		{" did:key:z6MkExample", false},
		{"https://identity.example", false},
		{strings.Repeat("a", 2049), false},
	}
	for _, test := range tests {
		if got := validSIOPDID(test.did); got != test.want {
			t.Errorf("validSIOPDID(%q) = %v, want %v", test.did, got, test.want)
		}
	}
}

func TestNonceMatchesDigest(t *testing.T) {
	digest := sha256.Sum256([]byte("challenge"))
	if !nonceMatchesDigest("challenge", digest[:]) {
		t.Fatal("matching nonce was rejected")
	}
	if nonceMatchesDigest("different", digest[:]) {
		t.Fatal("different nonce was accepted")
	}
	if nonceMatchesDigest("challenge", digest[:8]) {
		t.Fatal("short digest was accepted")
	}
}

func TestChallengeBelongsToRoleMatchedAccount(t *testing.T) {
	userID, adminID := uint(7), uint(9)
	if !challengeBelongsTo(model.SIOPChallenge{UserID: &userID}, userID, model.RoleUser) {
		t.Fatal("user challenge did not match its user")
	}
	if challengeBelongsTo(model.SIOPChallenge{UserID: &userID}, adminID, model.RoleAdmin) {
		t.Fatal("user challenge crossed into admin role")
	}
	if challengeBelongsTo(model.SIOPChallenge{AdminID: &adminID}, userID, model.RoleUser) {
		t.Fatal("admin challenge crossed into user role")
	}
}

func TestSIOPMetadataFailsClosed(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/auth/siop/metadata", nil)

	(&SIOPHandler{}).Metadata(context)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", recorder.Header().Get("Cache-Control"))
	}
	var body struct {
		Enabled bool   `json:"enabled"`
		RPDID   string `json:"rp_did"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Enabled || body.RPDID != "" {
		t.Fatalf("metadata = %+v, want disabled without RP configuration", body)
	}
}
