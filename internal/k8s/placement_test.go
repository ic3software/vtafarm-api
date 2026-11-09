package k8s

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ic3software/vtafarm-api/internal/config"
)

func TestListVTAPlacementsUsesOneLightweightClusterInventory(t *testing.T) {
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
		if action.GetResource().Resource == "persistentvolumeclaims" || action.GetResource().Resource == "persistentvolumes" {
			t.Fatalf("lightweight placement inventory read storage resource %s", action.GetResource().Resource)
		}
	}
	if podLists != 1 {
		t.Fatalf("listed pods %d times, want one cluster inventory read", podLists)
	}
}

func TestListVTAPlacementsWithStorageUsesBackupVolumeSummaries(t *testing.T) {
	ctx := context.Background()
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name: "vta-42", Namespace: "vtafarm-user-42",
		Labels: map[string]string{"app": "vta", "session-id": "42"},
	}}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: VtaPVCName(42), Namespace: deployment.Namespace, UID: types.UID("pvc-uid")},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-42"},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-42", UID: types.UID("pv-uid")},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: "volume-42"},
		}},
	}
	volume := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "longhorn.io/v1beta2", "kind": "Volume",
		"metadata": map[string]any{"name": "volume-42", "namespace": LonghornNamespace},
		"status":   map[string]any{"state": "attached", "robustness": "healthy"},
	}}
	backupVolume := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "longhorn.io/v1beta2", "kind": "BackupVolume",
		"metadata": map[string]any{"name": "volume-42-default", "namespace": LonghornNamespace},
		"spec":     map[string]any{"volumeName": "volume-42"},
		"status": map[string]any{
			"lastBackupName": "backup-latest",
			"lastBackupAt":   "2026-11-09T00:19:39Z",
		},
	}}
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), volume, backupVolume)
	client := &Client{
		kube:            fake.NewSimpleClientset(deployment, pvc, pv),
		dyn:             dyn,
		namespacePrefix: "vtafarm-user",
	}

	placements, err := client.ListVTAPlacementsWithStorage(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(placements) != 1 {
		t.Fatalf("got %d placements, want 1", len(placements))
	}
	storage := placements[0].Storage
	if storage.LonghornVolume != "volume-42" || storage.LastBackupName != "backup-latest" || storage.LastBackupAt != "2026-11-09T00:19:39Z" {
		t.Fatalf("unexpected storage summary: %#v", storage)
	}
	for _, action := range dyn.Actions() {
		if action.GetResource().Resource == "backups" {
			t.Fatal("storage enrichment listed complete Longhorn backup history")
		}
	}
}

func TestInspectVTAStorageFiltersBackupVolumeSummary(t *testing.T) {
	ctx := context.Background()
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: VtaPVCName(42), Namespace: "tenant", UID: types.UID("pvc-uid")},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: "pv-42"},
	}
	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pv-42", UID: types.UID("pv-uid")},
		Spec: corev1.PersistentVolumeSpec{PersistentVolumeSource: corev1.PersistentVolumeSource{
			CSI: &corev1.CSIPersistentVolumeSource{VolumeHandle: "volume-42"},
		}},
	}
	volume := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "longhorn.io/v1beta2", "kind": "Volume",
		"metadata": map[string]any{"name": "volume-42", "namespace": LonghornNamespace},
		"status":   map[string]any{"state": "attached", "robustness": "healthy"},
	}}
	backupVolume := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "longhorn.io/v1beta2", "kind": "BackupVolume",
		"metadata": map[string]any{
			"name": "volume-42-default", "namespace": LonghornNamespace,
			"labels": map[string]any{"backup-volume": "volume-42"},
		},
		"spec": map[string]any{"volumeName": "volume-42"},
		"status": map[string]any{
			"lastBackupName": "backup-latest",
			"lastBackupAt":   "2026-11-09T00:19:39Z",
		},
	}}
	dyn := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme(), volume, backupVolume)
	client := &Client{kube: fake.NewSimpleClientset(pvc, pv), dyn: dyn}

	storage, err := client.InspectVTAStorage(ctx, "tenant", 42)
	if err != nil {
		t.Fatal(err)
	}
	if storage.LastBackupName != "backup-latest" || storage.LastBackupAt != "2026-11-09T00:19:39Z" {
		t.Fatalf("unexpected backup summary: %#v", storage)
	}
	for _, action := range dyn.Actions() {
		if action.GetVerb() != "list" || action.GetResource().Resource != "backupvolumes" {
			continue
		}
		selector := action.(k8stesting.ListAction).GetListRestrictions().Labels.String()
		if selector != "backup-volume=volume-42" {
			t.Fatalf("backup volume selector = %q, want backup-volume=volume-42", selector)
		}
		return
	}
	t.Fatal("InspectVTAStorage did not list a filtered backup volume summary")
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

func TestReconcileVTADeploymentAddsRelocationGate(t *testing.T) {
	ctx := context.Background()
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: VtaDeploymentName(42), Namespace: "tenant"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{"app": "vta", "session-id": "42"}},
		}},
	}
	client := &Client{kube: fake.NewSimpleClientset(deployment), placementMode: config.PlacementEnabled}

	if err := client.ReconcileVTADeployment(ctx, "tenant", 42); err != nil {
		t.Fatal(err)
	}
	updated, err := client.kube.AppsV1().Deployments("tenant").Get(ctx, deployment.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Spec.Template.Spec.SchedulingGates) != 1 ||
		updated.Spec.Template.Spec.SchedulingGates[0].Name != PlacementGateName {
		t.Fatalf("scheduling gates = %#v, want relocation gate", updated.Spec.Template.Spec.SchedulingGates)
	}
	if updated.Spec.Template.Spec.Affinity == nil {
		t.Fatal("relocation Deployment is missing placement affinity")
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
