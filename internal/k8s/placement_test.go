package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
)

func TestListVTAPlacementsUsesOneClusterPodList(t *testing.T) {
	ctx := context.Background()
	objects := []runtime.Object{}
	for _, sessionID := range []string{"41", "42"} {
		namespace := "vtafarm-user-" + sessionID
		objects = append(objects,
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
				Name: "vta-" + sessionID, Namespace: namespace,
				Labels: map[string]string{"app": "vta", "session-id": sessionID},
			}},
			&corev1.Pod{ObjectMeta: metav1.ObjectMeta{
				Name: "vta-" + sessionID, Namespace: namespace, UID: types.UID("pod-" + sessionID),
				Labels: map[string]string{"app": "vta", "session-id": sessionID},
			}, Spec: corev1.PodSpec{NodeName: "worker-1"}},
		)
	}
	kube := fake.NewSimpleClientset(objects...)
	client := &Client{kube: kube, namespacePrefix: "vtafarm-user"}

	placements, err := client.ListVTAPlacements(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(placements) != 2 {
		t.Fatalf("got %d placements, want 2", len(placements))
	}
	podLists := 0
	for _, action := range kube.Actions() {
		if action.GetVerb() == "list" && action.GetResource().Resource == "pods" {
			podLists++
		}
	}
	if podLists != 1 {
		t.Fatalf("listed pods %d times, want one cluster inventory read", podLists)
	}
}

func TestRestoreVTADeploymentSnapshotPreservesDeploymentIdentity(t *testing.T) {
	ctx := context.Background()
	replicas := int32(1)
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: VtaDeploymentName(42), Namespace: "tenant", UID: types.UID("deployment-uid"), Labels: map[string]string{"before": "true"}},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Strategy: appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType},
			Template: corev1.PodTemplateSpec{ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "vta", "session-id": "42"}}},
		},
	}
	client := &Client{kube: fake.NewSimpleClientset(deployment)}
	snapshot, _, uid, err := client.SnapshotVTADeployment(ctx, "tenant", 42)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := client.kube.AppsV1().Deployments("tenant").Get(ctx, deployment.Name, metav1.GetOptions{})
	current.Labels = map[string]string{"changed": "true"}
	current.Spec.Template.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: PlacementGateName}}
	if _, err := client.kube.AppsV1().Deployments("tenant").Update(ctx, current, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	if err := client.RestoreVTADeploymentSnapshot(ctx, "tenant", 42, uid, snapshot); err != nil {
		t.Fatal(err)
	}
	restored, _ := client.kube.AppsV1().Deployments("tenant").Get(ctx, deployment.Name, metav1.GetOptions{})
	if restored.Labels["before"] != "true" || len(restored.Spec.Template.Spec.SchedulingGates) != 0 {
		t.Fatalf("snapshot was not restored: %#v", restored)
	}
}

func TestRestoreVTADeploymentSnapshotRefusesReplacement(t *testing.T) {
	ctx := context.Background()
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: VtaDeploymentName(42), Namespace: "tenant", UID: types.UID("replacement")}}
	client := &Client{kube: fake.NewSimpleClientset(deployment)}
	if err := client.RestoreVTADeploymentSnapshot(ctx, "tenant", 42, "original", []byte(`{"replicas":1,"template":{"metadata":{},"spec":{"containers":null}},"strategy":{}}`)); err == nil {
		t.Fatal("rollback accepted a replacement Deployment UID")
	}
}
