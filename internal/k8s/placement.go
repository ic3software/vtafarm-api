package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

type StorageIdentity struct {
	PVCName            string `json:"pvc_name"`
	PVCUID             string `json:"pvc_uid"`
	PVName             string `json:"pv_name"`
	PVUID              string `json:"pv_uid"`
	CSIVolumeHandle    string `json:"csi_volume_handle"`
	LonghornVolume     string `json:"longhorn_volume"`
	LonghornState      string `json:"longhorn_state"`
	LonghornRobustness string `json:"longhorn_robustness"`
	LastBackupName     string `json:"last_backup_name,omitempty"`
	LastBackupAt       string `json:"last_backup_at,omitempty"`
}

type VTAPlacement struct {
	SessionID      uint            `json:"session_id"`
	Namespace      string          `json:"namespace"`
	DeploymentName string          `json:"deployment_name"`
	DeploymentUID  string          `json:"deployment_uid"`
	PodName        string          `json:"pod_name,omitempty"`
	PodUID         string          `json:"pod_uid,omitempty"`
	NodeName       string          `json:"node_name,omitempty"`
	Ready          bool            `json:"ready"`
	Gated          bool            `json:"gated"`
	MemoryRequest  int64           `json:"memory_request_bytes"`
	Storage        StorageIdentity `json:"storage"`
}

type NodePlacement struct {
	Name             string `json:"name"`
	Schedulable      bool   `json:"schedulable"`
	MemoryState      string `json:"memory_state"`
	Eligible         bool   `json:"eligible"`
	MemAllocatable   int64  `json:"memory_allocatable_bytes"`
	MemRequested     int64  `json:"memory_requested_bytes"`
	MemUsed          int64  `json:"memory_used_bytes"`
	MemReserved      int64  `json:"memory_reserved_bytes"`
	VTAOnlyPods      int    `json:"vta_only_pods"`
	MetricsAvailable bool   `json:"metrics_available"`
}

type DeploymentPlacementSnapshot struct {
	Labels   map[string]string         `json:"labels"`
	Replicas int32                     `json:"replicas"`
	Strategy appsv1.DeploymentStrategy `json:"strategy"`
	Template corev1.PodTemplateSpec    `json:"template"`
}

func (c *Client) ListVTAPlacements(ctx context.Context) ([]VTAPlacement, error) {
	deployments, err := c.kube.AppsV1().Deployments("").List(ctx, metav1.ListOptions{LabelSelector: "app=vta"})
	if err != nil {
		return nil, fmt.Errorf("list VTA deployments: %w", err)
	}
	pods, err := c.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{LabelSelector: "app=vta"})
	if err != nil {
		return nil, fmt.Errorf("list VTA pods: %w", err)
	}
	podsBySession := make(map[string]*corev1.Pod, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		sessionID := pod.Labels["session-id"]
		if sessionID == "" {
			continue
		}
		key := placementSessionKey(pod.Namespace, sessionID)
		if _, exists := podsBySession[key]; !exists {
			podsBySession[key] = pod
		}
	}
	placements := make([]VTAPlacement, 0, len(deployments.Items))
	for i := range deployments.Items {
		deployment := &deployments.Items[i]
		if !strings.HasPrefix(deployment.Namespace, c.namespacePrefix+"-") {
			continue
		}
		sessionID64, err := strconv.ParseUint(deployment.Labels["session-id"], 10, 64)
		if err != nil || sessionID64 == 0 {
			continue
		}
		placement := VTAPlacement{
			SessionID: uint(sessionID64), Namespace: deployment.Namespace,
			DeploymentName: deployment.Name, DeploymentUID: string(deployment.UID),
		}
		if len(deployment.Spec.Template.Spec.Containers) > 0 {
			placement.MemoryRequest = deployment.Spec.Template.Spec.Containers[0].Resources.Requests.Memory().Value()
		}
		if pod := podsBySession[placementSessionKey(deployment.Namespace, deployment.Labels["session-id"])]; pod != nil {
			placement.PodName = pod.Name
			placement.PodUID = string(pod.UID)
			placement.NodeName = pod.Spec.NodeName
			placement.Gated = len(pod.Spec.SchedulingGates) > 0
			placement.Ready = podReady(pod)
		}
		placements = append(placements, placement)
	}
	return placements, nil
}

