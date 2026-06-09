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

func vtaDeploymentName(sessionID uint) string {
	return fmt.Sprintf("vta-%d", sessionID)
}

func vtaServiceName(sessionID uint) string {
	return fmt.Sprintf("vta-%d", sessionID)
}

// CreateVtaDeployment creates a Deployment that runs the VTA service using the PVC created
// during setup. The PVC is mounted at /app/vta, so VTA reads config.toml from
// there. Port 8100. Idempotent — AlreadyExists is ignored.
func (c *Client) CreateVtaDeployment(ctx context.Context, ns string, sessionID uint, image string) error {
	name := vtaDeploymentName(sessionID)
	pvcName := VtaPVCName(sessionID)
	replicas := int32(1)
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
					Containers: []corev1.Container{{
						Name:  "vta",
						Image: image,
						Ports: []corev1.ContainerPort{{
							ContainerPort: port,
							Protocol:      corev1.ProtocolTCP,
						}},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/app/vta",
						}},
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

// CreateVtaIngress creates an nginx Ingress routing the session FQDN to the VTA service.
// TLS is handled by the cluster-wide wildcard default-ssl-certificate on nginx-ingress;
// no tls: block is needed. Idempotent — AlreadyExists is ignored.
func (c *Client) CreateVtaIngress(ctx context.Context, ns string, sessionID uint, fqdn string) error {
	name := vtaDeploymentName(sessionID)
	svcName := vtaServiceName(sessionID)
	port := intstr.FromInt32(8100)
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
func (c *Client) StreamVtaPodLogs(ctx context.Context, ns string, sessionID uint, onLine func(string)) error {
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
				podName = pods.Items[0].Name
				break wait
			}
		}
	}

	req := c.kube.CoreV1().Pods(ns).GetLogs(podName, &corev1.PodLogOptions{Follow: true})
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
	_ = c.kube.BatchV1().Jobs(ns).Delete(ctx, ImportDidJobName(sessionID), opts)
	_ = c.kube.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, VtaPVCName(sessionID), metav1.DeleteOptions{})
}
