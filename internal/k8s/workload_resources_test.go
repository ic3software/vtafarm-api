package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestDefaultResourceProfiles(t *testing.T) {
	tests := []struct {
		component, cpu, request, limit string
	}{
		{ComponentVTA, "10m", "128Mi", "512Mi"},
		{ComponentMediator, "50m", "128Mi", "256Mi"},
		{ComponentDids, "10m", "128Mi", "512Mi"},
		{ComponentVTC, "10m", "256Mi", "512Mi"},
	}
	for _, test := range tests {
		profile, ok := DefaultResourceProfile(test.component)
		if !ok {
			t.Fatalf("missing profile for %s", test.component)
		}
		if profile.CPURequest != test.cpu || profile.CPULimit != "" ||
			profile.MemoryRequest != test.request || profile.MemoryLimit != test.limit {
			t.Errorf("%s profile = %+v", test.component, profile)
		}
	}
	if _, ok := DefaultResourceProfile("unknown"); ok {
		t.Fatal("unknown component has a default profile")
	}
}

func TestValidateMemoryResources(t *testing.T) {
	for _, test := range []struct {
		request, limit string
		valid          bool
	}{
		{"128Mi", "512Mi", true},
		{"1Gi", "512Mi", false},
		{"", "512Mi", false},
		{"0", "512Mi", false},
		{"128Mi", "invalid", false},
	} {
		err := ValidateMemoryResources(test.request, test.limit)
		if (err == nil) != test.valid {
			t.Errorf("ValidateMemoryResources(%q, %q) error = %v", test.request, test.limit, err)
		}
	}
}

func TestUpdateDeploymentMemoryPreservesCPUAndUsesRecreate(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "fs-7-vtc", Namespace: "test"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "fs-7-vtc",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("10m"),
						corev1.ResourceMemory: resource.MustParse("32Mi"),
					},
					Limits: corev1.ResourceList{corev1.ResourceMemory: resource.MustParse("64Mi")},
				},
			}},
		}}},
	}
	client := &Client{kube: fake.NewSimpleClientset(deployment)}
	if err := client.UpdateDeploymentMemory(context.Background(), "test", "fs-7-vtc", "256Mi", "512Mi"); err != nil {
		t.Fatal(err)
	}

	updated, err := client.kube.AppsV1().Deployments("test").Get(context.Background(), "fs-7-vtc", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	resources := updated.Spec.Template.Spec.Containers[0].Resources
	if got := resources.Requests.Cpu().String(); got != "10m" {
		t.Errorf("CPU request = %s, want 10m", got)
	}
	if got := resources.Requests.Memory().String(); got != "256Mi" {
		t.Errorf("memory request = %s, want 256Mi", got)
	}
	if got := resources.Limits.Memory().String(); got != "512Mi" {
		t.Errorf("memory limit = %s, want 512Mi", got)
	}
	if updated.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType || updated.Spec.Strategy.RollingUpdate != nil {
		t.Errorf("strategy = %+v, want Recreate", updated.Spec.Strategy)
	}
}
