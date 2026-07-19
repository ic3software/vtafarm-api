package k8s

import (
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestClassifyPod(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	window := 15 * time.Minute
	grace := 10 * time.Minute

	runningReady := func() *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:              "vta",
				CreationTimestamp: metav1.Time{Time: now.Add(-time.Hour)},
			},
			Status: corev1.PodStatus{
				Phase: corev1.PodRunning,
				Conditions: []corev1.PodCondition{{
					Type:               corev1.PodReady,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: metav1.Time{Time: now.Add(-time.Hour)},
				}},
				ContainerStatuses: []corev1.ContainerStatus{{
					Name:  "vta",
					State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
				}},
			},
		}
	}

	t.Run("healthy running pod", func(t *testing.T) {
		if got := classifyPod(runningReady(), now, window, grace); len(got) != 0 {
			t.Fatalf("expected no reasons, got %v", got)
		}
	})

	t.Run("crash loop", func(t *testing.T) {
		pod := runningReady()
		pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
		}
		got := classifyPod(pod, now, window, grace)
		if len(got) != 1 || !strings.Contains(got[0], "CrashLoopBackOff") {
			t.Fatalf("expected CrashLoopBackOff reason, got %v", got)
		}
	})

	t.Run("recent restart inside window", func(t *testing.T) {
		pod := runningReady()
		pod.Status.ContainerStatuses[0].RestartCount = 3
		pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				FinishedAt: metav1.Time{Time: now.Add(-5 * time.Minute)},
			},
		}
		got := classifyPod(pod, now, window, grace)
		if len(got) != 1 || !strings.Contains(got[0], "restarted") {
			t.Fatalf("expected restart reason, got %v", got)
		}
	})

	t.Run("old restart outside window stays quiet", func(t *testing.T) {
		pod := runningReady()
		pod.Status.ContainerStatuses[0].RestartCount = 7
		pod.Status.ContainerStatuses[0].LastTerminationState = corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				FinishedAt: metav1.Time{Time: now.Add(-2 * time.Hour)},
			},
		}
		if got := classifyPod(pod, now, window, grace); len(got) != 0 {
			t.Fatalf("cumulative restartCount must not alarm on its own, got %v", got)
		}
	})

	t.Run("pending inside grace stays quiet", func(t *testing.T) {
		pod := runningReady()
		pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
		pod.CreationTimestamp = metav1.Time{Time: now.Add(-2 * time.Minute)}
		if got := classifyPod(pod, now, window, grace); len(got) != 0 {
			t.Fatalf("expected no reasons during grace, got %v", got)
		}
	})

	t.Run("pending past grace", func(t *testing.T) {
		pod := runningReady()
		pod.Status = corev1.PodStatus{Phase: corev1.PodPending}
		pod.CreationTimestamp = metav1.Time{Time: now.Add(-30 * time.Minute)}
		got := classifyPod(pod, now, window, grace)
		if len(got) != 1 || !strings.Contains(got[0], "Pending") {
			t.Fatalf("expected Pending reason, got %v", got)
		}
	})

	t.Run("not ready past grace", func(t *testing.T) {
		pod := runningReady()
		pod.Status.Conditions[0].Status = corev1.ConditionFalse
		pod.Status.Conditions[0].LastTransitionTime = metav1.Time{Time: now.Add(-20 * time.Minute)}
		got := classifyPod(pod, now, window, grace)
		if len(got) != 1 || !strings.Contains(got[0], "not Ready") {
			t.Fatalf("expected not-Ready reason, got %v", got)
		}
	})

	t.Run("job-owned pod is skipped", func(t *testing.T) {
		pod := runningReady()
		pod.OwnerReferences = []metav1.OwnerReference{{Kind: "Job", Name: "setup-job"}}
		pod.Status.ContainerStatuses[0].State = corev1.ContainerState{
			Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"},
		}
		if got := classifyPod(pod, now, window, grace); len(got) != 0 {
			t.Fatalf("job pods must be skipped, got %v", got)
		}
	})

	t.Run("failed pod", func(t *testing.T) {
		pod := runningReady()
		pod.Status = corev1.PodStatus{Phase: corev1.PodFailed}
		got := classifyPod(pod, now, window, grace)
		if len(got) != 1 || got[0] != "pod Failed" {
			t.Fatalf("expected pod Failed, got %v", got)
		}
	})
}
