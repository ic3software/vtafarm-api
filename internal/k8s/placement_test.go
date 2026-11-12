package k8s

import (
	"context"
	"encoding/json"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
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
		template := corev1.PodTemplateSpec{}
		if sessionID == "41" {
			template.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: PlacementGateName}}
		}
		objects = append(objects,
			&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
				Name: "vta-" + sessionID, Namespace: namespace,
				Labels: map[string]string{"app": "vta", "session-id": sessionID},
			}, Spec: appsv1.DeploymentSpec{Template: template}},
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
	if !placements[0].TemplateGated || placements[1].TemplateGated {
		t.Fatalf("template gate inventory = %#v, want only session 41 gated", placements)
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

func TestFinalizeVTAOneShotRelocationRestoresTemplatesWithoutReplacingReadyPod(t *testing.T) {
	ctx := context.Background()
	controller := true
	deploymentUID := types.UID("deployment-uid")
	replicaSetUID := types.UID("replicaset-uid")
	podUID := types.UID("pod-uid")
	labels := map[string]string{"app": "vta", "session-id": "42"}
	customAffinity := &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		PreferredDuringSchedulingIgnoredDuringExecution: []corev1.PreferredSchedulingTerm{{
			Weight: 1,
			Preference: corev1.NodeSelectorTerm{MatchExpressions: []corev1.NodeSelectorRequirement{{
				Key: "example.com/custom", Operator: corev1.NodeSelectorOpExists,
			}}},
		}},
	}}
	original := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			SchedulingGates: []corev1.PodSchedulingGate{{Name: "example.com/other-gate"}},
			Affinity:        customAffinity,
		},
	}
	managed := *original.DeepCopy()
	managed.Spec.SchedulingGates = []corev1.PodSchedulingGate{{Name: PlacementGateName}}
	managed.Spec.Affinity = VTAPlacementAffinity()
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: VtaDeploymentName(42), Namespace: "tenant", UID: deploymentUID, Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: managed,
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1},
	}
	replicaTemplate := *managed.DeepCopy()
	replicaTemplate.Labels = map[string]string{
		"app": "vta", "session-id": "42", appsv1.DefaultDeploymentUniqueLabelKey: "managed-hash",
	}
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "vta-42-managed-hash", Namespace: "tenant", UID: replicaSetUID, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name,
				UID: deploymentUID, Controller: &controller,
			}},
		},
		Spec: appsv1.ReplicaSetSpec{Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: replicaTemplate},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "vta-42-ready", Namespace: "tenant", UID: podUID, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: replicaSet.Name,
				UID: replicaSetUID, Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{NodeName: "worker-2"},
	}
	snapshot, err := json.Marshal(DeploymentPlacementSnapshot{Template: original})
	if err != nil {
		t.Fatal(err)
	}
	client := &Client{kube: fake.NewSimpleClientset(deployment, replicaSet, pod)}

	if err := client.FinalizeVTAOneShotRelocation(
		ctx, "tenant", 42, string(deploymentUID), pod.Name, string(podUID), snapshot,
	); err != nil {
		t.Fatal(err)
	}

	updatedDeployment, err := client.kube.AppsV1().Deployments("tenant").Get(ctx, deployment.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if updatedDeployment.Spec.Paused || hasManagedRelocationPolicy(&updatedDeployment.Spec.Template) {
		t.Fatalf("Deployment retained relocation policy: %#v", updatedDeployment.Spec)
	}
	if !apiequality.Semantic.DeepEqual(updatedDeployment.Spec.Template.Spec.Affinity, customAffinity) ||
		!hasSchedulingGateInTemplate(&updatedDeployment.Spec.Template, "example.com/other-gate") {
		t.Fatalf("Deployment did not restore the original scheduling policy: %#v", updatedDeployment.Spec.Template.Spec)
	}
	updatedReplicaSet, err := client.kube.AppsV1().ReplicaSets("tenant").Get(ctx, replicaSet.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if hasManagedRelocationPolicy(&updatedReplicaSet.Spec.Template) ||
		!hasSchedulingGateInTemplate(&updatedReplicaSet.Spec.Template, "example.com/other-gate") {
		t.Fatalf("ReplicaSet retained relocation policy: %#v", updatedReplicaSet.Spec.Template.Spec)
	}
	readyPod, err := client.kube.CoreV1().Pods("tenant").Get(ctx, pod.Name, metav1.GetOptions{})
	if err != nil || readyPod.UID != podUID {
		t.Fatalf("ready Pod was replaced during policy cleanup: pod=%#v err=%v", readyPod, err)
	}
}

func TestFinalizeVTAOneShotRelocationReplacesLegacyGatedPod(t *testing.T) {
	ctx := context.Background()
	controller := true
	deploymentUID := types.UID("deployment-uid")
	replicaSetUID := types.UID("replicaset-uid")
	labels := map[string]string{"app": "vta", "session-id": "42"}
	template := corev1.PodTemplateSpec{
		ObjectMeta: metav1.ObjectMeta{Labels: labels},
		Spec: corev1.PodSpec{
			SchedulingGates: []corev1.PodSchedulingGate{{Name: PlacementGateName}},
			Affinity:        VTAPlacementAffinity(),
		},
	}
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: VtaDeploymentName(42), Namespace: "tenant", UID: deploymentUID, Generation: 1},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels}, Template: template,
		},
		Status: appsv1.DeploymentStatus{ObservedGeneration: 1},
	}
	replicaSet := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: "vta-42-managed-hash", Namespace: "tenant", UID: replicaSetUID, Labels: labels,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name,
				UID: deploymentUID, Controller: &controller,
			}},
		},
		Spec: appsv1.ReplicaSetSpec{Template: template},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "vta-42-gated", Namespace: "tenant", UID: types.UID("pod-uid"),
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: replicaSet.Name,
				UID: replicaSetUID, Controller: &controller,
			}},
		},
		Spec: corev1.PodSpec{SchedulingGates: []corev1.PodSchedulingGate{{Name: PlacementGateName}}},
	}
	client := &Client{kube: fake.NewSimpleClientset(deployment, replicaSet, pod)}

	if err := client.FinalizeVTAOneShotRelocation(
		ctx, "tenant", 42, string(deploymentUID), pod.Name, string(pod.UID), nil,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := client.kube.CoreV1().Pods("tenant").Get(ctx, pod.Name, metav1.GetOptions{}); !k8serrors.IsNotFound(err) {
		t.Fatalf("legacy gated Pod was not deleted after template cleanup: %v", err)
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
