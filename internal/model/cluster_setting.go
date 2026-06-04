package model

import "time"

type ClusterSetting struct {
	ID        uint      `gorm:"primaryKey;autoIncrement"`
	Name      string    `gorm:"not null"`
	IngressIP string    `gorm:"not null"`
	CreatedAt time.Time
	UpdatedAt time.Time
}
