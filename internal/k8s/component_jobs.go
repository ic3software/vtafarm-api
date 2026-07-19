package k8s

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PVCMount describes one PVC mounted into a full_stack Job or Deployment.
// Cross-component steps (design §4) mount two of these at once — e.g. the
// VTA and mediator PVCs together for step_mediator_reprov.
type PVCMount struct {
	Name      string // volume name, unique within the pod spec
	ClaimName string
	MountPath string // e.g. "/work/vta"
}

// ComponentJobSpec configures a one-off full_stack setup Job. Generic over
// component (vta/mediator/dids) and over how many PVCs it touches — distinct
// from vta_only's CreateSetupResources/CreateProvisionJob (left untouched;
// see plan decision #4), which are specific to the single VTA PVC mounted at
// /app/vta.
type ComponentJobSpec struct {
	Name           string
	Image          string
	Command        []string
	WorkingDir     string
	ServiceAccount string
	PVCMounts      []PVCMount

	// ConfigMapName == "" means no recipe ConfigMap is mounted (e.g.
	// step_import_admin_did, which only runs a CLI command against the PVC).
	ConfigMapName string
	ConfigMapKey  string // e.g. "vta-setup.toml", mounted at /config/<key>
	ConfigMapData string

	Env []corev1.EnvVar

	// ActiveDeadlineSeconds defaults to 600 (10 min) when zero.
	ActiveDeadlineSeconds int64
}

// CreateComponentPVC creates an RWO PVC of storageSize for a full_stack
// component.
// Idempotent — AlreadyExists is ignored.
func (c *Client) CreateComponentPVC(ctx context.Context, ns, name, storageSize string) error {
	storage, err := resource.ParseQuantity(storageSize)
	if err != nil {
		return fmt.Errorf("parse pvc %s storage size %q: %w", name, storageSize, err)
	}

	_, err = c.kube.CoreV1().PersistentVolumeClaims(ns).Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"managed-by": "vtafarm"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: storage,
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create pvc %s: %w", name, err)
	}
	return nil
}

// CreateComponentJob creates (idempotently) the ConfigMap (if any) and Job
// described by spec. WaitForJob/JobLogs/StreamJobLogs (setup_jobs.go) are
// already generic over job name and are reused as-is to drive it.
func (c *Client) CreateComponentJob(ctx context.Context, ns string, spec ComponentJobSpec) error {
	if spec.ConfigMapName != "" {
		_, err := c.kube.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: spec.ConfigMapName, Namespace: ns},
			Data:       map[string]string{spec.ConfigMapKey: spec.ConfigMapData},
		}, metav1.CreateOptions{})
		if err != nil && !k8serrors.IsAlreadyExists(err) {
			return fmt.Errorf("create configmap %s: %w", spec.ConfigMapName, err)
		}
	}

	volumes := make([]corev1.Volume, 0, len(spec.PVCMounts)+1)
	mounts := make([]corev1.VolumeMount, 0, len(spec.PVCMounts)+1)
	for _, m := range spec.PVCMounts {
		volumes = append(volumes, corev1.Volume{
			Name: m.Name,
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: m.ClaimName},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: m.Name, MountPath: m.MountPath})
	}
	if spec.ConfigMapName != "" {
		volumes = append(volumes, corev1.Volume{
			Name: "config",
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: spec.ConfigMapName},
				},
			},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: "config", MountPath: "/config"})
	}

	backoff := int32(0)
	ttl := int32(3600)
	deadline := spec.ActiveDeadlineSeconds
	if deadline == 0 {
		deadline = 600
	}

	_, err := c.kube.BatchV1().Jobs(ns).Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: spec.Name, Namespace: ns},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:      corev1.RestartPolicyNever,
					ServiceAccountName: spec.ServiceAccount,
					Containers: []corev1.Container{{
						Name:         spec.Name,
						Image:        spec.Image,
						Command:      spec.Command,
						WorkingDir:   spec.WorkingDir,
						Env:          spec.Env,
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create job %s: %w", spec.Name, err)
	}
	return nil
}

// DeleteComponentJob removes a full_stack setup Job and its ConfigMap of the
// same name (if any). Best-effort — errors are silently ignored.
func (c *Client) DeleteComponentJob(ctx context.Context, ns, name string) {
	propagation := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &propagation}
	_ = c.kube.BatchV1().Jobs(ns).Delete(ctx, name, opts)
	_ = c.kube.CoreV1().ConfigMaps(ns).Delete(ctx, name, metav1.DeleteOptions{})
}

// DeleteAllComponentJobs removes every full_stack setup Job (+ ConfigMap)
// for a session. Best-effort.
func (c *Client) DeleteAllComponentJobs(ctx context.Context, ns string, sessionID uint) {
	for _, name := range allFSJobNames(sessionID) {
		c.DeleteComponentJob(ctx, ns, name)
	}
}
