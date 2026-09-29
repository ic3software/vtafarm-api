package k8s

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
)

// VtaDeploymentName exposes vta_only's Deployment name (vta-<sessionID>) to
// the upgrade runner, which patches Deployments created by CreateVtaDeployment.
// full_stack components use the exported FS*Name funcs directly.
func VtaDeploymentName(sessionID uint) string {
	return vtaDeploymentName(sessionID)
}

// SetDeploymentImage points every container in the Deployment at image and
// forces the Recreate strategy. Recreate matters: these Deployments mount RWO
// PVCs, so the default RollingUpdate would start the new pod while the old
// one still holds the volume and deadlock the rollout. Replacing the whole
// Strategy struct also clears any rollingUpdate params, which the API server
// rejects alongside type Recreate.
func (c *Client) SetDeploymentImage(ctx context.Context, ns, name, image string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deploy, err := c.kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		deploy.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		for i := range deploy.Spec.Template.Spec.Containers {
			deploy.Spec.Template.Spec.Containers[i].Image = image
		}
		_, err = c.kube.AppsV1().Deployments(ns).Update(ctx, deploy, metav1.UpdateOptions{})
		return err
	})
}

// RolloutStatus is a point-in-time view of a Deployment rollout. Reason is a
// pod-level explanation (ImagePullBackOff, CrashLoopBackOff, …) when the
// rollout is stuck — empty while pods are progressing normally.
type RolloutStatus struct {
	Ready             bool
	Reason            string
	CrashLoopRestarts int32
}

// DeploymentRollout reports whether the Deployment has fully rolled out
// image: spec updated, generation observed, and all replicas updated + ready.
// When not ready it best-effort surfaces why from the pods' container states.
func (c *Client) DeploymentRollout(ctx context.Context, ns, name, image string) (RolloutStatus, error) {
	deploy, err := c.kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return RolloutStatus{}, fmt.Errorf("get deployment %s: %w", name, err)
	}

	specMatches := true
	for _, ctr := range deploy.Spec.Template.Spec.Containers {
		if ctr.Image != image {
			specMatches = false
			break
		}
	}

	replicas := int32(1)
	if deploy.Spec.Replicas != nil {
		replicas = *deploy.Spec.Replicas
	}
	selector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
	if err != nil {
		return RolloutStatus{}, fmt.Errorf("deployment selector: %w", err)
	}
	pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return RolloutStatus{}, fmt.Errorf("list deployment pods: %w", err)
	}
	status := RolloutStatus{}
	var readyPods int32
	for _, pod := range pods.Items {
		if pod.DeletionTimestamp != nil {
			continue
		}
		matches := len(pod.Spec.Containers) > 0
		for _, ctr := range pod.Spec.Containers {
			matches = matches && ctr.Image == image
		}
		if !matches {
			continue
		}
		ready := len(pod.Status.ContainerStatuses) == len(pod.Spec.Containers)
		for _, cs := range pod.Status.ContainerStatuses {
			ready = ready && cs.Ready
			if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
				if status.Reason == "" {
					status.Reason = w.Reason
					if w.Message != "" {
						status.Reason += ": " + w.Message
					}
				}
				if w.Reason == "CrashLoopBackOff" && cs.RestartCount > status.CrashLoopRestarts {
					status.CrashLoopRestarts = cs.RestartCount
					status.Reason = fmt.Sprintf("pod %s container %s: CrashLoopBackOff after %d restarts", pod.Name, cs.Name, cs.RestartCount)
				}
			}
		}
		if ready {
			readyPods++
		}
	}
	if specMatches &&
		deploy.Status.ObservedGeneration >= deploy.Generation &&
		deploy.Status.Replicas == replicas &&
		deploy.Status.UpdatedReplicas == replicas &&
		deploy.Status.ReadyReplicas == replicas && readyPods == replicas {
		status.Ready = true
	}

	return status, nil
}
