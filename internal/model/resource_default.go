package model

import "time"

type ResourceDefault struct {
	Component     string `gorm:"primaryKey"`
	MemoryRequest string `gorm:"not null"`
	MemoryLimit   string `gorm:"not null"`
	UpdatedAt     time.Time
}
