package k8s

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// LonghornNamespace is where Longhorn keeps its CRD instances (nodes, settings).
const LonghornNamespace = "longhorn-system"

var (
	longhornNodesGVR    = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "nodes"}
	longhornSettingsGVR = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "settings"}
	longhornVolumesGVR  = schema.GroupVersionResource{Group: "longhorn.io", Version: "v1beta2", Resource: "volumes"}
)

// NodeResourceStat is one node's CPU/memory picture: what the scheduler can
// hand out (allocatable), what pods have reserved (requested), and what is
// actually being consumed right now (used, from metrics-server).
type NodeResourceStat struct {
	Name string
	// Schedulable is false for cordoned nodes and nodes carrying any
	// NoSchedule/NoExecute taint — farm pods declare no tolerations, so such
	// nodes contribute nothing to new-session capacity.
	Schedulable          bool
	CPUAllocatableMillis int64
	CPURequestedMillis   int64
	CPUUsedMillis        int64
	MemAllocatableBytes  int64
	MemRequestedBytes    int64
	MemUsedBytes         int64
}

// StorageNodeStat is one Longhorn node's disk picture, summed across its
// schedulable disks. SchedulableBytes is the headroom Longhorn would accept
// new replicas into: (maximum − reserved) × overProvisioning% − scheduled.
type StorageNodeStat struct {
	Name string
	// Schedulable is false when the Longhorn node (or every one of its disks)
	// has allowScheduling off — it then accepts no new replicas.
	Schedulable      bool
	MaximumBytes     int64
	ReservedBytes    int64
	ScheduledBytes   int64
	AvailableBytes   int64
	SchedulableBytes int64
}

// ClusterStats aggregates everything the admin dashboard needs in one read.
// MetricsAvailable / StorageAvailable flag partial results: metrics-server or
// Longhorn being unreachable degrades the response instead of failing it.
type ClusterStats struct {
	Nodes        []NodeResourceStat
	StorageNodes []StorageNodeStat
	// StorageDataWrittenBytes is the bytes actually written across all Longhorn
	// volumes (sum of status.actualSize) — volumes are thin-provisioned, so
	// this is typically far below the scheduled total.
	StorageDataWrittenBytes int64
	// StorageReplicaCount is numberOfReplicas from the default Longhorn
	// StorageClass — each PVC of size S schedules S × replicas of disk.
	StorageReplicaCount int
	MetricsAvailable    bool
	StorageAvailable    bool
}

// ClusterResourceStats reads node capacity, pod requests, live usage
// (metrics-server), and Longhorn disk stats across every node in the cluster.
func (c *Client) ClusterResourceStats(ctx context.Context) (*ClusterStats, error) {
	nodeList, err := c.kube.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, fmt.Errorf("list nodes: %w", err)
	}

	// Non-terminal pods only: Succeeded/Failed pods release their requests.
	podList, err := c.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase!=Succeeded,status.phase!=Failed",
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	cpuReq := map[string]int64{}
	memReq := map[string]int64{}
	for i := range podList.Items {
		pod := &podList.Items[i]
		if pod.Spec.NodeName == "" {
			continue // unscheduled — reserves nothing yet
		}
		cpu, mem := podEffectiveRequests(pod)
		cpuReq[pod.Spec.NodeName] += cpu
		memReq[pod.Spec.NodeName] += mem
	}

	stats := &ClusterStats{StorageReplicaCount: 1}

	cpuUsed, memUsed, metricsErr := c.nodeUsage(ctx)
	stats.MetricsAvailable = metricsErr == nil

	for i := range nodeList.Items {
		node := &nodeList.Items[i]
		stat := NodeResourceStat{
			Name:                 node.Name,
			Schedulable:          nodeSchedulable(node),
			CPUAllocatableMillis: node.Status.Allocatable.Cpu().MilliValue(),
			MemAllocatableBytes:  node.Status.Allocatable.Memory().Value(),
			CPURequestedMillis:   cpuReq[node.Name],
			MemRequestedBytes:    memReq[node.Name],
			CPUUsedMillis:        cpuUsed[node.Name],
			MemUsedBytes:         memUsed[node.Name],
		}
		stats.Nodes = append(stats.Nodes, stat)
	}

	storageNodes, dataWritten, replicas, storageErr := c.longhornStats(ctx)
	if storageErr == nil {
		stats.StorageAvailable = true
		stats.StorageNodes = storageNodes
		stats.StorageDataWrittenBytes = dataWritten
		stats.StorageReplicaCount = replicas
	}

	return stats, nil
}

