package model

import "time"

const (
	IncidentFiring   = "firing"
	IncidentResolved = "resolved"
)

type Incident struct {
	ID              uint       `gorm:"primaryKey;autoIncrement" json:"-"`
	PublicID        string     `gorm:"not null;uniqueIndex" json:"id"`
	SetupSessionID  uint       `gorm:"not null;index" json:"-"`
	Cluster         string     `gorm:"not null" json:"cluster"`
	Fingerprint     string     `gorm:"not null" json:"-"`
	AlertName       string     `gorm:"not null" json:"alert_name"`
	IncidentType    string     `gorm:"not null" json:"type"`
	Component       string     `gorm:"not null" json:"component"`
	Resource        string     `gorm:"not null;default:''" json:"resource,omitempty"`
	Severity        string     `gorm:"not null" json:"severity"`
	Status          string     `gorm:"not null" json:"status"`
	Title           string     `gorm:"not null" json:"title"`
	Message         string     `gorm:"not null" json:"message"`
	UsagePercent    *float64   `json:"usage_percent,omitempty"`
	StartedAt       time.Time  `gorm:"not null;index" json:"started_at"`
	ResolvedAt      *time.Time `json:"resolved_at,omitempty"`
	LastSeenAt      time.Time  `gorm:"not null" json:"last_seen_at"`
	ReadAt          *time.Time `json:"read_at,omitempty"`
	Suppressed      bool       `gorm:"not null;default:false" json:"-"`
	LabelsJSON      string     `gorm:"column:labels;type:jsonb;not null" json:"-"`
	AnnotationsJSON string     `gorm:"column:annotations;type:jsonb;not null" json:"-"`
	CreatedAt       time.Time  `json:"created_at"`
	UpdatedAt       time.Time  `json:"updated_at"`
}

func (i Incident) DowntimeSeconds() *int64 {
	if i.ResolvedAt == nil {
		return nil
	}
	seconds := int64(i.ResolvedAt.Sub(i.StartedAt).Seconds())
	if seconds < 0 {
		seconds = 0
	}
	return &seconds
}
