package k8s

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCreateVtaDeploymentUsesHealthReadinessProbe(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset()}

	if err := client.CreateVtaDeployment(context.Background(), "test", 42, "example/vta:test"); err != nil {
		t.Fatalf("CreateVtaDeployment() error = %v", err)
	}

	deployment, err := client.kube.AppsV1().Deployments("test").Get(
		context.Background(), VtaDeploymentName(42), metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("container count = %d, want 1", len(deployment.Spec.Template.Spec.Containers))
	}

	probe := deployment.Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.HTTPGet == nil {
		t.Fatal("readiness probe is not an HTTP probe")
	}
	if probe.HTTPGet.Path != "/health" {
		t.Fatalf("readiness path = %q, want /health", probe.HTTPGet.Path)
	}
	if probe.HTTPGet.Port.IntVal != 8100 {
		t.Fatalf("readiness port = %d, want 8100", probe.HTTPGet.Port.IntVal)
	}
	if probe.InitialDelaySeconds != 1 || probe.PeriodSeconds != 2 ||
		probe.TimeoutSeconds != 2 || probe.FailureThreshold != 3 {
		t.Fatalf("unexpected readiness timing: %+v", probe)
	}
}
