package handler

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/connection"
	"github.com/ic3software/vtafarm-api/internal/didkey"
	"github.com/ic3software/vtafarm-api/internal/middleware"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
)

func validVTAClaimConfig() config.VTAClaimConfig {
	return config.VTAClaimConfig{
		FarmDID:        "did:webvh:QmFarm:example.com",
		FarmSigningKey: base64.StdEncoding.EncodeToString(make([]byte, 32)),
		TrustTaskBase:  "https://api.example.com/trust-task",
		LinkHost:       "https://link.trustoverip.org",
	}
}

func TestVTAClaimConfigFailsClosed(t *testing.T) {
	tooLong := validVTAClaimConfig()
	tooLong.FarmDID = "did:webvh:" + strings.Repeat("a", 220)
	for _, tc := range []config.VTAClaimConfig{
		{},
		{FarmDID: "did:webvh:example", LinkHost: "http://example.com"},
		{FarmDID: "did:webvh:example", LinkHost: "https://user@example.com"},
		{FarmDID: "did:webvh:example", FarmSigningKey: validVTAClaimConfig().FarmSigningKey, TrustTaskBase: "https://api.example.com/wrong", LinkHost: "https://link.example.com"},
		tooLong,
	} {
		h := NewMobileConnectionHandler(nil, nil, nil, tc, true)
		if h.enabled || h.linkHost != nil {
			t.Errorf("accepted invalid config: %+v", tc)
		}
	}
}

func TestTriggerLinkShape(t *testing.T) {
	h := NewMobileConnectionHandler(nil, nil, nil, validVTAClaimConfig(), true)
	handle := "abcdefghijklmnopqrstuv"
	r := &model.MobileConnection{ClaimHandle: &handle, ExpiresAt: time.Unix(1_800_000_000, 0)}
	link := h.triggerLink(r)
	const expected = "https://link.trustoverip.org/t#_from=did%3Awebvh%3AQmFarm%3Aexample.com&_id=abcdefghijklmnopqrstuv&_exp=1800000000&_type=%2Fvti%2Fflow%2Fvta-claim%2F0.1"
	if link != expected {
		t.Fatalf("trigger link = %s", link)
	}
	u, err := url.Parse(link)
	if err != nil {
		t.Fatal(err)
	}
	values, err := url.ParseQuery(u.Fragment)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "https" || u.Host != "link.trustoverip.org" || u.Path != "/t" ||
		values.Get("_from") != h.farmDID || values.Get("_id") != handle || values.Get("_type") != vtaClaimFlowType {
		t.Fatalf("unexpected trigger link: %s", link)
	}
}

func TestMobileOwnerClaimFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.Postgres(t)
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	other := testutil.SetupSession(t, db, model.ModeVtaOnly)
	h := NewMobileConnectionHandler(db, nil, nil, validVTAClaimConfig(), true)
	h.enabled = true
	h.kickInitial = func(uint) {}
	router := gin.New()
	router.Use(middleware.NoStore())
	owner := router.Group("/owner", func(c *gin.Context) { c.Set(middleware.ContextUserID, s.UserID) })
	owner.GET("/:id/current", h.Current)
	owner.POST("/:id", h.Change)
	owner.POST("/:id/:request_id/refresh", h.Change)
	owner.DELETE("/:id/:request_id", h.Change)
	owner.POST("/:id/:request_id/confirm", h.Confirm)
	owner.POST("/:id/:request_id/reject", h.Reject)
	call := func(method, path string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		return w
	}

	if w := call(http.MethodPost, "/owner/"+other.VtaName); w.Code != http.StatusNotFound {
		t.Fatalf("cross-owner creation=%d", w.Code)
	}
	w := call(http.MethodPost, "/owner/"+s.VtaName)
	if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("create=%d %s", w.Code, w.Body)
	}
	var created struct {
		Connection mobileView `json:"connection"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Connection.Status != "open" || created.Connection.TriggerLink == "" || strings.Contains(w.Body.String(), "callback_url") {
		t.Fatalf("unexpected create response: %s", w.Body)
	}

	var reservation model.MobileConnection
	if err := db.First(&reservation, "id = ?", created.Connection.RequestID).Error; err != nil {
		t.Fatal(err)
	}
	did, err := didkey.Generate()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Claim(t.Context(), db, *reservation.ClaimHandle, did); err != nil {
		t.Fatal(err)
	}
	w = call(http.MethodGet, "/owner/"+s.VtaName+"/current")
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"status":"claimed"`)) ||
		!bytes.Contains(w.Body.Bytes(), []byte(`"claim_check":"`)) || !bytes.Contains(w.Body.Bytes(), []byte(did)) {
		t.Fatalf("claimed view=%d %s", w.Code, w.Body)
	}

	w = call(http.MethodPost, "/owner/"+s.VtaName+"/"+reservation.ID+"/confirm")
	if w.Code != http.StatusOK || bytes.Contains(w.Body.Bytes(), []byte("claim_check")) || !bytes.Contains(w.Body.Bytes(), []byte(`"status":"provisioning"`)) {
		t.Fatalf("confirm=%d %s", w.Code, w.Body)
	}
	var saved model.SetupSession
	db.First(&saved, s.ID)
	if saved.AdminDid != did {
		t.Fatalf("confirmed DID not passed to worker: %+v", saved)
	}
}

func TestMobileRejectFlow(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.Postgres(t)
	s := testutil.SetupSession(t, db, model.ModeVtaOnly)
	h := NewMobileConnectionHandler(db, nil, nil, validVTAClaimConfig(), true)
	h.enabled = true
	router := gin.New()
	owner := router.Group("/owner", func(c *gin.Context) { c.Set(middleware.ContextUserID, s.UserID) })
	owner.POST("/:id/:request_id/reject", h.Reject)
	r, err := connection.Change(t.Context(), db, s.ID, "create", "")
	if err != nil {
		t.Fatal(err)
	}
	did, _ := didkey.Generate()
	if _, err := connection.Claim(t.Context(), db, *r.ClaimHandle, did); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/owner/"+s.VtaName+"/"+r.ID+"/reject", nil))
	if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(`"status":"rejected"`)) || bytes.Contains(w.Body.Bytes(), []byte(did)) {
		t.Fatalf("reject=%d %s", w.Code, w.Body)
	}
}
