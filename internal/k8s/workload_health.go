package k8s

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Ping verifies the API server is reachable — the cheapest possible call.
func (c *Client) Ping(ctx context.Context) error {
	return c.kube.Discovery().RESTClient().Get().AbsPath("/version").Do(ctx).Error()
}

// PodAlert is one unhealthy pod as reported by GET /api/v1/monitor/workloads.
type PodAlert struct {
	Namespace string `json:"namespace"`
	Pod       string `json:"pod"`
	Reason    string `json:"reason"`
}

// WorkloadAlertConfig tunes what counts as unhealthy; see config.MonitorConfig.
type WorkloadAlertConfig struct {
	RestartWindow   time.Duration
	PendingGrace    time.Duration
	ExtraNamespaces []string
}

// WorkloadAlerts scans the user namespaces ({prefix}-*) plus the configured
// infra namespaces and reports pods that look unhealthy: crash/image-pull
// backoffs, recent restarts, and pods stuck Pending or not-Ready past the
// grace period. Job-owned pods are skipped — a failed setup Job is a
// session-level concern, not a platform alarm.
func (c *Client) WorkloadAlerts(ctx context.Context, cfg WorkloadAlertConfig) ([]PodAlert, error) {
	podList, err := c.kube.CoreV1().Pods("").List(ctx, metav1.ListOptions{
		FieldSelector: "status.phase!=Succeeded",
	})
	if err != nil {
		return nil, fmt.Errorf("list pods: %w", err)
	}

	watched := func(ns string) bool {
		return strings.HasPrefix(ns, c.namespacePrefix+"-") ||
			slices.Contains(cfg.ExtraNamespaces, ns)
	}

	now := time.Now()
	var alerts []PodAlert
	for i := range podList.Items {
		pod := &podList.Items[i]
		if !watched(pod.Namespace) {
			continue
		}
		if reasons := classifyPod(pod, now, cfg.RestartWindow, cfg.PendingGrace); len(reasons) > 0 {
			alerts = append(alerts, PodAlert{
				Namespace: pod.Namespace,
				Pod:       pod.Name,
				Reason:    strings.Join(reasons, "; "),
			})
		}
	}
	return alerts, nil
}

// alertWaitingReasons are the container waiting states that always mean
// something is broken (as opposed to transient ones like ContainerCreating).
var alertWaitingReasons = map[string]bool{
	"CrashLoopBackOff":           true,
	"ImagePullBackOff":           true,
	"ErrImagePull":               true,
	"InvalidImageName":           true,
	"CreateContainerConfigError": true,
	"CreateContainerError":       true,
}

// classifyPod returns the list of reasons a pod counts as unhealthy, empty
// when it looks fine. Restart detection is time-based (a termination inside
// restartWindow) rather than restartCount-based: the count is cumulative for
// the pod's lifetime, so a threshold on it would alarm forever once crossed.
func classifyPod(pod *corev1.Pod, now time.Time, restartWindow, grace time.Duration) []string {
	for _, owner := range pod.OwnerReferences {
		if owner.Kind == "Job" {
			return nil
		}
	}

	var reasons []string
	switch pod.Status.Phase {
	case corev1.PodSucceeded:
		return nil
	case corev1.PodFailed:
		return []string{"pod Failed"}
	case corev1.PodPending:
		if age := now.Sub(pod.CreationTimestamp.Time); age > grace {
			reasons = append(reasons, fmt.Sprintf("Pending for %s", age.Round(time.Minute)))
		}
	}

	statuses := make([]corev1.ContainerStatus, 0,
		len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	for _, cs := range statuses {
		if w := cs.State.Waiting; w != nil && alertWaitingReasons[w.Reason] {
			reasons = append(reasons, fmt.Sprintf("%s: %s", cs.Name, w.Reason))
			continue // the backoff already covers its recent restarts
		}
		if t := cs.LastTerminationState.Terminated; t != nil && cs.RestartCount > 0 {
			if since := now.Sub(t.FinishedAt.Time); since >= 0 && since <= restartWindow {
				reasons = append(reasons, fmt.Sprintf(
					"%s restarted %s ago (restart #%d)", cs.Name, since.Round(time.Second), cs.RestartCount))
			}
		}
	}

	if pod.Status.Phase == corev1.PodRunning {
		for _, cond := range pod.Status.Conditions {
			if cond.Type == corev1.PodReady && cond.Status != corev1.ConditionTrue {
				if since := now.Sub(cond.LastTransitionTime.Time); since > grace {
					reasons = append(reasons, fmt.Sprintf("not Ready for %s", since.Round(time.Minute)))
				}
			}
		}
	}
	return reasons
}
