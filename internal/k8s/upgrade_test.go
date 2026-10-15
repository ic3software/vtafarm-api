package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRecoverDeploymentImageReplacesImageAndForcesRestart(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "vtc", Namespace: "test"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "vtc", Image: "example/vtc:bad"}}},
		}},
	}
	client := &Client{kube: fake.NewSimpleClientset(deployment)}

	if err := client.RecoverDeploymentImage(context.Background(), "test", "vtc", "example/vtc:good"); err != nil {
		t.Fatalf("RecoverDeploymentImage() error = %v", err)
	}
	got, err := client.kube.AppsV1().Deployments("test").Get(context.Background(), "vtc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	if got.Spec.Template.Spec.Containers[0].Image != "example/vtc:good" {
		t.Fatalf("image = %q", got.Spec.Template.Spec.Containers[0].Image)
	}
	if got.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("strategy = %q", got.Spec.Strategy.Type)
	}
	if got.Spec.Template.Annotations[recoveryRestartAnnotation] == "" {
		t.Fatal("recovery restart annotation is empty")
	}
}

func TestRecoverDeploymentImageAllowsMissingDeployment(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset()}
	if err := client.RecoverDeploymentImage(context.Background(), "test", "missing", "example/vtc:good"); err != nil {
		t.Fatalf("RecoverDeploymentImage() error = %v", err)
	}
}
