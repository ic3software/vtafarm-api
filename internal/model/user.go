package model

import (
	"time"

	"gorm.io/gorm"
)

type User struct {
	ID        uint           `json:"id"         gorm:"primaryKey;autoIncrement"`
	Email     string         `json:"email"      gorm:"not null"`
	Username  string         `json:"username"   gorm:"not null"`
	Password  string         `json:"-"          gorm:"not null"`
	CreatedAt time.Time      `json:"created_at"`
	UpdatedAt time.Time      `json:"updated_at"`
	DeletedAt gorm.DeletedAt `json:"-"          gorm:"index"`
}
