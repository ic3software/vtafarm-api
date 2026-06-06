package model

import "time"

const (
	RoleUser  = "user"
	RoleAdmin = "admin"
)

type User struct {
	ID        uint      `json:"id"         gorm:"primaryKey;autoIncrement"`
	UniqueId  string    `json:"unique_id"  gorm:"column:unique_id;not null"`
	Email     string    `json:"email"      gorm:"not null"`
	Password  string    `json:"-"          gorm:"not null"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
