package model

import (
	"encoding/json"
	"time"

	"gorm.io/gorm"
)

type PodDeployment struct {
	ID        uint            `json:"id"         gorm:"primaryKey"`
	UserID    uint            `json:"user_id"    gorm:"not null;index"`
	User      User            `json:"-"          gorm:"foreignKey:UserID"`
	Name      string          `json:"name"       gorm:"not null"`
	Namespace string          `json:"namespace"  gorm:"not null"`
	Spec      json.RawMessage `json:"spec"       gorm:"type:jsonb;not null;default:'{}'"`
	Status    string          `json:"status"     gorm:"default:pending"`
	CreatedAt time.Time       `json:"created_at"`
	UpdatedAt time.Time       `json:"updated_at"`
	DeletedAt gorm.DeletedAt  `json:"-"          gorm:"index"`
}
