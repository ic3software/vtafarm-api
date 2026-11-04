package k8s

import (
	"bufio"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/util/retry"
)

const (
	PoolActivationAnnotation   = "vtafarm.ic3software.com/pool-activation-id"
	VtaActivationContainerName = "vta-activate"
	activationRestartLimit     = int32(3)
)

var ErrPoolActivationConflict = errors.New("pooled VTA activation conflicts with the existing Deployment")

type VtaActivationSpec struct {
	SessionID    uint
	Image        string
	AdminDID     string
	AdminLabel   string
	ControlDID   string
	ActivationID string
}

func vtaDeploymentName(sessionID uint) string {
	return fmt.Sprintf("vta-%d", sessionID)
}

func vtaServiceName(sessionID uint) string {
	return fmt.Sprintf("vta-%d", sessionID)
}

// VtaServiceAccount is the ServiceAccount the VTA setup/import-did jobs and the
// VTA Deployment run as. The farm Vault's per-user kubernetes-auth role is bound
// to this SA name + the user namespace, so the VTA can authenticate to Vault and
// read/write only its own seed paths. The master seed lives in Vault — there is
// no per-session Kubernetes Secret anymore.
const VtaServiceAccount = "vta"

// VtaTuningEnv is shared by the VTA-only and full-stack Deployments.
func VtaTuningEnv() []corev1.EnvVar {
	return []corev1.EnvVar{
		{Name: "STORAGE_FJALL_BLOCK_CACHE", Value: "8388608"},   // 8 MiB
		{Name: "STORAGE_FJALL_WRITE_BUFFER", Value: "16777216"}, // 16 MiB
		{Name: "STORAGE_FJALL_MAX_JOURNAL", Value: "67108864"},  // 64 MiB
	}
}

// CreateVtaDeployment creates a Deployment that runs the VTA service using the PVC created
// during setup. Command and workingDir are set explicitly — the farm decides
// what runs and where state lives, never the image — and the PVC is mounted at
// /work/vta to match, so VTA resolves its relative config.toml and data_dir
// onto it. Port 8100. Idempotent — AlreadyExists is ignored.
func (c *Client) CreateVtaDeployment(ctx context.Context, ns string, sessionID uint, image string, resources corev1.ResourceRequirements) error {
	return c.createVtaDeployment(ctx, ns, sessionID, image, resources, 1)
}

// CreateParkedVtaDeployment creates the final Deployment without starting a
// pod. Pool claim confirmation later injects activation and starts this exact
// Deployment, avoiding a separate provision Job Pod.
func (c *Client) CreateParkedVtaDeployment(ctx context.Context, ns string, sessionID uint, image string, resources corev1.ResourceRequirements) error {
	return c.createVtaDeployment(ctx, ns, sessionID, image, resources, 0)
}

// ActivateParkedVtaDeployment atomically installs the one-shot activation init
// container and starts the already-prepared Deployment. Updating the template
// and replica count together prevents a pod from starting without the claimed
// administrator. Repeating the same activation is safe; a different activation
// identity is refused so two claims can never write the PVC.
func (c *Client) ActivateParkedVtaDeployment(ctx context.Context, ns string, spec VtaActivationSpec) error {
	command, err := vtaActivationCommand(spec)
	if err != nil {
		return err
	}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		name := vtaDeploymentName(spec.SessionID)
		deployment, err := c.kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("get parked deployment %s: %w", name, err)
		}

		currentID := deployment.Spec.Template.Annotations[PoolActivationAnnotation]
		if currentID != "" && currentID != spec.ActivationID {
			return ErrPoolActivationConflict
		}
		if len(deployment.Spec.Template.Spec.Containers) != 1 ||
			deployment.Spec.Template.Spec.Containers[0].Name != "vta" ||
			deployment.Spec.Template.Spec.Containers[0].Image != spec.Image {
			return fmt.Errorf("%w: parked VTA template is incompatible", ErrPoolActivationConflict)
		}

		activation := corev1.Container{
			Name:       VtaActivationContainerName,
			Image:      spec.Image,
			Command:    []string{"sh", "-c", command},
			WorkingDir: "/work/vta",
			Env: []corev1.EnvVar{
				{Name: "NO_COLOR", Value: "1"},
				{Name: "CLICOLOR", Value: "0"},
			},
			VolumeMounts: []corev1.VolumeMount{{Name: "data", MountPath: "/work/vta"}},
		}
		found := false
		for i := range deployment.Spec.Template.Spec.InitContainers {
			if deployment.Spec.Template.Spec.InitContainers[i].Name != VtaActivationContainerName {
				continue
			}
			found = true
			if currentID == "" || !activationContainersEqual(deployment.Spec.Template.Spec.InitContainers[i], activation) {
				return fmt.Errorf("%w: activation init container does not match", ErrPoolActivationConflict)
			}
		}
		if currentID != "" && !found {
			return fmt.Errorf("%w: activation annotation has no init container", ErrPoolActivationConflict)
		}

		replicas := int32(0)
		if deployment.Spec.Replicas != nil {
			replicas = *deployment.Spec.Replicas
		}
		if currentID == "" && replicas != 0 {
			return fmt.Errorf("%w: unclaimed Deployment is already running", ErrPoolActivationConflict)
		}
		if currentID == spec.ActivationID && found && replicas == 1 {
			return nil
		}

		if !found {
			deployment.Spec.Template.Spec.InitContainers = append(
				deployment.Spec.Template.Spec.InitContainers, activation,
			)
		}
		if deployment.Annotations == nil {
			deployment.Annotations = map[string]string{}
		}
		if deployment.Spec.Template.Annotations == nil {
			deployment.Spec.Template.Annotations = map[string]string{}
		}
		deployment.Annotations[PoolActivationAnnotation] = spec.ActivationID
		deployment.Spec.Template.Annotations[PoolActivationAnnotation] = spec.ActivationID
		one := int32(1)
		deployment.Spec.Replicas = &one
		if _, err := c.kube.AppsV1().Deployments(ns).Update(ctx, deployment, metav1.UpdateOptions{}); err != nil {
			return fmt.Errorf("activate parked deployment %s: %w", name, err)
		}
		return nil
	})
}

