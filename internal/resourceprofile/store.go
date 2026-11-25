package resourceprofile

import (
	"context"
	"fmt"

	"gorm.io/gorm"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
)

const FactoryStorageSize = "200Mi"

type Profile struct {
	k8s.ResourceProfile
	StorageSize string `json:"storage_size"`
}

type Profiles map[string]Profile

func Factory() Profiles {
	profiles := make(Profiles, 5)
	for _, component := range []string{k8s.ComponentMediator, k8s.ComponentVTC, k8s.ComponentVTA, k8s.ComponentDids, k8s.ComponentTrustRegistry} {
		resources, _ := k8s.DefaultResourceProfile(component)
		profiles[component] = Profile{ResourceProfile: resources, StorageSize: FactoryStorageSize}
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
		if err := k8s.ValidateCPUSettings(row.CPURequest, row.CPULimit); err != nil {
			return nil, err
		}
		if err := k8s.ValidateMemorySettings(row.MemoryRequest, row.MemoryLimit); err != nil {
			return nil, err
		}
		if err := ValidateStorageSize(row.StorageSize); err != nil {
			return nil, err
		}
		profile.CPURequest, profile.CPULimit = row.CPURequest, row.CPULimit
		profile.MemoryRequest, profile.MemoryLimit = row.MemoryRequest, row.MemoryLimit
		profile.StorageSize = row.StorageSize
		profiles[row.Component] = profile
	}
	return profiles, nil
}

func ValidateStorageSize(value string) error {
	storage, err := resource.ParseQuantity(value)
	if err != nil || storage.Sign() <= 0 {
		return fmt.Errorf("storage_size must be a positive Kubernetes quantity")
	}
	min, max := resource.MustParse("200Mi"), resource.MustParse("2Gi")
	if storage.Cmp(min) < 0 || storage.Cmp(max) > 0 {
		return fmt.Errorf("storage_size must be between 200Mi and 2Gi")
	}
	return nil
}

func (p Profiles) Requirements(component string) corev1.ResourceRequirements {
	profile := p[component]
	return k8s.ComponentResourcesWithCPULimit(profile.CPURequest, profile.CPULimit, profile.MemoryRequest, profile.MemoryLimit)
}
