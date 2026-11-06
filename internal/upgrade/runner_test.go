package upgrade

import (
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
}
