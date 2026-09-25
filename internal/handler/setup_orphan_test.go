package handler

import (
	"testing"

	"github.com/ic3software/vtafarm-api/internal/model"
)

func TestIsOrphaned(t *testing.T) {
	providerID := uint(7)
	tests := []struct {
		name   string
		source string
		pid    *uint
		want   bool
	}{
		{"connected in farm", model.ConnectionInFarm, &providerID, false},
		{"provider deleted", model.ConnectionInFarm, nil, true},
		{"platform default", model.ConnectionPlatform, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &model.SetupSession{ConnectionSource: tc.source, ProviderSessionID: tc.pid}
			if got := s.IsOrphaned(); got != tc.want {
				t.Errorf("IsOrphaned() = %v, want %v", got, tc.want)
			}
		})
	}
}
