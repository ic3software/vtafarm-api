package model

import (
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ImageDefault is the user-facing default image for a component. It follows
// what the platform stack last deployed successfully — on reaching running, on
// enabling its Trust Registry, and on every component upgrade — and an admin
// can override it by hand until the next such deploy. No row means frontends
// fall back to the "latest" channel.
type ImageDefault struct {
	Component string `gorm:"primaryKey"`
	Image     string `gorm:"not null"`
	UpdatedAt time.Time
}

// UpsertImageDefault sets component's default to image, replacing any row
// already there (a manual override included).
func UpsertImageDefault(tx *gorm.DB, component, image string) error {
	row := ImageDefault{Component: component, Image: image}
	return tx.Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "component"}},
		DoUpdates: clause.AssignmentColumns([]string{"image", "updated_at"}),
	}).Create(&row).Error
}

// SyncPlatformImageDefaults copies s's current image for each component into
// image_defaults when s is the platform stack, and does nothing otherwise.
// Components without a current image (e.g. a Trust Registry that is not
// active) are skipped, so their defaults are left as they are.
func SyncPlatformImageDefaults(tx *gorm.DB, s *SetupSession, components ...string) error {
	if s.DomainType != DomainPlatform {
		return nil
	}
	return tx.Transaction(func(tx *gorm.DB) error {
		for _, component := range components {
			image := s.ComponentImage(component)
			if image == "" {
				continue
			}
			if err := UpsertImageDefault(tx, component, image); err != nil {
				return err
			}
		}
		return nil
	})
}
