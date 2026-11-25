package handler

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ic3software/vtafarm-api/internal/capacity"
	"github.com/ic3software/vtafarm-api/internal/resourceprofile"
	"github.com/ic3software/vtafarm-api/internal/testutil"
	"gorm.io/gorm"
)

func TestResourceDefaultsRejectInvalidInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &SetupHandler{}
	for _, body := range []string{
		`{`, `{}`, `{"resources":[]}`,
		`{"resources":[{"component":"vta","memory_request":"16Mi","memory_limit":"64Mi"}]}`,
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.AdminSaveResourceDefaults(c)
		if w.Code != http.StatusBadRequest && w.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid body %s returned %d", body, w.Code)
		}
	}
}

func TestResourceDefaultsPersistence(t *testing.T) {
	scoped := testutil.Postgres(t)

	gin.SetMode(gin.TestMode)
	h := &SetupHandler{db: scoped}
	router := gin.New()
	router.GET("/defaults", h.AdminResourceDefaults)
	router.PUT("/defaults", h.AdminSaveResourceDefaults)
	request := func(method, body string, want int) *httptest.ResponseRecorder {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/defaults", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s returned %d, want %d: %s", method, w.Code, want, w.Body.String())
		}
		return w
	}
	read := func() resourceprofile.Profiles {
		t.Helper()
		w := request(http.MethodGet, "", http.StatusOK)
		var response struct {
			Resources resourceprofile.Profiles `json:"resources"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.Resources
	}
	factory := read()
	var requestTotal, limitTotal int64
	for component := range factory {
		resources := factory.Requirements(component)
		requestTotal += resources.Requests.Memory().Value()
		limitTotal += resources.Limits.Memory().Value()
	}
	if requestTotal != 336<<20 || limitTotal != 960<<20 {
		t.Fatalf("factory totals = %d / %d", requestTotal, limitTotal)
	}
	inputs := []workloadResourceInput{
		testResourceInput("mediator", "128Mi", "256Mi"),
		testResourceInput("vtc", "64Mi", "256Mi"),
		testResourceInput("vta", "16Mi", "1Gi"),
		testResourceInput("dids", "64Mi", "128Mi"),
	}
	encode := func() string {
		b, err := json.Marshal(map[string]any{"resources": inputs})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	request(http.MethodPut, encode(), http.StatusOK)
	inputs = append(inputs, testResourceInput("trust-registry", "128Mi", "512Mi"))
	request(http.MethodPut, encode(), http.StatusOK)
	saved, err := resourceprofile.Load(context.Background(), scoped.Session(&gorm.Session{NewDB: true}))
	if err != nil {
		t.Fatal(err)
	}
	if saved["vta"].MemoryLimit != "1Gi" || read()["vta"].MemoryLimit != "1Gi" {
		t.Fatal("saved defaults were not reloaded")
	}
	if saved["trust-registry"].MemoryRequest != "128Mi" || saved["trust-registry"].MemoryLimit != "512Mi" || read()["trust-registry"].MemoryLimit != "512Mi" {
		t.Fatal("saved Trust Registry defaults were not reloaded")
	}
	if capacity.Modes(saved)[0].Components[0].MemBytes != 1<<30 {
		t.Fatal("capacity did not use saved defaults")
	}
	if saved["mediator"].CPURequest != "50m" {
		t.Fatal("CPU changed")
	}
	for _, invalid := range []workloadResourceInput{
		testResourceInput("dids", "15Mi", "128Mi"),
		testResourceInput("dids", "64Mi", "1025Mi"),
		testResourceInput("dids", "256Mi", "64Mi"),
		{Component: "vta", CPURequest: "250m", CPULimit: "100m", MemoryRequest: "16Mi", MemoryLimit: "64Mi"},
		{Component: "unknown", CPURequest: "10m", CPULimit: "100m", MemoryRequest: "16Mi", MemoryLimit: "64Mi"},
	} {
		inputs[2].MemoryLimit = "64Mi"
		inputs[3] = invalid
		request(http.MethodPut, encode(), http.StatusUnprocessableEntity)
		if read()["vta"].MemoryLimit != "1Gi" {
			t.Fatal("invalid batch partially updated defaults")
		}
	}
	inputs[3] = testResourceInput("dids", "64Mi", "128Mi")
	request(http.MethodPut, encode(), http.StatusOK)
	if read()["vta"].MemoryLimit != "64Mi" {
		t.Fatal("factory restore failed")
	}
	down, err := os.ReadFile("../../migrations/000038_resource_defaults.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := scoped.Exec(string(down)).Error; err != nil {
		t.Fatal(err)
	}
	request(http.MethodGet, "", http.StatusInternalServerError)
}
