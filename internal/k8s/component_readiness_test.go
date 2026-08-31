package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestComponentDeploymentReady(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset(&appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "vta-42", Namespace: "fpp-user-1"},
		Status:     appsv1.DeploymentStatus{ReadyReplicas: 1},
	})}

	ready, err := client.ComponentDeploymentReady(context.Background(), "fpp-user-1", "vta-42")
	if err != nil {
		t.Fatal(err)
	}
	if !ready {
		t.Fatal("expected deployment to be ready")
	}

	ready, err = client.ComponentDeploymentReady(context.Background(), "fpp-user-1", "missing")
	if err == nil || ready {
		t.Fatalf("missing deployment = ready %v, err %v", ready, err)
	}
}
