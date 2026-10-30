package handler

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ic3software/vtafarm-api/internal/ghcr"
	"github.com/ic3software/vtafarm-api/internal/testutil"
)

func TestMarkImageDefault(t *testing.T) {
	list := func() []ghcr.ImageTag {
		return []ghcr.ImageTag{
			{Tag: "0.67.0-abc", Image: "ghcr.io/o/vta:0.67.0-abc", Latest: true},
			{Tag: "0.66.0-def", Image: "ghcr.io/o/vta:0.66.0-def"},
		}
	}
	flagged := func(tags []ghcr.ImageTag) []string {
		var out []string
		for _, tag := range tags {
			if tag.Default {
				out = append(out, tag.Tag)
			}
		}
		return out
	}

	tags := list()
	markImageDefault(tags, "ghcr.io/o/vta:0.66.0-def")
	if got := flagged(tags); len(got) != 1 || got[0] != "0.66.0-def" {
		t.Fatalf("pinned image not flagged: %v", got)
	}
	if !tags[0].Latest {
		t.Fatal("latest flag must survive")
	}

	tags = list()
	markImageDefault(tags, "ghcr.io/o/vta:0.1.0-gone")
	if got := flagged(tags); len(got) != 0 {
		t.Fatalf("stale pin flagged %v", got)
	}

	tags = list()
	markImageDefault(tags, "")
	if got := flagged(tags); len(got) != 0 {
		t.Fatalf("no pin flagged %v", got)
	}
}

func TestSaveImageDefaultRejectsInvalidInput(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &SetupHandler{}
	for body, want := range map[string]int{
		`{`:                                   http.StatusBadRequest,
		`{}`:                                  http.StatusBadRequest,
		`{"component":"","image":"x"}`:        http.StatusBadRequest,
		`{"component":"unknown","image":"x"}`: http.StatusBadRequest,
		`{"component":"vta","image":"ghcr.io/o/vta:1-a"}`: http.StatusServiceUnavailable, // no registry client configured
	} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		h.AdminSaveImageDefault(c)
		if w.Code != want {
			t.Fatalf("body %s returned %d, want %d: %s", body, w.Code, want, w.Body.String())
		}
	}
}

func TestImageDefaultsPersistence(t *testing.T) {
	scoped := testutil.Postgres(t)

	gin.SetMode(gin.TestMode)
	h := &SetupHandler{db: scoped}
	router := gin.New()
	router.GET("/defaults", h.AdminImageDefaults)
	router.PUT("/defaults", h.AdminSaveImageDefault)
	request := func(method, body string, want int) map[string]string {
		t.Helper()
		w := httptest.NewRecorder()
		req := httptest.NewRequest(method, "/defaults", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("%s returned %d, want %d: %s", method, w.Code, want, w.Body.String())
		}
		var response struct {
			Images map[string]string `json:"images"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response.Images
	}

	if images := request(http.MethodGet, "", http.StatusOK); len(images) != 0 {
		t.Fatalf("fresh table returned %v", images)
	}

	// Seed a pin directly: saving through the API needs a registry round trip.
	if err := scoped.Exec(`INSERT INTO image_defaults (component, image) VALUES ('vta', 'ghcr.io/o/vta:0.66.0-def')`).Error; err != nil {
		t.Fatal(err)
	}
	if images := request(http.MethodGet, "", http.StatusOK); images["vta"] != "ghcr.io/o/vta:0.66.0-def" {
		t.Fatalf("pin not reported: %v", images)
	}
	pinned, err := loadImageDefault(t.Context(), scoped, "vta")
	if err != nil || pinned != "ghcr.io/o/vta:0.66.0-def" {
		t.Fatalf("loadImageDefault = %q, %v", pinned, err)
	}
	if pinned, err := loadImageDefault(t.Context(), scoped, "mediator"); err != nil || pinned != "" {
		t.Fatalf("unpinned component = %q, %v", pinned, err)
	}

	// Clearing needs no registry and removes only that component's row.
	if err := scoped.Exec(`INSERT INTO image_defaults (component, image) VALUES ('vtc', 'ghcr.io/o/vtc:1-a')`).Error; err != nil {
		t.Fatal(err)
	}
	images := request(http.MethodPut, `{"component":"vta","image":""}`, http.StatusOK)
	if _, still := images["vta"]; still || images["vtc"] != "ghcr.io/o/vtc:1-a" {
		t.Fatalf("clear left %v", images)
	}
	// The table's CHECK constraint keeps unknown components out even below the handler.
	if err := scoped.Exec(`INSERT INTO image_defaults (component, image) VALUES ('bogus', 'x')`).Error; err == nil {
		t.Fatal("unknown component accepted by the table")
	}
}