// ListVTAPlacementsWithStorage enriches the lightweight workload inventory for
// the admin overview. Reconciliation does not need this cluster-wide storage
// data and must use ListVTAPlacements instead.
func (c *Client) ListVTAPlacementsWithStorage(ctx context.Context) ([]VTAPlacement, error) {
	placements, err := c.ListVTAPlacements(ctx)
	if err != nil {
		return nil, err
	}
	storageByPVC := c.listPlacementStorage(ctx)
	for i := range placements {
		placement := &placements[i]
		if storage, ok := storageByPVC[placementPVCKey(placement.Namespace, VtaPVCName(placement.SessionID))]; ok {
			placement.Storage = storage
		}
	}
	return placements, nil
}

func placementSessionKey(namespace, sessionID string) string {
	return namespace + "/" + sessionID
}

func placementPVCKey(namespace, name string) string {
	return namespace + "/" + name
}

// listPlacementStorage builds one best-effort cluster inventory. Storage data
// is supplementary in the overview, so a missing CRD or temporary Longhorn
// failure must not hide otherwise healthy workloads.
func (c *Client) listPlacementStorage(ctx context.Context) map[string]StorageIdentity {
	result := map[string]StorageIdentity{}
	pvcs, err := c.kube.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	if err != nil {
		return result
	}
	pvs, err := c.kube.CoreV1().PersistentVolumes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return result
	}
	pvByName := make(map[string]*corev1.PersistentVolume, len(pvs.Items))
	for i := range pvs.Items {
		pvByName[pvs.Items[i].Name] = &pvs.Items[i]
	}
	if c.dyn == nil {
		return result
	}
	volumes, err := c.dyn.Resource(longhornVolumesGVR).Namespace(LonghornNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return result
	}
	volumeStatus := make(map[string][2]string, len(volumes.Items))
	for i := range volumes.Items {
		volume := &volumes.Items[i]
		state, _, _ := unstructuredString(volume.Object, "status", "state")
		robustness, _, _ := unstructuredString(volume.Object, "status", "robustness")
		volumeStatus[volume.GetName()] = [2]string{state, robustness}
	}

	latestBackup := c.listLatestBackupSummaries(ctx, metav1.ListOptions{})

	for i := range pvcs.Items {
		pvc := &pvcs.Items[i]
		pv := pvByName[pvc.Spec.VolumeName]
		if pv == nil || pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
			continue
		}
		status, exists := volumeStatus[pv.Spec.CSI.VolumeHandle]
		if !exists {
			continue
		}
		backup := latestBackup[pv.Spec.CSI.VolumeHandle]
		result[placementPVCKey(pvc.Namespace, pvc.Name)] = StorageIdentity{
			PVCName: pvc.Name, PVCUID: string(pvc.UID),
			PVName: pv.Name, PVUID: string(pv.UID),
			CSIVolumeHandle: pv.Spec.CSI.VolumeHandle, LonghornVolume: pv.Spec.CSI.VolumeHandle,
			LonghornState: status[0], LonghornRobustness: status[1],
			LastBackupName: backup[0], LastBackupAt: backup[1],
		}
	}
	return result
}

