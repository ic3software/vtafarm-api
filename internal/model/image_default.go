package model

import "time"

// ImageDefault is the image an admin pinned as the user-facing default for a
// component. No row means the frontend falls back to the "latest" channel.
type ImageDefault struct {
	Component string `gorm:"primaryKey"`
	Image     string `gorm:"not null"`
	UpdatedAt time.Time
}
