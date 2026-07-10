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
	Ready  bool
	Reason string
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
	if specMatches &&
		deploy.Status.ObservedGeneration >= deploy.Generation &&
		deploy.Status.UpdatedReplicas == replicas &&
		deploy.Status.ReadyReplicas == replicas {
		return RolloutStatus{Ready: true}, nil
	}

	return RolloutStatus{Reason: c.podStuckReason(ctx, ns, deploy)}, nil
}

// podStuckReason scans the Deployment's pods for waiting containers and
// returns the first meaningful reason (best-effort — "" on any error).
func (c *Client) podStuckReason(ctx context.Context, ns string, deploy *appsv1.Deployment) string {
	selector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
	if err != nil {
		return ""
	}
	pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector.String()})
	if err != nil {
		return ""
	}
	for _, pod := range pods.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			if w := cs.State.Waiting; w != nil && w.Reason != "" && w.Reason != "ContainerCreating" {
				if w.Message != "" {
					return w.Reason + ": " + w.Message
				}
				return w.Reason
			}
		}
	}
	return ""
}