func podReady(pod *corev1.Pod) bool {
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func (c *Client) InspectVTAStorage(ctx context.Context, namespace string, sessionID uint) (StorageIdentity, error) {
	identity := StorageIdentity{PVCName: VtaPVCName(sessionID)}
	pvc, err := c.kube.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, identity.PVCName, metav1.GetOptions{})
	if err != nil {
		return identity, fmt.Errorf("get PVC %s: %w", identity.PVCName, err)
	}
	identity.PVCUID = string(pvc.UID)
	identity.PVName = pvc.Spec.VolumeName
	if identity.PVName == "" {
		return identity, fmt.Errorf("PVC %s is not bound", identity.PVCName)
	}
	pv, err := c.kube.CoreV1().PersistentVolumes().Get(ctx, identity.PVName, metav1.GetOptions{})
	if err != nil {
		return identity, fmt.Errorf("get PV %s: %w", identity.PVName, err)
	}
	identity.PVUID = string(pv.UID)
	if pv.Spec.CSI == nil || pv.Spec.CSI.VolumeHandle == "" {
		return identity, fmt.Errorf("PV %s has no CSI volume handle", identity.PVName)
	}
	identity.CSIVolumeHandle = pv.Spec.CSI.VolumeHandle
	identity.LonghornVolume = pv.Spec.CSI.VolumeHandle
	volume, err := c.dyn.Resource(longhornVolumesGVR).Namespace(LonghornNamespace).Get(ctx, identity.LonghornVolume, metav1.GetOptions{})
	if err != nil {
		return identity, fmt.Errorf("get Longhorn volume %s: %w", identity.LonghornVolume, err)
	}
	identity.LonghornState, _, _ = unstructuredString(volume.Object, "status", "state")
	identity.LonghornRobustness, _, _ = unstructuredString(volume.Object, "status", "robustness")
	c.addLatestLonghornBackup(ctx, &identity)
	return identity, nil
}

func (c *Client) addLatestLonghornBackup(ctx context.Context, identity *StorageIdentity) {
	if c.dyn == nil {
		return
	}
	summaries := c.listLatestBackupSummaries(ctx, metav1.ListOptions{
		LabelSelector: "backup-volume=" + identity.LonghornVolume,
	})
	if backup := summaries[identity.LonghornVolume]; backup[1] > identity.LastBackupAt {
		identity.LastBackupName = backup[0]
		identity.LastBackupAt = backup[1]
	}
}

// listLatestBackupSummaries reads one compact BackupVolume status per volume
// instead of materializing the complete, ever-growing Backup history.
func (c *Client) listLatestBackupSummaries(ctx context.Context, options metav1.ListOptions) map[string][2]string {
	result := map[string][2]string{}
	if c.dyn == nil {
		return result
	}
	volumes, err := c.dyn.Resource(longhornBackupVolumesGVR).Namespace(LonghornNamespace).List(ctx, options)
	if err != nil {
		return result
	}
	for i := range volumes.Items {
		volume := &volumes.Items[i]
		volumeName, _, _ := unstructuredString(volume.Object, "spec", "volumeName")
		backupName, _, _ := unstructuredString(volume.Object, "status", "lastBackupName")
		backupAt, _, _ := unstructuredString(volume.Object, "status", "lastBackupAt")
		if volumeName == "" || backupName == "" || backupAt == "" {
			continue
		}
		if previous := result[volumeName]; backupAt > previous[1] {
			result[volumeName] = [2]string{backupName, backupAt}
		}
	}
	return result
}

func unstructuredString(object map[string]any, fields ...string) (string, bool, error) {
	current := any(object)
	for _, field := range fields {
		mapped, ok := current.(map[string]any)
		if !ok {
			return "", false, nil
		}
		current, ok = mapped[field]
		if !ok {
			return "", false, nil
		}
	}
	value, ok := current.(string)
	return value, ok, nil
}

