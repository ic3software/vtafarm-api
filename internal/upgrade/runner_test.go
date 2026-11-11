package upgrade

import (
	"os"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
	"gorm.io/gorm"
)

func makePlatformSession(t *testing.T, db *gorm.DB) model.SetupSession {
	t.Helper()
	session := testutil.SetupSession(t, db, model.ModeFullStack)
	domain := model.Domain{
		UserID: session.UserID,
		Domain: "platform-" + session.VtaName + ".example.com",
		Kind:   model.DomainKindPlatform,
	}
	if err := db.Create(&domain).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&session).Updates(map[string]any{
		"domain_id":   domain.ID,
		"domain_type": model.DomainPlatform,
	}).Error; err != nil {
		t.Fatal(err)
	}
	session.DomainID = &domain.ID
	session.DomainType = model.DomainPlatform
	return session
}

func TestRecordSuccessfulUpgradeSyncsPlatformVtaDefault(t *testing.T) {
	db := testutil.Postgres(t)
	session := makePlatformSession(t, db)
	if err := db.Model(&session).Update("vta_image", "ghcr.io/o/vta:1.0.0-old").Error; err != nil {
		t.Fatal(err)
	}

	assertImage := func(wantSession, wantDefault string) {
		t.Helper()
		var gotSession model.SetupSession
		if err := db.First(&gotSession, session.ID).Error; err != nil {
			t.Fatal(err)
		}
		var gotDefault model.ImageDefault
		if err := db.First(&gotDefault, "component = ?", "vta").Error; err != nil {
			t.Fatal(err)
		}
		if gotSession.VtaImage != wantSession || gotDefault.Image != wantDefault {
			t.Fatalf("images = session %q, default %q; want %q, %q",
				gotSession.VtaImage, gotDefault.Image, wantSession, wantDefault)
		}
	}

	first := model.UpgradeTask{Component: "vta", ToImage: "ghcr.io/o/vta:1.0.0-new"}
	if err := recordSuccessfulUpgrade(db, &session, &first); err != nil {
		t.Fatal(err)
	}
	assertImage(first.ToImage, first.ToImage)

	// An admin may override the default until the platform VTA is updated again.
	if err := db.Model(&model.ImageDefault{}).Where("component = ?", "vta").
		Update("image", "ghcr.io/o/vta:2.0.0-manual").Error; err != nil {
		t.Fatal(err)
	}
	second := model.UpgradeTask{Component: "vta", ToImage: "ghcr.io/o/vta:1.0.1-new"}
	if err := recordSuccessfulUpgrade(db, &session, &second); err != nil {
		t.Fatal(err)
	}
	assertImage(second.ToImage, second.ToImage)
}

func TestRecordSuccessfulUpgradeLeavesDefaultForOtherTargets(t *testing.T) {
	db := testutil.Postgres(t)
	if err := db.Create(&model.ImageDefault{
		Component: "vta",
		Image:     "ghcr.io/o/vta:2.0.0-manual",
	}).Error; err != nil {
		t.Fatal(err)
	}

	userSession := testutil.SetupSession(t, db, model.ModeVtaOnly)
	userTask := model.UpgradeTask{Component: "vta", ToImage: "ghcr.io/o/vta:1.0.1-user"}
	if err := recordSuccessfulUpgrade(db, &userSession, &userTask); err != nil {
		t.Fatal(err)
	}

	platformSession := makePlatformSession(t, db)
	if err := db.Model(&platformSession).Update("mediator_image", "ghcr.io/o/mediator:1.0.0-old").Error; err != nil {
		t.Fatal(err)
	}
	mediatorTask := model.UpgradeTask{Component: "mediator", ToImage: "ghcr.io/o/mediator:1.0.1-new"}
	if err := recordSuccessfulUpgrade(db, &platformSession, &mediatorTask); err != nil {
		t.Fatal(err)
	}

	var imageDefault model.ImageDefault
	if err := db.First(&imageDefault, "component = ?", "vta").Error; err != nil {
		t.Fatal(err)
	}
	if imageDefault.Image != "ghcr.io/o/vta:2.0.0-manual" {
		t.Fatalf("default image = %q, want manual override", imageDefault.Image)
	}
	// The platform mediator upgrade syncs its own component's default.
	var mediatorDefault model.ImageDefault
	if err := db.First(&mediatorDefault, "component = ?", "mediator").Error; err != nil {
		t.Fatal(err)
	}
	if mediatorDefault.Image != mediatorTask.ToImage {
		t.Fatalf("mediator default = %q, want %q", mediatorDefault.Image, mediatorTask.ToImage)
	}
}

