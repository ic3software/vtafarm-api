package model

import "time"

type ResourceDefault struct {
	Component     string `gorm:"primaryKey"`
	CPURequest    string `gorm:"not null"`
	CPULimit      string `gorm:"not null"`
	MemoryRequest string `gorm:"not null"`
	MemoryLimit   string `gorm:"not null"`
	StorageSize   string `gorm:"not null"`
	UpdatedAt     time.Time
}