func (c *Client) PlacementNodesFromPlacements(ctx context.Context, reservations map[string]int64, placements []VTAPlacement) ([]NodePlacement, error) {
	stats, metricsAvailable, err := c.nodeResourceStats(ctx)
	if err != nil {
		return nil, err
	}
	counts := map[string]int{}
	for _, placement := range placements {
		if placement.NodeName != "" {
			counts[placement.NodeName]++
		}
	}
	nodes, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}
	labels := map[string]map[string]string{}
	for i := range nodes.Items {
		labels[nodes.Items[i].Name] = nodes.Items[i].Labels
	}
	result := make([]NodePlacement, 0, len(stats))
	for _, node := range stats {
		state := labels[node.Name][PlacementLabelMemoryState]
		metricsFresh := metricsAvailable && !node.MetricsObservedAt.IsZero() &&
			time.Since(node.MetricsObservedAt) <= c.placementMetricsMaxAge
		result = append(result, NodePlacement{
			Name: node.Name, Schedulable: node.Schedulable,
			MemoryState: state, Eligible: labels[node.Name][PlacementLabelVTAEligible] == "true",
			MemAllocatable: node.MemAllocatableBytes, MemRequested: node.MemRequestedBytes,
			MemUsed: node.MemUsedBytes, MemReserved: reservations[node.Name],
			VTAOnlyPods: counts[node.Name], MetricsAvailable: metricsFresh,
		})
	}
	return result, nil
}

func (c *Client) UpdateNodePlacementLabels(ctx context.Context, nodeName, memoryState string, eligible bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		node, err := c.kube.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if node.Labels == nil {
			node.Labels = map[string]string{}
		}
		node.Labels[PlacementLabelMemoryState] = memoryState
		node.Labels[PlacementLabelVTAEligible] = strconv.FormatBool(eligible)
		_, err = c.kube.CoreV1().Nodes().Update(ctx, node, metav1.UpdateOptions{})
		return err
	})
}

func (c *Client) SnapshotVTADeployment(ctx context.Context, namespace string, sessionID uint) ([]byte, int32, string, error) {
	name := VtaDeploymentName(sessionID)
	deployment, err := c.kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, 0, "", fmt.Errorf("get VTA deployment %s: %w", name, err)
	}
	replicas := int32(1)
	if deployment.Spec.Replicas != nil {
		replicas = *deployment.Spec.Replicas
	}
	snapshot, err := json.Marshal(DeploymentPlacementSnapshot{
		Labels: deployment.Labels, Replicas: replicas,
		Strategy: deployment.Spec.Strategy, Template: deployment.Spec.Template,
	})
	if err != nil {
		return nil, 0, "", fmt.Errorf("marshal deployment snapshot: %w", err)
	}
	return snapshot, replicas, string(deployment.UID), nil
}

func (c *Client) ReconcileVTADeployment(ctx context.Context, namespace string, sessionID uint) error {
	name := VtaDeploymentName(sessionID)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment, err := c.kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		c.applyVTARelocationPolicy(deployment, sessionID)
		_, err = c.kube.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("reconcile VTA deployment %s: %w", name, err)
	}
	return nil
}

func (c *Client) RestoreVTADeploymentSnapshot(ctx context.Context, namespace string, sessionID uint, deploymentUID string, raw []byte) error {
	var snapshot DeploymentPlacementSnapshot
	if len(raw) == 0 || json.Unmarshal(raw, &snapshot) != nil {
		return errors.New("saved Deployment snapshot is invalid")
	}
	name := VtaDeploymentName(sessionID)
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment, err := c.kube.AppsV1().Deployments(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if deploymentUID != "" && string(deployment.UID) != deploymentUID {
			return errors.New("Deployment identity changed; refusing snapshot rollback")
		}
		deployment.Labels = snapshot.Labels
		deployment.Spec.Strategy = snapshot.Strategy
		deployment.Spec.Template = snapshot.Template
		deployment.Spec.Replicas = &snapshot.Replicas
		_, err = c.kube.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return fmt.Errorf("restore VTA deployment %s: %w", name, err)
	}
	return nil
}

func (c *Client) ScaleVTADeployment(ctx context.Context, namespace string, sessionID uint, replicas int32) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deployment, err := c.kube.AppsV1().Deployments(namespace).Get(ctx, VtaDeploymentName(sessionID), metav1.GetOptions{})
		if err != nil {
			return err
		}
		deployment.Spec.Replicas = &replicas
		_, err = c.kube.AppsV1().Deployments(namespace).Update(ctx, deployment, metav1.UpdateOptions{})
		return err
	})
}

