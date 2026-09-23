package k8s

import (
	"context"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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

func TestWaitForComponentDeploymentReadyOrRestarts(t *testing.T) {
	for _, test := range []struct {
		name          string
		restarts      int32
		podReady      bool
		readyReplicas int32
		want          string
	}{
		{name: "ready", podReady: true, readyReplicas: 1},
		{name: "three crashes", restarts: 3, want: "restarted 3 times"},
		{name: "two crashes still waits", restarts: 2, want: "timeout"},
		{name: "stale deployment readiness", restarts: 2, readyReplicas: 1, want: "timeout"},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{kube: fake.NewSimpleClientset(
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: "vta-42", Namespace: "fpp-user-1", Generation: 2},
					Status:     appsv1.DeploymentStatus{ObservedGeneration: 2, ReadyReplicas: test.readyReplicas},
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "vta-42-pod", Namespace: "fpp-user-1", Labels: map[string]string{"app": "vta", "session-id": "42"}},
					Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "vta"}}},
					Status:     corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "vta", RestartCount: test.restarts, Ready: test.podReady}}},
				},
			)}
			err := client.WaitForComponentDeploymentReadyOrRestarts(context.Background(), "fpp-user-1", "vta-42", "app=vta,session-id=42", 20*time.Millisecond, 3)
			if test.want == "" {
				if err != nil {
					t.Fatalf("ready deployment returned error: %v", err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}