func activationContainersEqual(left, right corev1.Container) bool {
	return left.Name == right.Name && left.Image == right.Image &&
		reflect.DeepEqual(left.Command, right.Command) &&
		reflect.DeepEqual(left.Args, right.Args) &&
		left.WorkingDir == right.WorkingDir &&
		reflect.DeepEqual(left.Env, right.Env) &&
		reflect.DeepEqual(left.VolumeMounts, right.VolumeMounts)
}

// vtaActivationCommand is safe to retry after a container or API restart.
// import-did is not itself idempotent (it prompts on an existing ACL row), so
// the authoritative ACL probe closes the command/receipt crash window. Server
// registration is an idempotent upsert when the id and DID are unchanged.
func vtaActivationCommand(spec VtaActivationSpec) (string, error) {
	decoded, err := hex.DecodeString(spec.ActivationID)
	if err != nil || len(decoded) != 16 {
		return "", errors.New("activation ID must be 32 hexadecimal characters")
	}
	if spec.SessionID == 0 || spec.Image == "" || spec.AdminDID == "" {
		return "", errors.New("activation requires session, image, and administrator DID")
	}

	importCmd := "vta import-did --did " + quoteShellArg(spec.AdminDID) + " --role admin"
	if spec.AdminLabel != "" {
		importCmd += " --label " + quoteShellArg(spec.AdminLabel)
	}
	var command strings.Builder
	command.WriteString("set -eu\n")
	command.WriteString("receipt_dir=.vtafarm/activation/" + spec.ActivationID + "\n")
	command.WriteString("mkdir -p \"$receipt_dir\"\n")
	command.WriteString("admin_receipt=\"$receipt_dir/admin-imported\"\n")
	command.WriteString("if [ ! -f \"$admin_receipt\" ]; then\n")
	command.WriteString("  if ! vta acl get " + quoteShellArg(spec.AdminDID) + " >/dev/null 2>&1; then\n")
	command.WriteString("    " + importCmd + "\n")
	command.WriteString("  fi\n")
	command.WriteString("  tmp=\"${admin_receipt}.tmp.$$\"\n")
	command.WriteString("  printf '%s\\n' " + quoteShellArg(spec.ActivationID) + " >\"$tmp\"\n")
	command.WriteString("  mv -f \"$tmp\" \"$admin_receipt\"\n")
	command.WriteString("fi\n")
	if spec.ControlDID != "" {
		command.WriteString("control_receipt=\"$receipt_dir/control-server-added\"\n")
		command.WriteString("if [ ! -f \"$control_receipt\" ]; then\n")
		command.WriteString("  vta did-mgmt servers add --id control --did " + quoteShellArg(spec.ControlDID) + " --label 'DID Hosting Control Plane'\n")
		command.WriteString("  tmp=\"${control_receipt}.tmp.$$\"\n")
		command.WriteString("  printf '%s\\n' " + quoteShellArg(spec.ActivationID) + " >\"$tmp\"\n")
		command.WriteString("  mv -f \"$tmp\" \"$control_receipt\"\n")
		command.WriteString("fi\n")
	}
	return command.String(), nil
}