// podEffectiveRequests mirrors the scheduler's math: per resource,
// max(sum of container requests, largest init-container request).
func podEffectiveRequests(pod *corev1.Pod) (cpuMillis, memBytes int64) {
	for _, ctr := range pod.Spec.Containers {
		cpuMillis += ctr.Resources.Requests.Cpu().MilliValue()
		memBytes += ctr.Resources.Requests.Memory().Value()
	}
	for _, ctr := range pod.Spec.InitContainers {
		if v := ctr.Resources.Requests.Cpu().MilliValue(); v > cpuMillis {
			cpuMillis = v
		}
		if v := ctr.Resources.Requests.Memory().Value(); v > memBytes {
			memBytes = v
		}
	}
	return cpuMillis, memBytes
}

func nodeSchedulable(node *corev1.Node) bool {
	if node.Spec.Unschedulable {
		return false
	}
	for _, taint := range node.Spec.Taints {
		if taint.Effect == corev1.TaintEffectNoSchedule || taint.Effect == corev1.TaintEffectNoExecute {
			return false
		}
	}
	return true
}

// nodeUsage reads live per-node consumption from metrics-server. Raw REST
// (rather than the k8s.io/metrics clientset) keeps the dependency surface at
// client-go, which already ships the discovery REST client.
func (c *Client) nodeUsage(ctx context.Context) (cpuMillis, memBytes map[string]int64, err error) {
	raw, err := c.kube.Discovery().RESTClient().Get().
		AbsPath("/apis/metrics.k8s.io/v1beta1/nodes").Do(ctx).Raw()
	if err != nil {
		return nil, nil, fmt.Errorf("query metrics-server: %w", err)
	}

	var list struct {
		Items []struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
			Usage struct {
				CPU    string `json:"cpu"`
				Memory string `json:"memory"`
			} `json:"usage"`
		} `json:"items"`
	}
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, nil, fmt.Errorf("parse node metrics: %w", err)
	}

	cpuMillis = map[string]int64{}
	memBytes = map[string]int64{}
	for _, item := range list.Items {
		if q, err := resource.ParseQuantity(item.Usage.CPU); err == nil {
			cpuMillis[item.Metadata.Name] = q.MilliValue()
		}
		if q, err := resource.ParseQuantity(item.Usage.Memory); err == nil {
			memBytes[item.Metadata.Name] = q.Value()
		}
	}
	return cpuMillis, memBytes, nil
}

