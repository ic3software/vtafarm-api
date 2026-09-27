package model

import "time"

// WorkloadResource is the admin-selected memory desired state for one
// long-running component. Kubernetes remains the source of observed state.
type WorkloadResource struct {
	ID             uint   `gorm:"primaryKey;autoIncrement"`
	SetupSessionID uint   `gorm:"not null;uniqueIndex:workload_resources_session_component_key"`
	Component      string `gorm:"not null;uniqueIndex:workload_resources_session_component_key"`
	MemoryRequest  string `gorm:"not null"`
	MemoryLimit    string `gorm:"not null"`
	ApplyError     string `gorm:"not null;default:''"`
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