// HasProvisionJob identifies work that already crossed the old Job boundary.
// Recovery must finish that path instead of starting a second PVC writer.
func (c *Client) HasProvisionJob(ctx context.Context, ns string, sessionID uint) (bool, error) {
	_, err := c.kube.BatchV1().Jobs(ns).Get(ctx, ProvisionJobName(sessionID), metav1.GetOptions{})
	if k8serrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("get provision job: %w", err)
	}
	return true, nil
}

// WaitForVtaActivation observes both init-container failure and application
// readiness. It stops early after repeated init failures instead of hiding them
// behind the full readiness timeout.
func (c *Client) WaitForVtaActivation(ctx context.Context, ns string, sessionID uint, timeout time.Duration, onStage func(string)) error {
	name := vtaDeploymentName(sessionID)
	selector := fmt.Sprintf("app=vta,session-id=%d", sessionID)
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	initStarted, initCompleted := false, false

	for {
		deployment, deploymentErr := c.kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		pods, podsErr := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if podsErr == nil {
			for i := range pods.Items {
				for _, status := range pods.Items[i].Status.InitContainerStatuses {
					if status.Name != VtaActivationContainerName {
						continue
					}
					if !initStarted && (status.State.Running != nil || status.State.Terminated != nil || status.RestartCount > 0) {
						initStarted = true
						if onStage != nil {
							onStage("activation_init_started")
						}
					}
					if !initCompleted && status.State.Terminated != nil && status.State.Terminated.ExitCode == 0 {
						initCompleted = true
						if onStage != nil {
							onStage("activation_init_completed")
						}
					}
				}
				if err := activationPodError(&pods.Items[i]); err != nil {
					return err
				}
			}
		}
		if deploymentErr == nil && deployment.Status.ObservedGeneration >= deployment.Generation && deployment.Status.ReadyReplicas > 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("timeout waiting for pooled VTA deployment %s to become ready", name)
		case <-ticker.C:
		}
	}
}

func activationPodError(pod *corev1.Pod) error {
	for _, status := range pod.Status.InitContainerStatuses {
		if status.Name != VtaActivationContainerName {
			continue
		}
		if waiting := status.State.Waiting; waiting != nil {
			switch waiting.Reason {
			case "CreateContainerConfigError", "ErrImagePull", "ImagePullBackOff", "InvalidImageName":
				return fmt.Errorf("VTA activation init could not start: %s", waiting.Reason)
			}
		}
		terminated := status.State.Terminated
		if terminated == nil && status.State.Waiting != nil && status.State.Waiting.Reason == "CrashLoopBackOff" {
			terminated = status.LastTerminationState.Terminated
		}
		if terminated != nil && terminated.ExitCode != 0 && status.RestartCount >= activationRestartLimit {
			return fmt.Errorf("VTA activation init failed after %d attempts (exit code %d)", status.RestartCount, terminated.ExitCode)
		}
	}
	return nil
}

func (c *Client) createVtaDeployment(ctx context.Context, ns string, sessionID uint, image string, resources corev1.ResourceRequirements, replicas int32) error {
	name := vtaDeploymentName(sessionID)
	pvcName := VtaPVCName(sessionID)
	port := int32(8100)
	labels := map[string]string{
		"app":        "vta",
		"session-id": fmt.Sprintf("%d", sessionID),
	}

	_, err := c.kube.AppsV1().Deployments(ns).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: VtaServiceAccount,
					Containers: []corev1.Container{{
						Name:       "vta",
						Image:      image,
						Command:    []string{"vta"},
						WorkingDir: "/work/vta",
						// Disable ANSI colour codes in the VTA's tracing logs so
						// streamed/captured output (provision --follow, kubectl
						// logs, log stores) stays plain text instead of leaking
						// escape sequences. NO_COLOR is the cross-tool standard;
						// CLICOLOR=0 covers tools that honour that convention too.
						Env: append([]corev1.EnvVar{
							{Name: "NO_COLOR", Value: "1"},
							{Name: "CLICOLOR", Value: "0"},
						}, VtaTuningEnv()...),
						Ports: []corev1.ContainerPort{{
							ContainerPort: port,
							Protocol:      corev1.ProtocolTCP,
						}},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/health",
									Port: intstr.FromInt32(port),
								},
							},
							InitialDelaySeconds: 1,
							PeriodSeconds:       2,
							TimeoutSeconds:      2,
							FailureThreshold:    3,
						},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/work/vta",
						}},
						Resources: resources,
					}},
					Volumes: []corev1.Volume{{
						Name: "data",
						VolumeSource: corev1.VolumeSource{
							PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvcName,
							},
						},
					}},
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create deployment: %w", err)
	}
	return nil
}

