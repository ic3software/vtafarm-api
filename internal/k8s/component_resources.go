package k8s

import (
	"bufio"
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// PodOperatorServiceAccount is the ServiceAccount full_stack's mediator and
// dids Jobs/Deployments run as — they don't talk to Vault via kubernetes
// auth (the mediator uses a token instead, the dids daemon is plaintext), so
// they don't need the "vta" SA's Vault binding.
const PodOperatorServiceAccount = "pod-operator"

// ComponentDeploymentSpec configures a full_stack long-running server
// Deployment. Generic over component (vta/mediator/dids) — distinct from
// vta_only's CreateVtaDeployment (left untouched; see plan decision #4),
// which is specific to the VTA's /app/vta mount and port 8100.
type ComponentDeploymentSpec struct {
	Name           string
	Image          string
	Command        []string // nil = image entrypoint
	WorkingDir     string
	ServiceAccount string
	PVCMounts      []PVCMount
	Env            []corev1.EnvVar
	Port           int32
	Labels         map[string]string
}

// CreateComponentDeployment creates a Deployment per spec. Idempotent —
// AlreadyExists is ignored.
func (c *Client) CreateComponentDeployment(ctx context.Context, ns string, spec ComponentDeploymentSpec) error {
	replicas := int32(1)

	volumes := make([]corev1.Volume, 0, len(spec.PVCMounts))
	mounts := make([]corev1.VolumeMount, 0, len(spec.PVCMounts))
	for _, m := range spec.PVCMounts {
		volumes = append(volumes, corev1.Volume{
			Name: m.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: m.ClaimName},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: m.Name, MountPath: m.MountPath})
	}

	var ports []corev1.ContainerPort
	if spec.Port != 0 {
		ports = []corev1.ContainerPort{{ContainerPort: spec.Port, Protocol: corev1.ProtocolTCP}}
	}

	_, err := c.kube.AppsV1().Deployments(ns).Create(ctx, &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: ns, Labels: spec.Labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: spec.Labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: spec.Labels},
				Spec: corev1.PodSpec{
					ServiceAccountName: spec.ServiceAccount,
					Containers: []corev1.Container{{
						Name:         spec.Name,
						Image:        spec.Image,
						Command:      spec.Command,
						WorkingDir:   spec.WorkingDir,
						Env:          spec.Env,
						Ports:        ports,
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create deployment %s: %w", spec.Name, err)
	}
	return nil
}

// ScaleComponentDeployment sets a Deployment's replica count. Used to stop
// the dids daemon before a Job needs exclusive access to its local (Fjall)
// store on the shared PVC — the daemon takes a file lock the whole time it's
// running, so a concurrent Job open fails with "store error: FjallError:
// Locked" — and to restart it afterwards.
func (c *Client) ScaleComponentDeployment(ctx context.Context, ns, name string, replicas int32) error {
	deploy, err := c.kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("get deployment %s: %w", name, err)
	}
	deploy.Spec.Replicas = &replicas
	if _, err := c.kube.AppsV1().Deployments(ns).Update(ctx, deploy, metav1.UpdateOptions{}); err != nil {
		return fmt.Errorf("scale deployment %s to %d: %w", name, replicas, err)
	}
	return nil
}

// WaitForComponentPodsGone polls until no pod matching selector remains.
// Scaling a Deployment to 0 only stops new scheduling — the outgoing pod
// keeps its file lock on the PVC until it actually terminates, so callers
// that need exclusive access (e.g. ReissueDidsEnroll) must wait for this
// before starting a Job against the same volume.
func (c *Client) WaitForComponentPodsGone(ctx context.Context, ns, selector string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err == nil && len(pods.Items) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for pods (selector %q) to terminate", selector)
		case <-ticker.C:
		}
	}
}

// WaitForComponentDeploymentReady polls until the Deployment reports at
// least one ready replica. Used after scaling back up so callers can confirm
// the daemon actually came back, not just that the scale request was
// accepted.
func (c *Client) WaitForComponentDeploymentReady(ctx context.Context, ns, name string, timeout time.Duration) error {
	deadline := time.After(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		deploy, err := c.kube.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
		if err == nil && deploy.Status.ReadyReplicas > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for deployment %s to become ready", name)
		case <-ticker.C:
		}
	}
}

// CreateComponentService creates a ClusterIP Service selecting labels on
// port. Idempotent — AlreadyExists is ignored.
func (c *Client) CreateComponentService(ctx context.Context, ns, name string, labels map[string]string, port int32) error {
	_, err := c.kube.CoreV1().Services(ns).Create(ctx, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: corev1.ServiceSpec{
			Type:     corev1.ServiceTypeClusterIP,
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Port:       port,
				TargetPort: intstr.FromInt32(port),
				Protocol:   corev1.ProtocolTCP,
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create service %s: %w", name, err)
	}
	return nil
}

// CreateComponentIngress creates an nginx Ingress routing host to svcName on
// port. TLS is handled by the cluster-wide wildcard default-ssl-certificate
// (no tls: block needed — matches CreateVtaIngress). Idempotent.
func (c *Client) CreateComponentIngress(ctx context.Context, ns, name, svcName string, port int32, host string) error {
	pathType := networkingv1.PathTypePrefix
	ingressClass := "nginx"

	_, err := c.kube.NetworkingV1().Ingresses(ns).Create(ctx, &networkingv1.Ingress{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Annotations: map[string]string{
				"nginx.ingress.kubernetes.io/ssl-redirect": "true",
			},
		},
		Spec: networkingv1.IngressSpec{
			IngressClassName: &ingressClass,
			Rules: []networkingv1.IngressRule{{
				Host: host,
				IngressRuleValue: networkingv1.IngressRuleValue{
					HTTP: &networkingv1.HTTPIngressRuleValue{
						Paths: []networkingv1.HTTPIngressPath{{
							Path:     "/",
							PathType: &pathType,
							Backend: networkingv1.IngressBackend{
								Service: &networkingv1.IngressServiceBackend{
									Name: svcName,
									Port: networkingv1.ServiceBackendPort{Number: port},
								},
							},
						}},
					},
				},
			}},
		},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create ingress %s: %w", name, err)
	}
	return nil
}

// StreamComponentPodLogs finds the running pod matching selector and streams
// its logs line by line. Waits up to 2 minutes for the pod to appear. Mirrors
// StreamVtaPodLogs (setup_jobs.go is vta_only-specific) but generic over the
// label selector so it covers the mediator and dids Deployments too.
func (c *Client) StreamComponentPodLogs(ctx context.Context, ns, selector string, fromStart bool, onLine func(string)) error {
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
			return fmt.Errorf("timeout waiting for pod (selector %q)", selector)
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

// DeleteComponentResources removes the Deployment, Service, Ingress, and PVC
// for one full_stack component (name is one of FSVtaName/FSMediatorName/
// FSDidsName). Best-effort — errors are silently ignored.
func (c *Client) DeleteComponentResources(ctx context.Context, ns, name string) {
	propagation := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &propagation}

	_ = c.kube.AppsV1().Deployments(ns).Delete(ctx, name, opts)
	_ = c.kube.NetworkingV1().Ingresses(ns).Delete(ctx, name, metav1.DeleteOptions{})
	_ = c.kube.CoreV1().Services(ns).Delete(ctx, name, metav1.DeleteOptions{})
	_ = c.kube.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, name, metav1.DeleteOptions{})
}
