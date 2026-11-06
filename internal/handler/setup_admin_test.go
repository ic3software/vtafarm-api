package handler

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
)

func TestAdminListSessionsSearchAndModeFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.Postgres(t)

	alphaEmail := "alpha.owner@example.com"
	alpha := model.User{UniqueId: "alpha-user", Email: &alphaEmail}
	beta := model.User{UniqueId: "beta-user"}
	for _, user := range []*model.User{&alpha, &beta} {
		if err := db.Create(user).Error; err != nil {
			t.Fatal(err)
		}
	}
	sessions := []model.SetupSession{
		{UserID: alpha.ID, VtaName: "North-Star", Mode: model.ModeVtaOnly, Status: "running", Domain: "example.com", Subdomain: "north"},
		{UserID: alpha.ID, VtaName: "full-alpha", VtcName: "Compass-VTC", Mode: model.ModeFullStack, Status: "running", Domain: "example.com", Subdomain: "alpha"},
		{UserID: beta.ID, VtaName: "south-star", Mode: model.ModeVtaOnly, Status: "failed", Domain: "example.com", Subdomain: "south"},
	}
	for i := range sessions {
		if err := db.Create(&sessions[i]).Error; err != nil {
			t.Fatal(err)
		}
	}
	verifiedAt := time.Now()
	platformDomain := model.Domain{
		UserID: beta.ID, Domain: "platform.example.com", Kind: model.DomainKindPlatform, VerifiedAt: &verifiedAt,
	}
	if err := db.Create(&platformDomain).Error; err != nil {
		t.Fatal(err)
	}
	platform := model.SetupSession{
		UserID: beta.ID, VtaName: "platform-stack", Mode: model.ModeFullStack, Status: "running",
		Domain: platformDomain.Domain, Subdomain: "vta", DomainType: model.DomainPlatform, DomainID: &platformDomain.ID,
	}
	if err := db.Create(&platform).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.WorkloadResource{
		SetupSessionID: sessions[0].ID,
		Component:      "vta",
		MemoryRequest:  "20Mi",
		MemoryLimit:    "64Mi",
	}).Error; err != nil {
		t.Fatal(err)
	}

	h := &SetupHandler{db: db}
	router := gin.New()
	router.GET("/sessions", h.AdminListSessions)
	type item struct {
		VtaName      string  `json:"vta_name"`
		UserID       uint    `json:"user_id"`
		UserUniqueID string  `json:"user_unique_id"`
		UserEmail    *string `json:"user_email"`
		Resources    []struct {
			Component  string `json:"component"`
			Customized bool   `json:"customized"`
		} `json:"resources"`
	}
	type page struct {
		Items    []item   `json:"items"`
		Total    int64    `json:"total"`
		Statuses []string `json:"statuses"`
	}
	request := func(query string) page {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/sessions?"+query, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET ?%s returned %d: %s", query, w.Code, w.Body.String())
		}
		var response page
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}
	assertOne := func(query, want string) page {
		t.Helper()
		response := request(query)
		if response.Total != 1 || len(response.Items) != 1 || response.Items[0].VtaName != want {
			t.Fatalf("GET ?%s = %+v, want only %s", query, response, want)
		}
		return response
	}

	unfiltered := request("")
	if unfiltered.Total != 3 {
		t.Fatalf("unfiltered response = %+v", unfiltered)
	}
	if len(unfiltered.Statuses) != 2 || unfiltered.Statuses[0] != "failed" || unfiltered.Statuses[1] != "running" {
		t.Fatalf("unfiltered statuses = %v", unfiltered.Statuses)
	}
	byName := assertOne("q="+url.QueryEscape("NORTH"), "North-Star")
	if byName.Items[0].UserID != alpha.ID || byName.Items[0].UserUniqueID != alpha.UniqueId ||
		byName.Items[0].UserEmail == nil || *byName.Items[0].UserEmail != alphaEmail {
		t.Fatalf("owner fields = %+v", byName.Items[0])
	}
	if len(byName.Items[0].Resources) != 1 || byName.Items[0].Resources[0].Component != "vta" ||
		!byName.Items[0].Resources[0].Customized {
		t.Fatalf("cached resources = %+v", byName.Items[0].Resources)
	}
	assertOne("q="+url.QueryEscape("compass-vtc"), "full-alpha")
	byEmail := request("q=" + url.QueryEscape("OWNER@EXAMPLE"))
	if byEmail.Total != 2 {
		t.Fatalf("email search total = %d, want 2", byEmail.Total)
	}
	if escaped := request("q=" + url.QueryEscape("%")); escaped.Total != 0 {
		t.Fatalf("literal wildcard search total = %d, want 0", escaped.Total)
	}
	byUserID := request("q=" + fmt.Sprint(alpha.ID))
	if byUserID.Total != 2 {
		t.Fatalf("numeric user search total = %d, want 2", byUserID.Total)
	}
	assertOne("q="+url.QueryEscape("#"+fmt.Sprint(sessions[2].ID)), "south-star")
	assertOne("status=failed", "south-star")
	fullStack := assertOne("mode="+model.ModeFullStack, "full-alpha")
	if len(fullStack.Statuses) != 1 || fullStack.Statuses[0] != "running" {
		t.Fatalf("mode-aware statuses = %v", fullStack.Statuses)
	}
	assertOne("q="+url.QueryEscape(alpha.UniqueId)+"&mode="+model.ModeFullStack, "full-alpha")
	if platformResult := request("q=platform-stack"); platformResult.Total != 0 || len(platformResult.Items) != 0 {
		t.Fatalf("platform stack appeared in sessions response: %+v", platformResult)
	}
}