func TestRecordSuccessfulUpgradeLeavesDefaultForUserComponents(t *testing.T) {
	db := testutil.Postgres(t)
	userSession := testutil.SetupSession(t, db, model.ModeFullStack)
	task := model.UpgradeTask{Component: "mediator", ToImage: "ghcr.io/o/mediator:1.0.1-user"}
	if err := recordSuccessfulUpgrade(db, &userSession, &task); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&model.ImageDefault{}).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("user upgrade wrote %d image defaults", count)
	}
}

func TestSyncPlatformImageDefaults(t *testing.T) {
	db := testutil.Postgres(t)
	defaults := func() map[string]string {
		t.Helper()
		var rows []model.ImageDefault
		if err := db.Find(&rows).Error; err != nil {
			t.Fatal(err)
		}
		got := map[string]string{}
		for _, row := range rows {
			got[row.Component] = row.Image
		}
		return got
	}

	user := model.SetupSession{DomainType: model.DomainManaged, VtaImage: "ghcr.io/o/vta:1-user"}
	if err := model.SyncPlatformImageDefaults(db, &user, model.UpgradeComponents...); err != nil {
		t.Fatal(err)
	}
	if got := defaults(); len(got) != 0 {
		t.Fatalf("non-platform session wrote %v", got)
	}

	if err := db.Create(&model.ImageDefault{Component: "vtc", Image: "ghcr.io/o/vtc:0-manual"}).Error; err != nil {
		t.Fatal(err)
	}
	platform := model.SetupSession{
		DomainType:         model.DomainPlatform,
		VtaImage:           "ghcr.io/o/vta:1-a",
		MediatorImage:      "ghcr.io/o/mediator:1-a",
		DidsImage:          "", // not recorded: skipped, no row written
		VtcImage:           "ghcr.io/o/vtc:1-a",
		TrustRegistryImage: "ghcr.io/o/trust-registry:1-a",
		TrustRegistryState: "enabling", // not active: skipped
	}
	if err := model.SyncPlatformImageDefaults(db, &platform, model.UpgradeComponents...); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"vta":      platform.VtaImage,
		"mediator": platform.MediatorImage,
		"vtc":      platform.VtcImage, // replaces the manual pin
	}
	if got := defaults(); len(got) != len(want) || got["vta"] != want["vta"] || got["mediator"] != want["mediator"] || got["vtc"] != want["vtc"] {
		t.Fatalf("defaults = %v, want %v", got, want)
	}

	platform.TrustRegistryState = "active"
	if err := model.SyncPlatformImageDefaults(db, &platform, "trust-registry"); err != nil {
		t.Fatal(err)
	}
	if got := defaults(); got["trust-registry"] != platform.TrustRegistryImage || len(got) != 4 {
		t.Fatalf("after registry enable, defaults = %v", got)
	}
}

func TestBackfillPlatformImageDefaultsMigration(t *testing.T) {
	db := testutil.Postgres(t)
	migration, err := os.ReadFile("../../migrations/000053_backfill_platform_image_defaults.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	platform := makePlatformSession(t, db)
	if err := db.Model(&platform).Updates(map[string]any{
		"status":               "running",
		"vta_image":            "ghcr.io/o/vta:1-platform",
		"mediator_image":       "ghcr.io/o/mediator:1-platform",
		"dids_image":           "ghcr.io/o/dids:1-platform",
		"vtc_image":            "ghcr.io/o/vtc:1-platform",
		"trust_registry_image": "ghcr.io/o/trust-registry:1-platform",
		"trust_registry_state": "active",
	}).Error; err != nil {
		t.Fatal(err)
	}
	user := testutil.SetupSession(t, db, model.ModeFullStack)
	if err := db.Model(&user).Updates(map[string]any{"status": "running", "dids_image": "ghcr.io/o/dids:1-user"}).Error; err != nil {
		t.Fatal(err)
	}
	// An existing row is kept, whether a synced VTA default or a manual pin.
	if err := db.Create(&model.ImageDefault{Component: "vta", Image: "ghcr.io/o/vta:0-kept"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(string(migration)).Error; err != nil {
		t.Fatal(err)
	}

	var rows []model.ImageDefault
	if err := db.Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, row := range rows {
		got[row.Component] = row.Image
	}
	want := map[string]string{
		"vta":            "ghcr.io/o/vta:0-kept",
		"mediator":       "ghcr.io/o/mediator:1-platform",
		"dids":           "ghcr.io/o/dids:1-platform",
		"vtc":            "ghcr.io/o/vtc:1-platform",
		"trust-registry": "ghcr.io/o/trust-registry:1-platform",
	}
	if len(got) != len(want) {
		t.Fatalf("defaults = %v, want %v", got, want)
	}
	for component, image := range want {
		if got[component] != image {
			t.Fatalf("defaults = %v, want %v", got, want)
		}
	}
}
