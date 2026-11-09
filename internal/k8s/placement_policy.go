package k8s

import (
	"maps"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ic3software/vtafarm-api/internal/config"
)

const (
	PlacementLabelWorkloadClass = "vtafarm.ic3.dev/workload-class"
	PlacementLabelMode          = "vtafarm.ic3.dev/mode"
	PlacementLabelComponent     = "vtafarm.ic3.dev/component"
	PlacementLabelAutoRebalance = "vtafarm.ic3.dev/auto-rebalance"
	PlacementLabelMemoryState   = "vtafarm.ic3.dev/memory-state"
	PlacementLabelVTAEligible   = "vtafarm.ic3.dev/vta-eligible"
	PlacementGateName           = "vtafarm.ic3.dev/placement-ready"
	TenantWorkloadClass         = "tenant"
	PlacementModeVTAOnly        = "vta-only"
	PlacementModeFullStack      = "full-stack"
)

func TenantPlacementLabels(mode, component string) map[string]string {
	return map[string]string{
		PlacementLabelWorkloadClass: TenantWorkloadClass,
		PlacementLabelMode:          mode,
		PlacementLabelComponent:     component,
	}
}

func mergeLabels(base, extra map[string]string) map[string]string {
	merged := maps.Clone(base)
	if merged == nil {
		merged = map[string]string{}
	}
	maps.Copy(merged, extra)
	return merged
}

func TenantTopologySpread() []corev1.TopologySpreadConstraint {
	return []corev1.TopologySpreadConstraint{{
		MaxSkew:           1,
		TopologyKey:       corev1.LabelHostname,
		WhenUnsatisfiable: corev1.ScheduleAnyway,
		LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
			PlacementLabelWorkloadClass: TenantWorkloadClass,
		}},
	}}
}

func VTAPlacementAffinity() *corev1.Affinity {
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{MatchExpressions: []corev1.NodeSelectorRequirement{
				{Key: PlacementLabelVTAEligible, Operator: corev1.NodeSelectorOpIn, Values: []string{"true"}},
				{Key: PlacementLabelMemoryState, Operator: corev1.NodeSelectorOpIn, Values: []string{"green", "yellow", "orange"}},
			}}},
		},
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{
			preferredMemoryState("green", 100),
			preferredMemoryState("yellow", 50),
			preferredMemoryState("orange", 10),
		},
	}}
}

func preferredMemoryState(state string, weight int32) corev1.PreferredSchedulingTerm {
	return corev1.PreferredSchedulingTerm{
		Weight: weight,
		Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{
			Key: PlacementLabelMemoryState, Operator: corev1.NodeSelectorOpIn, Values: []string{state},
		}}},
	}
}

func (c *Client) placementActive(sessionID uint) bool {
	switch c.placementMode {
	case config.PlacementEnabled:
		return true
	case config.PlacementCanary:
		_, ok := c.placementCanary[sessionID]
		return ok
	default:
		return false
	}
}

func (c *Client) applyVTAPlacement(deployment *appsv1.Deployment, sessionID uint) {
	placementLabels := TenantPlacementLabels(PlacementModeVTAOnly, "vta")
	placementLabels[PlacementLabelAutoRebalance] = "true"
	deployment.Labels = mergeLabels(deployment.Labels, placementLabels)
	deployment.Spec.Template.Labels = mergeLabels(deployment.Spec.Template.Labels, placementLabels)
	deployment.Spec.Template.Spec.TopologySpreadConstraints = TenantTopologySpread()
	deployment.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
	if !c.placementActive(sessionID) {
		return
	}
	deployment.Spec.Template.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: PlacementGateName}}
	deployment.Spec.Template.Spec.Affinity = VTAPlacementAffinity()
}

func applyComponentPlacement(deployment *appsv1.Deployment, mode, component string) {
	if mode == "" || component == "" {
		return
	}
	labels := TenantPlacementLabels(mode, component)
	deployment.Labels = mergeLabels(deployment.Labels, labels)
	deployment.Spec.Template.Labels = mergeLabels(deployment.Spec.Template.Labels, labels)
	deployment.Spec.Template.Spec.TopologySpreadConstraints = TenantTopologySpread()
}
