package model

import "time"

// ImageDefault is the user-facing default image for a component. An admin can
// set it manually; a successful platform VTA upgrade also resets the VTA row.
// No row means the frontend falls back to the "latest" channel.
type ImageDefault struct {
	Component string `gorm:"primaryKey"`
	Image     string `gorm:"not null"`
	UpdatedAt time.Time
}