// longhornStats reads per-node disk capacity from the Longhorn CRDs, the
// bytes actually written across all volumes, and the replica count new PVCs
// will be created with.
func (c *Client) longhornStats(ctx context.Context) ([]StorageNodeStat, int64, int, error) {
	nodes, err := c.dyn.Resource(longhornNodesGVR).Namespace(LonghornNamespace).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, 0, 0, fmt.Errorf("list longhorn nodes: %w", err)
	}

	overProvisioning := c.longhornOverProvisioningPercent(ctx)

	var out []StorageNodeStat
	for _, item := range nodes.Items {
		stat := StorageNodeStat{Name: item.GetName()}
		nodeAllows, _, _ := unstructured.NestedBool(item.Object, "spec", "allowScheduling")
		specDisks, _, _ := unstructured.NestedMap(item.Object, "spec", "disks")
		statusDisks, _, _ := unstructured.NestedMap(item.Object, "status", "diskStatus")

		anySchedulableDisk := false
		for diskName, rawStatus := range statusDisks {
			status, ok := rawStatus.(map[string]any)
			if !ok {
				continue
			}
			var reserved int64
			allowScheduling := false
			if spec, ok := specDisks[diskName].(map[string]any); ok {
				reserved = toInt64(spec["storageReserved"])
				allowScheduling, _ = spec["allowScheduling"].(bool)
			}
			maximum := toInt64(status["storageMaximum"])
			scheduled := toInt64(status["storageScheduled"])
			available := toInt64(status["storageAvailable"])

			stat.MaximumBytes += maximum
			stat.ReservedBytes += reserved
			stat.ScheduledBytes += scheduled
			stat.AvailableBytes += available
			if allowScheduling {
				anySchedulableDisk = true
				headroom := (maximum-reserved)*overProvisioning/100 - scheduled
				if headroom > 0 {
					stat.SchedulableBytes += headroom
				}
			}
		}
		stat.Schedulable = nodeAllows && anySchedulableDisk
		out = append(out, stat)
	}

	return out, c.longhornDataWritten(ctx), c.longhornReplicaCount(ctx), nil
}

// longhornDataWritten sums status.actualSize across all Longhorn volumes —
// the data really on disk, as opposed to the thin-provisioned scheduled
// sizes. Best-effort: 0 if the volumes can't be listed.
func (c *Client) longhornDataWritten(ctx context.Context) int64 {
	vols, err := c.dyn.Resource(longhornVolumesGVR).Namespace(LonghornNamespace).
		List(ctx, metav1.ListOptions{})
	if err != nil {
		return 0
	}
	var total int64
	for _, v := range vols.Items {
		size, _, _ := unstructured.NestedFieldNoCopy(v.Object, "status", "actualSize")
		total += toInt64(size)
	}
	return total
}

// longhornOverProvisioningPercent reads the storage-over-provisioning-percentage
// setting; 100 (no over-provisioning) on any failure.
func (c *Client) longhornOverProvisioningPercent(ctx context.Context) int64 {
	setting, err := c.dyn.Resource(longhornSettingsGVR).Namespace(LonghornNamespace).
		Get(ctx, "storage-over-provisioning-percentage", metav1.GetOptions{})
	if err != nil {
		return 100
	}
	value, _, _ := unstructured.NestedString(setting.Object, "value")
	if v := parseLonghornIntSetting(value); v > 0 {
		return v
	}
	return 100
}

// parseLonghornIntSetting handles both value encodings Longhorn uses: a plain
// integer ("100") and the newer per-data-engine JSON ({"v1":"100","v2":"100"}).
func parseLonghornIntSetting(value string) int64 {
	if v, err := strconv.ParseInt(value, 10, 64); err == nil {
		return v
	}
	var perEngine map[string]string
	if err := json.Unmarshal([]byte(value), &perEngine); err == nil {
		if v, err := strconv.ParseInt(perEngine["v1"], 10, 64); err == nil {
			return v
		}
	}
	return 0
}

// longhornReplicaCount returns numberOfReplicas from the default StorageClass
// (the farm's PVCs don't name a class, so the default is what they get);
// 1 on any failure.
func (c *Client) longhornReplicaCount(ctx context.Context) int {
	classes, err := c.kube.StorageV1().StorageClasses().List(ctx, metav1.ListOptions{})
	if err != nil {
		return 1
	}
	for i := range classes.Items {
		sc := &classes.Items[i]
		if sc.Annotations["storageclass.kubernetes.io/is-default-class"] != "true" {
			continue
		}
		if v, err := strconv.Atoi(sc.Parameters["numberOfReplicas"]); err == nil && v > 0 {
			return v
		}
		return 1
	}
	return 1
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case float64:
		return int64(n)
	case string:
		if parsed, err := strconv.ParseInt(n, 10, 64); err == nil {
			return parsed
		}
	}
	return 0
}