func (c *Client) WaitVTAStorageDetached(ctx context.Context, namespace string, sessionID uint, timeout time.Duration) (StorageIdentity, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		identity, err := c.InspectVTAStorage(ctx, namespace, sessionID)
		if err == nil {
			attachments, listErr := c.kube.StorageV1().VolumeAttachments().List(ctx, metav1.ListOptions{})
			if listErr == nil && !hasPVAttachment(attachments.Items, identity.PVName) && identity.LonghornState == "detached" && identity.LonghornRobustness != "faulted" {
				return identity, nil
			}
		}
		select {
		case <-ctx.Done():
			return StorageIdentity{}, ctx.Err()
		case <-deadline.C:
			return StorageIdentity{}, fmt.Errorf("timeout waiting for VTA %d volume to detach", sessionID)
		case <-ticker.C:
		}
	}
}

func hasPVAttachment(attachments []storagev1.VolumeAttachment, pvName string) bool {
	for _, attachment := range attachments {
		if attachment.Spec.Source.PersistentVolumeName != nil && *attachment.Spec.Source.PersistentVolumeName == pvName {
			return true
		}
	}
	return false
}

func (c *Client) WaitForGatedVTAPod(ctx context.Context, namespace string, sessionID uint, timeout time.Duration) (*corev1.Pod, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	selector := fmt.Sprintf("app=vta,session-id=%d", sessionID)
	for {
		pods, err := c.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err == nil {
			for i := range pods.Items {
				if pods.Items[i].DeletionTimestamp == nil && hasSchedulingGate(&pods.Items[i], PlacementGateName) {
					return &pods.Items[i], nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("timeout waiting for gated VTA %d pod", sessionID)
		case <-ticker.C:
		}
	}
}

func hasSchedulingGate(pod *corev1.Pod, name string) bool {
	for _, gate := range pod.Spec.SchedulingGates {
		if gate.Name == name {
			return true
		}
	}
	return false
}

func (c *Client) AssignGatedPod(ctx context.Context, namespace, podName, targetNode string) (*corev1.Pod, error) {
	var updated *corev1.Pod
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		pod, err := c.kube.CoreV1().Pods(namespace).Get(ctx, podName, metav1.GetOptions{})
		if err != nil {
			return err
		}
		if pod.Spec.NodeName != "" {
			if pod.Spec.NodeName != targetNode {
				return fmt.Errorf("pod already bound to %s", pod.Spec.NodeName)
			}
			updated = pod
			return nil
		}
		if !hasSchedulingGate(pod, PlacementGateName) {
			return fmt.Errorf("pod %s is not placement-gated", podName)
		}
		if pod.Spec.NodeSelector == nil {
			pod.Spec.NodeSelector = map[string]string{}
		}
		pod.Spec.NodeSelector[corev1.LabelHostname] = targetNode
		gates := pod.Spec.SchedulingGates[:0]
		for _, gate := range pod.Spec.SchedulingGates {
			if gate.Name != PlacementGateName {
				gates = append(gates, gate)
			}
		}
		pod.Spec.SchedulingGates = gates
		updated, err = c.kube.CoreV1().Pods(namespace).Update(ctx, pod, metav1.UpdateOptions{})
		return err
	})
	if err != nil {
		return nil, fmt.Errorf("assign gated pod %s to %s: %w", podName, targetNode, err)
	}
	return updated, nil
}

func (c *Client) WaitVTAReadyOnNode(ctx context.Context, namespace string, sessionID uint, nodeName string, timeout time.Duration) (*corev1.Pod, error) {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	selector := fmt.Sprintf("app=vta,session-id=%d", sessionID)
	for {
		pods, err := c.kube.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err == nil {
			for i := range pods.Items {
				pod := &pods.Items[i]
				if pod.DeletionTimestamp == nil && pod.Spec.NodeName == nodeName && podReady(pod) {
					return pod, nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-deadline.C:
			return nil, fmt.Errorf("timeout waiting for VTA %d on node %s", sessionID, nodeName)
		case <-ticker.C:
		}
	}
}