// CreateVtaService creates a ClusterIP Service for the VTA Deployment on port 8100.
// Idempotent — AlreadyExists is ignored.
func (c *Client) CreateVtaService(ctx context.Context, ns string, sessionID uint) error {
	name := vtaServiceName(sessionID)
	labels := map[string]string{
		"app":        "vta",
		"session-id": fmt.Sprintf("%d", sessionID),
	}

	_, err := c.kube.CoreV1().Services(ns).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    labels,
		},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Port:       8100,
				TargetPort: intstr.FromInt32(8100),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create service: %w", err)
	}
	return nil
}

// CreateVtaIngress creates an Ingress routing the session FQDN to the VTA service.
// TLS is the cluster-wide wildcard, which Traefik serves as its default
// certificate, so no tls: block is needed — and the HTTP→HTTPS redirect is an
// entrypoint setting on the controller, so no annotation is either.
// Idempotent — AlreadyExists is ignored.
func (c *Client) CreateVtaIngress(ctx context.Context, ns string, sessionID uint, fqdn string) error {
	name := vtaDeploymentName(sessionID)
	svcName := vtaServiceName(sessionID)
	port := intstr.FromInt32(8100)
	pathType := networkingv1.PathTypePrefix
	ingressClass := IngressClass

	_, err := c.kube.NetworkingV1().Ingresses(ns).Create(ctx, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClass,
			Rules: []networkingv1.IngressRule{{
				Host: fqdn,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: svcName,
									Port: networkingv1.ServiceBackendPort{Number: port.IntVal},
								},
							},
						}},
					},
				},
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ingress: %w", err)
	}
	return nil
}

// StreamVtaPodLogs finds the running VTA pod for a session and streams its logs line by line.
// Waits up to 2 minutes for the pod to appear. Blocks until the stream ends or ctx is cancelled.
// fromStart=true delivers all historical logs from pod start; false streams only new lines.
func (c *Client) StreamVtaPodLogs(ctx context.Context, ns string, sessionID uint, fromStart bool, onLine func(string)) error {
	selector := fmt.Sprintf("app=vta,session-id=%d", sessionID)

	var podName string
	timeout := time.After(2 * time.Minute)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

wait:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timeout:
			return fmt.Errorf("timeout waiting for VTA pod (session %d)", sessionID)
		case <-ticker.C:
			pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
			if err == nil && len(pods.Items) > 0 {
				p := pods.Items[0]
				if p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodSucceeded {
					podName = p.Name
					break wait
				}
			}
		}
	}

	opts := &corev1.PodLogOptions{Follow: true}
	if !fromStart {
		tailLines := int64(0)
		opts.TailLines = &tailLines
	}
	req := c.kube.CoreV1().Pods(ns).GetLogs(podName, opts)
	stream, err := req.Stream(ctx)
	if err != nil {
		return fmt.Errorf("stream logs for pod %s: %w", podName, err)
	}
	defer stream.Close()

	scanner := bufio.NewScanner(stream)
	for scanner.Scan() {
		onLine(scanner.Text())
	}
	return scanner.Err()
}

// DeleteVtaResources removes the Deployment, Service, Ingress, import-did Job, and PVC for a session.
// All errors are silently ignored — this is best-effort cleanup.
func (c *Client) DeleteVtaResources(ctx context.Context, ns string, sessionID uint) {
	propagation := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &propagation}

	_ = c.kube.AppsV1().Deployments(ns).Delete(ctx, vtaDeploymentName(sessionID), opts)
	_ = c.kube.NetworkingV1().Ingresses(ns).Delete(ctx, vtaDeploymentName(sessionID), metav1.DeleteOptions{})
	_ = c.kube.CoreV1().Services(ns).Delete(ctx, vtaServiceName(sessionID), metav1.DeleteOptions{})
	_ = c.kube.BatchV1().Jobs(ns).Delete(ctx, ProvisionJobName(sessionID), opts)
	_ = c.kube.BatchV1().Jobs(ns).Delete(ctx, VtaACLJobName(sessionID), opts)
	c.DeleteComponentJob(ctx, ns, FSJobConfigUpdate(sessionID))
	c.DeleteComponentJob(ctx, ns, FSJobConfigRollback(sessionID))
	_ = c.kube.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, VtaPVCName(sessionID), metav1.DeleteOptions{})
}
