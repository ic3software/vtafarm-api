package k8s

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
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
// during setup. The PVC is mounted at /vta-data (workingDir), so VTA reads config.toml from
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
						Name:       "vta",
						Image:      image,
						WorkingDir: "/vta-data",
						Ports: []corev1.ContainerPort{{
							ContainerPort: port,
							Protocol:      corev1.ProtocolTCP,
						}},
						VolumeMounts: []corev1.VolumeMount{{
							Name:      "data",
							MountPath: "/vta-data",
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

// DeleteVtaResources removes the Deployment, Service, import-did Job, and PVC for a session.
// All errors are silently ignored — this is best-effort cleanup.
func (c *Client) DeleteVtaResources(ctx context.Context, ns string, sessionID uint) {
	propagation := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &propagation}

	_ = c.kube.AppsV1().Deployments(ns).Delete(ctx, vtaDeploymentName(sessionID), opts)
	_ = c.kube.CoreV1().Services(ns).Delete(ctx, vtaServiceName(sessionID), metav1.DeleteOptions{})
	_ = c.kube.BatchV1().Jobs(ns).Delete(ctx, ImportDidJobName(sessionID), opts)
	_ = c.kube.CoreV1().PersistentVolumeClaims(ns).Delete(ctx, VtaPVCName(sessionID), metav1.DeleteOptions{})
}