func TestAdminListSessionsSupportsAllowedPageSizes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.Postgres(t)
	user := model.User{UniqueId: "pagination-user"}
	if err := db.Create(&user).Error; err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= 51; i++ {
		session := model.SetupSession{
			UserID: user.ID, VtaName: fmt.Sprintf("pagination-%02d", i),
			Mode: model.ModeVtaOnly, Status: "running",
			Domain: "example.com", Subdomain: fmt.Sprintf("pagination-%02d", i),
		}
		if err := db.Create(&session).Error; err != nil {
			t.Fatal(err)
		}
	}

	h := &SetupHandler{db: db}
	router := gin.New()
	router.GET("/sessions", h.AdminListSessions)
	request := func(page int, pageSize string) struct {
		Items    []json.RawMessage `json:"items"`
		Total    int64             `json:"total"`
		PageSize int               `json:"page_size"`
	} {
		t.Helper()
		w := httptest.NewRecorder()
		path := fmt.Sprintf("/sessions?page=%d", page)
		if pageSize != "" {
			path += "&page_size=" + pageSize
		}
		router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("GET %s returned %d: %s", path, w.Code, w.Body.String())
		}
		var response struct {
			Items    []json.RawMessage `json:"items"`
			Total    int64             `json:"total"`
			PageSize int               `json:"page_size"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
			t.Fatal(err)
		}
		return response
	}

	firstPage := request(1, "")
	if firstPage.Total != 51 || firstPage.PageSize != 20 || len(firstPage.Items) != 20 {
		t.Fatalf("first page = %+v, want total 51, page size 20, and 20 items", firstPage)
	}
	thirdPage := request(3, "")
	if thirdPage.Total != 51 || thirdPage.PageSize != 20 || len(thirdPage.Items) != 11 {
		t.Fatalf("third page = %+v, want total 51, page size 20, and 11 items", thirdPage)
	}
	for _, pageSize := range []string{"50", "100", "500"} {
		response := request(1, pageSize)
		wantItems := 51
		if pageSize == "50" {
			wantItems = 50
		}
		if response.Total != 51 || fmt.Sprint(response.PageSize) != pageSize || len(response.Items) != wantItems {
			t.Fatalf("page_size=%s response = %+v, want total 51 and %d items", pageSize, response, wantItems)
		}
	}
}

func TestAdminListSessionsRejectsInvalidPageSize(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &SetupHandler{}
	for _, pageSize := range []string{"0", "10", "twenty"} {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodGet, "/?page_size="+pageSize, nil)
		h.AdminListSessions(c)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("page_size=%s returned %d, want 400", pageSize, w.Code)
		}
	}
}

func TestUpgradeListRejectsInvalidActiveFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &UpgradeHandler{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/?active=false", nil)
	h.List(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("active=false returned %d, want 400", w.Code)
	}
}

func TestUpgradeListActiveOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := testutil.Postgres(t)
	admin := model.Admin{UniqueId: "activeadmin"}
	if err := db.Create(&admin).Error; err != nil {
		t.Fatal(err)
	}
	statuses := []string{
		model.UpgradeBatchRunning,
		model.UpgradeBatchCompleted,
		model.UpgradeBatchPaused,
		model.UpgradeBatchCompleted,
	}
	var paused model.UpgradeBatch
	for _, status := range statuses {
		batch := model.UpgradeBatch{AdminID: &admin.ID, Status: status}
		if err := db.Create(&batch).Error; err != nil {
			t.Fatal(err)
		}
		if status == model.UpgradeBatchPaused {
			paused = batch
		}
	}

	h := &UpgradeHandler{db: db}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/?active=true", nil)
	h.List(c)
	if w.Code != http.StatusOK {
		t.Fatalf("active list returned %d: %s", w.Code, w.Body.String())
	}
	var response []model.UpgradeBatch
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response) != 1 || response[0].ID != paused.ID {
		t.Fatalf("active list = %+v, want paused batch %d", response, paused.ID)
	}
}

func TestAdminListSessionsRejectsInvalidSearch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &SetupHandler{}
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/?q="+url.QueryEscape(strings.Repeat("x", 201)), nil)
	h.AdminListSessions(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("long q returned %d, want 400", w.Code)
	}
}
