package resourceprofile

import (
	"context"
	"fmt"

	"gorm.io/gorm"
	corev1 "k8s.io/api/core/v1"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
)

type Profiles map[string]k8s.ResourceProfile

func Factory() Profiles {
	profiles := make(Profiles, 5)
	for _, component := range []string{k8s.ComponentMediator, k8s.ComponentVTC, k8s.ComponentVTA, k8s.ComponentDids, k8s.ComponentTrustRegistry} {
		profiles[component], _ = k8s.DefaultResourceProfile(component)
	}
	return profiles
}

// Load reads a single snapshot on each call so all API replicas see saved
// settings without a process-local cache or restart.
func Load(ctx context.Context, db *gorm.DB) (Profiles, error) {
	var rows []model.ResourceDefault
	if err := db.WithContext(ctx).Find(&rows).Error; err != nil {
		return nil, err
	}
	return FromRows(rows)
}

// FromRows overlays persisted admin settings on the factory profiles. It is
// also used when a larger query has already fetched resource_defaults.
func FromRows(rows []model.ResourceDefault) (Profiles, error) {
	profiles := Factory()
	for _, row := range rows {
		profile, ok := profiles[row.Component]
		if !ok {
			return nil, fmt.Errorf("unknown resource component %q", row.Component)
		}
		if err := k8s.ValidateMemorySettings(row.MemoryRequest, row.MemoryLimit); err != nil {
			return nil, err
		}
		profile.MemoryRequest, profile.MemoryLimit = row.MemoryRequest, row.MemoryLimit
		profiles[row.Component] = profile
	}
	return profiles, nil
}

func (p Profiles) Requirements(component string) corev1.ResourceRequirements {
	profile := p[component]
	return k8s.ComponentResources(profile.CPURequest, profile.MemoryRequest, profile.MemoryLimit)
}
