package model

import "time"

type Admin struct {
	ID        uint      `json:"id"         gorm:"primaryKey;autoIncrement"`
	Email     string    `json:"email"      gorm:"not null"`
	Password  string    `json:"-"          gorm:"not null"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
