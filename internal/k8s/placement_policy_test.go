package k8s

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ic3software/vtafarm-api/internal/config"
)

func TestApplyVTAWorkloadPolicyAddsStableMetadataWithoutGating(t *testing.T) {
	deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{}}}

	applyVTAWorkloadPolicy(deployment)

	if got := deployment.Labels[PlacementLabelMode]; got != PlacementModeVTAOnly {
		t.Fatalf("mode label = %q", got)
	}
	if got := deployment.Spec.Template.Labels[PlacementLabelAutoRebalance]; got != "true" {
		t.Fatalf("auto-rebalance label = %q", got)
	}
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("strategy = %q", deployment.Spec.Strategy.Type)
	}
	if len(deployment.Spec.Template.Spec.TopologySpreadConstraints) != 1 {
		t.Fatal("tenant topology spread was not added")
	}
	if len(deployment.Spec.Template.Spec.SchedulingGates) != 0 {
		t.Fatal("initial workload policy added a scheduling gate")
	}
	if deployment.Spec.Template.Spec.Affinity != nil {
		t.Fatal("initial workload policy added placement affinity")
	}
}

func TestApplyVTARelocationPolicyGatesOnlyActivatedSessions(t *testing.T) {
	tests := []struct {
		name   string
		client Client
		id     uint
		gated  bool
	}{
		{name: "enabled", client: Client{placementMode: config.PlacementEnabled}, id: 1, gated: true},
		{name: "canary included", client: Client{placementMode: config.PlacementCanary, placementCanary: map[uint]struct{}{7: {}}}, id: 7, gated: true},
		{name: "canary excluded", client: Client{placementMode: config.PlacementCanary, placementCanary: map[uint]struct{}{7: {}}}, id: 8},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			deployment := &appsv1.Deployment{Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{}}}
			tt.client.applyVTARelocationPolicy(deployment, tt.id)
			if got := len(deployment.Spec.Template.Spec.SchedulingGates) == 1; got != tt.gated {
				t.Fatalf("gated = %v, want %v", got, tt.gated)
			}
			if tt.gated && deployment.Spec.Template.Spec.Affinity == nil {
				t.Fatal("activated placement has no required node affinity")
			}
		})
	}
}

func TestApplyComponentPlacementLabelsFullStack(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "mediator"}},
		Spec:       appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "mediator"}}}},
	}

	applyComponentPlacement(deployment, PlacementModeFullStack, "mediator")

	if got := deployment.Spec.Template.Labels[PlacementLabelComponent]; got != "mediator" {
		t.Fatalf("component label = %q", got)
	}
	if got := deployment.Spec.Template.Labels[PlacementLabelMode]; got != PlacementModeFullStack {
		t.Fatalf("mode label = %q", got)
	}
	if len(deployment.Spec.Template.Spec.SchedulingGates) != 0 {
		t.Fatal("full-stack component must not be placement-gated")
	}
}
