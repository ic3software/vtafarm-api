package model

import "time"

type Admin struct {
	ID        uint      `json:"id"         gorm:"primaryKey;autoIncrement"`
	UniqueId  string    `json:"unique_id"  gorm:"column:unique_id;not null"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}
