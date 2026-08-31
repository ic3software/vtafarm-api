package k8s

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestCreateComponentPVCUsesRequestedStorageSize(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset()}

	tests := []struct {
		name string
		size string
	}{
		{name: "vta", size: VtaPVCStorageSize},
		{name: "did-hosting", size: DIDHostingPVCStorageSize},
		{name: "mediator", size: MediatorPVCStorageSize},
		{name: "vtc", size: VtcPVCStorageSize},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := client.CreateComponentPVC(context.Background(), "test", tt.name, tt.size); err != nil {
				t.Fatalf("CreateComponentPVC() error = %v", err)
			}

			pvc, err := client.kube.CoreV1().PersistentVolumeClaims("test").Get(
				context.Background(), tt.name, metav1.GetOptions{},
			)
			if err != nil {
				t.Fatalf("get PVC: %v", err)
			}
			got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
			want := resource.MustParse(tt.size)
			if got.Cmp(want) != 0 {
				t.Fatalf("storage request = %s, want %s", got.String(), want.String())
			}
		})
	}
}

func TestCreateComponentPVCRejectsInvalidStorageSize(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset()}

	if err := client.CreateComponentPVC(context.Background(), "test", "vta", "invalid"); err == nil {
		t.Fatal("CreateComponentPVC() error = nil, want invalid size error")
	}
}

func TestCreateComponentDeploymentUsesConfiguredHealthReadinessProbe(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset()}

	if err := client.CreateComponentDeployment(context.Background(), "test", ComponentDeploymentSpec{
		Name:            "mediator",
		Image:           "example/mediator:test",
		Port:            7037,
		HealthCheckPath: "/mediator/v1/readyz",
	}); err != nil {
		t.Fatalf("CreateComponentDeployment() error = %v", err)
	}

	deployment, err := client.kube.AppsV1().Deployments("test").Get(
		context.Background(), "mediator", metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	probe := deployment.Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.HTTPGet == nil {
		t.Fatal("readiness probe is not an HTTP probe")
	}
	if probe.HTTPGet.Path != "/mediator/v1/readyz" {
		t.Fatalf("readiness path = %q, want /mediator/v1/readyz", probe.HTTPGet.Path)
	}
	if probe.HTTPGet.Port.IntVal != 7037 {
		t.Fatalf("readiness port = %d, want 7037", probe.HTTPGet.Port.IntVal)
	}
}

func TestCreateSetupResourcesUsesVtaStorageSize(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset()}

	if err := client.CreateSetupResources(
		context.Background(), "test", 42, "setup = true", "example/vta:test",
	); err != nil {
		t.Fatalf("CreateSetupResources() error = %v", err)
	}

	pvc, err := client.kube.CoreV1().PersistentVolumeClaims("test").Get(
		context.Background(), VtaPVCName(42), metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get PVC: %v", err)
	}
	got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]
	want := resource.MustParse(VtaPVCStorageSize)
	if got.Cmp(want) != 0 {
		t.Fatalf("storage request = %s, want %s", got.String(), want.String())
	}
}
