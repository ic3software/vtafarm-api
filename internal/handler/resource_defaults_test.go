package handler

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ic3software/vtafarm-api/internal/capacity"
	"github.com/ic3software/vtafarm-api/internal/resourceprofile"
	"gorm.io/driver/postgres"
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

// Use a disposable PostgreSQL database; each run owns a separate schema.
func TestResourceDefaultsPersistence(t *testing.T) {
	dsn := os.Getenv("VTAFARM_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("VTAFARM_TEST_DATABASE_URL is not set")
	}
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer sqlDB.Close()
	schema := fmt.Sprintf("resource_defaults_test_%d", time.Now().UnixNano())
	if err := db.Exec("CREATE SCHEMA " + schema).Error; err != nil {
		t.Fatal(err)
	}
	defer db.Exec("DROP SCHEMA " + schema + " CASCADE")
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()
	scoped, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	scopedSQL, _ := scoped.DB()
	defer scopedSQL.Close()
	migration, err := os.ReadFile("../../migrations/000038_resource_defaults.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	if err := scoped.Exec(string(migration)).Error; err != nil {
		t.Fatal(err)
	}

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
	if requestTotal != 272<<20 || limitTotal != 704<<20 {
		t.Fatalf("factory totals = %d / %d", requestTotal, limitTotal)
	}
	inputs := []memoryResourceInput{
		{Component: "mediator", MemoryRequest: "128Mi", MemoryLimit: "256Mi"},
		{Component: "vtc", MemoryRequest: "64Mi", MemoryLimit: "256Mi"},
		{Component: "vta", MemoryRequest: "16Mi", MemoryLimit: "1Gi"},
		{Component: "dids", MemoryRequest: "64Mi", MemoryLimit: "128Mi"},
	}
	encode := func() string {
		b, err := json.Marshal(map[string]any{"resources": inputs})
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	request(http.MethodPut, encode(), http.StatusOK)
	saved, err := resourceprofile.Load(context.Background(), scoped.Session(&gorm.Session{NewDB: true}))
	if err != nil {
		t.Fatal(err)
	}
	if saved["vta"].MemoryLimit != "1Gi" || read()["vta"].MemoryLimit != "1Gi" {
		t.Fatal("saved defaults were not reloaded")
	}
	if capacity.Modes(saved)[0].Components[0].MemBytes != 1<<30 {
		t.Fatal("capacity did not use saved defaults")
	}
	if saved["mediator"].CPURequest != "50m" {
		t.Fatal("CPU changed")
	}
	for _, invalid := range []memoryResourceInput{
		{Component: "dids", MemoryRequest: "15Mi", MemoryLimit: "128Mi"},
		{Component: "dids", MemoryRequest: "64Mi", MemoryLimit: "1025Mi"},
		{Component: "dids", MemoryRequest: "256Mi", MemoryLimit: "64Mi"},
		{Component: "vta", MemoryRequest: "16Mi", MemoryLimit: "64Mi"},
		{Component: "unknown", MemoryRequest: "16Mi", MemoryLimit: "64Mi"},
	} {
		inputs[2].MemoryLimit = "64Mi"
		inputs[3] = invalid
		request(http.MethodPut, encode(), http.StatusUnprocessableEntity)
		if read()["vta"].MemoryLimit != "1Gi" {
			t.Fatal("invalid batch partially updated defaults")
		}
	}
	inputs[3] = memoryResourceInput{Component: "dids", MemoryRequest: "64Mi", MemoryLimit: "128Mi"}
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
