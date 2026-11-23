package k8s

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
)

const recoveryRestartAnnotation = "vtafarm.ic3software.com/recovery-restarted-at"

type upgradeProbePaths struct {
	readiness string
	liveness  string
	startup   string
}

func currentProbePathsFor(app string) (upgradeProbePaths, bool) {
	switch app {
	case "vta", "fs-vta", "fs-vtc", "fs-trust-registry", "fs-dids":
		return upgradeProbePaths{readiness: "/readyz", liveness: "/livez", startup: "/livez"}, true
	case "fs-mediator":
		return upgradeProbePaths{
			readiness: "/mediator/v1/readyz",
			liveness:  "/mediator/v1/livez",
			startup:   "/mediator/v1/livez",
		}, true
	default:
		return upgradeProbePaths{}, false
	}
}

// rollbackProbePathsFor uses only endpoints available before the dedicated
// probes landed, so a failed new image can still roll back cleanly.
func rollbackProbePathsFor(app string) (upgradeProbePaths, bool) {
	switch app {
	case "vta", "fs-vta", "fs-vtc", "fs-trust-registry":
		return upgradeProbePaths{readiness: "/health", liveness: "/health", startup: "/health"}, true
	case "fs-dids":
		return upgradeProbePaths{readiness: "/health"}, true
	case "fs-mediator":
		return upgradeProbePaths{readiness: "/mediator/v1/readyz"}, true
	default:
		return upgradeProbePaths{}, false
	}
}

func upgradeHTTPProbe(path string, port int32, period, failures int32) *corev1.Probe {
	if path == "" {
		return nil
	}
	return &corev1.Probe{
		ProbeHandler: corev1.ProbeHandler{HTTPGet: &corev1.HTTPGetAction{
			Path: path,
			Port: intstr.FromInt32(port),
		}},
		PeriodSeconds:    period,
		TimeoutSeconds:   2,
		FailureThreshold: failures,
	}
}

func isManagedHTTPProbe(probe *corev1.Probe, port, period, failures int32, paths ...string) bool {
	if probe == nil || probe.HTTPGet == nil || probe.HTTPGet.Port.IntVal != port ||
		probe.PeriodSeconds != period || probe.TimeoutSeconds != 2 ||
		probe.FailureThreshold != failures {
		return false
	}
	for _, path := range paths {
		if path != "" && probe.HTTPGet.Path == path {
			return true
		}
	}
	return false
}

func reconcileProbe(
	probe **corev1.Probe,
	desiredPath string,
	port, period, failures int32,
	managedPaths ...string,
) {
	if *probe != nil && !isManagedHTTPProbe(*probe, port, period, failures, managedPaths...) {
		return
	}
	*probe = upgradeHTTPProbe(desiredPath, port, period, failures)
}

func reconcileHealthProbes(deployment *appsv1.Deployment, rollback bool) {
	current, ok := currentProbePathsFor(deployment.Labels["app"])
	if !ok || len(deployment.Spec.Template.Spec.Containers) == 0 {
		return
	}
	legacy, _ := rollbackProbePathsFor(deployment.Labels["app"])
	desired := current
	if rollback {
		desired = legacy
	}
	container := &deployment.Spec.Template.Spec.Containers[0]
	if len(container.Ports) == 0 {
		return
	}
	port := container.Ports[0].ContainerPort
	reconcileProbe(&container.ReadinessProbe, desired.readiness, port, 2, 3, current.readiness, legacy.readiness)
	reconcileProbe(&container.LivenessProbe, desired.liveness, port, 10, 3, current.liveness, legacy.liveness)
	reconcileProbe(&container.StartupProbe, desired.startup, port, 2, 60, current.startup, legacy.startup)
}

// reconcileUpgradeHealthProbes fills missing probes and migrates probes from
// the Farm's former `/health` policy. Other operator-supplied probes remain.
func reconcileUpgradeHealthProbes(deployment *appsv1.Deployment) {
	reconcileHealthProbes(deployment, false)
}

func reconcileRollbackHealthProbes(deployment *appsv1.Deployment) {
	reconcileHealthProbes(deployment, true)
}

// VtaDeploymentName exposes vta_only's Deployment name (vta-<sessionID>) to
// the upgrade runner, which patches Deployments created by CreateVtaDeployment.
// full_stack components use the exported FS*Name funcs directly.
func VtaDeploymentName(sessionID uint) string {
	return vtaDeploymentName(sessionID)
}

// SetDeploymentImage points every container in the Deployment at image and
// forces the Recreate strategy. The same rollout reconciles component tuning,
// missing health probes, and an existing mediator's persisted Vault CA
// configuration. Recreate matters: these Deployments mount RWO PVCs, so the
// default RollingUpdate would start the new pod while the old one still holds
// the volume and deadlock the rollout. Replacing the whole Strategy struct also
// clears any rollingUpdate params, which the API server rejects alongside type
// Recreate.
func (c *Client) SetDeploymentImage(ctx context.Context, ns, name, image string) error {
	return c.setDeploymentImage(ctx, ns, name, image, false)
}

// RollbackDeploymentImage restores an older image with the legacy probe paths
// that image is guaranteed to expose.
func (c *Client) RollbackDeploymentImage(ctx context.Context, ns, name, image string) error {
	return c.setDeploymentImage(ctx, ns, name, image, true)
}

func (c *Client) setDeploymentImage(ctx context.Context, ns, name, image string, rollback bool) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deploy, err := c.kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return err
		}
		deploy.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		for i := range deploy.Spec.Template.Spec.Containers {
			deploy.Spec.Template.Spec.Containers[i].Image = image
		}
		reconcileTuningEnv(deploy)
		if rollback {
			reconcileRollbackHealthProbes(deploy)
		} else {
			reconcileUpgradeHealthProbes(deploy)
		}
		ensureMediatorVaultCA(deploy, image)
		_, err = c.kube.AppsV1().Deployments(ns).Update(ctx, deploy, metav1.UpdateOptions{})
		return err
	})
}

// RecoverDeploymentImage reconciles an existing failed Deployment to image
// and changes its pod template even when image is unchanged. A deployment can
// be absent when the original create call failed before persistence; that case
// is left to the normal idempotent create helper used immediately afterwards.
func (c *Client) RecoverDeploymentImage(ctx context.Context, ns, name, image string) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		deploy, err := c.kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if k8serrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		deploy.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		for i := range deploy.Spec.Template.Spec.Containers {
			deploy.Spec.Template.Spec.Containers[i].Image = image
		}
		reconcileTuningEnv(deploy)
		ensureMediatorVaultCA(deploy, image)
		if deploy.Spec.Template.Annotations == nil {
			deploy.Spec.Template.Annotations = map[string]string{}
		}
		deploy.Spec.Template.Annotations[recoveryRestartAnnotation] = time.Now().UTC().Format(time.RFC3339Nano)
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
