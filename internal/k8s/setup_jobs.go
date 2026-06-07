package k8s

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// SetupJobName returns the deterministic K8s Job/ConfigMap name for a session.
func SetupJobName(sessionID uint) string {
	return fmt.Sprintf("vta-setup-%d", sessionID)
}

func setupConfigMapName(sessionID uint) string {
	return fmt.Sprintf("vta-setup-%d", sessionID)
}

// VtaPVCName returns the deterministic PVC name for a session's persistent data.
func VtaPVCName(sessionID uint) string {
	return fmt.Sprintf("vta-data-%d", sessionID)
}

// ImportDidJobName returns the K8s Job name for the import-did phase.
func ImportDidJobName(sessionID uint) string {
	return fmt.Sprintf("vta-import-%d", sessionID)
}


// CreateSetupResources creates a 1Gi PVC, a ConfigMap with the TOML config, and a Job
// that runs `vta setup --from /config/vta-setup.toml` with the PVC mounted at /app/vta.
// All three calls are idempotent (AlreadyExists is ignored).
func (c *Client) CreateSetupResources(ctx context.Context, ns string, sessionID uint, toml, vtaImage string) error {
	pvcName := VtaPVCName(sessionID)
	cmName := setupConfigMapName(sessionID)
	jobName := SetupJobName(sessionID)

	// 1. PVC — persists config.toml + data/vta/ across pod restarts and into the Deployment.
	_, err := c.kube.CoreV1().PersistentVolumeClaims(ns).Create(ctx, &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: ns,
			Labels:    map[string]string{"managed-by": "cipherportal"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			StorageClassName: nil, // use cluster default (e.g. longhorn)
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create pvc: %w", err)
	}

	// 2. ConfigMap — input TOML (ephemeral, only needed during setup).
	_, err = c.kube.CoreV1().ConfigMaps(ns).Create(ctx, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: cmName, Namespace: ns},
		Data:       map[string]string{"vta-setup.toml": toml},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create configmap: %w", err)
	}

	// 3. Job — writes config.toml + data/ to the PVC via workingDir.
	backoff := int32(0)
	ttl := int32(3600)
	deadline := int64(600)

	_, err = c.kube.BatchV1().Jobs(ns).Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "vta-setup",
						Image: vtaImage,
						// After setup, print the marker then cat did.jsonl so the
						// orchestrator can extract it from job logs without a reader pod.
						Command:    []string{"sh", "-c", "vta setup --from /config/vta-setup.toml && echo '---DID_LOG_START---' && cat /app/vta/data/vta/did-logs/VTA-did.jsonl"},
						WorkingDir: "/app/vta",
						VolumeMounts: []corev1.VolumeMount{
							{Name: "config", MountPath: "/config"},
							{Name: "data", MountPath: "/app/vta"},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "config",
							VolumeSource: corev1.VolumeSource{
								ConfigMap: &corev1.ConfigMapVolumeSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: cmName},
								},
							},
						},
						{
							Name: "data",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: pvcName,
								},
							},
						},
					},
				},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil && !k8serrors.IsAlreadyExists(err) {
		return fmt.Errorf("create job: %w", err)
	}

	return nil
}

// WaitForJob polls the job every 5 s until it reaches a terminal state.
// Returns (true, "", nil) on success, (false, reason, nil) on failure.
// Returns a non-nil error only on unexpected API errors or context cancellation.
func (c *Client) WaitForJob(ctx context.Context, ns, jobName string) (succeeded bool, failMsg string, err error) {
	for {
		job, getErr := c.kube.BatchV1().Jobs(ns).Get(ctx, jobName, metav1.GetOptions{})
		if getErr != nil {
			if ctx.Err() != nil {
				return false, "", ctx.Err()
			}
			return false, "", fmt.Errorf("get job %s: %w", jobName, getErr)
		}
		for _, cond := range job.Status.Conditions {
			if cond.Type == batchv1.JobComplete && cond.Status == corev1.ConditionTrue {
				return true, "", nil
			}
			if cond.Type == batchv1.JobFailed && cond.Status == corev1.ConditionTrue {
				msg := cond.Message
				if msg == "" {
					msg = "job failed without message"
				}
				return false, msg, nil
			}
		}
		select {
		case <-ctx.Done():
			return false, "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

// JobLogs returns the full combined stdout/stderr of the job's pod.
// Returns an error if no pod exists or logs cannot be fetched.
func (c *Client) JobLogs(ctx context.Context, ns, jobName string) (string, error) {
	pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil {
		return "", fmt.Errorf("list pods for job %s: %w", jobName, err)
	}
	if len(pods.Items) == 0 {
		return "", fmt.Errorf("no pods found for job %s", jobName)
	}

	req := c.kube.CoreV1().Pods(ns).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{})
	stream, err := req.Stream(ctx)
	if err != nil {
		return "", fmt.Errorf("get logs for pod %s: %w", pods.Items[0].Name, err)
	}
	defer stream.Close()

	data, err := io.ReadAll(stream)
	return string(data), err
}

// StreamJobLogs waits up to 2 minutes for the job's pod to appear, then streams its
// logs line by line via onLine. Blocks until the stream ends or ctx is cancelled.
func (c *Client) StreamJobLogs(ctx context.Context, ns, jobName string, onLine func(string)) error {
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
			return fmt.Errorf("timeout waiting for pod for job %s", jobName)
		case <-ticker.C:
			pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{
				LabelSelector: "job-name=" + jobName,
			})
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

// CreateImportDidJob creates a K8s Job that runs `vta import-did --did <adminDid> --role admin`
// using the session PVC (workingDir /app/vta). Idempotent on AlreadyExists.
func (c *Client) CreateImportDidJob(ctx context.Context, ns string, sessionID uint, image, adminDid string) error {
	jobName := ImportDidJobName(sessionID)
	pvcName := VtaPVCName(sessionID)

	backoff := int32(0)
	ttl := int32(3600)
	deadline := int64(300)

	_, err := c.kube.BatchV1().Jobs(ns).Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: ns},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			ActiveDeadlineSeconds:   &deadline,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:       "vta-import",
						Image:      image,
						Command:    []string{"vta", "import-did", "--did", adminDid, "--role", "admin"},
						WorkingDir: "/app/vta",
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
		return fmt.Errorf("create import-did job: %w", err)
	}
	return nil
}

// DeleteSetupResources removes the setup Job and its ConfigMap.
// Errors are silently ignored — this is best-effort cleanup.
func (c *Client) DeleteSetupResources(ctx context.Context, ns string, sessionID uint) {
	propagation := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{PropagationPolicy: &propagation}
	_ = c.kube.BatchV1().Jobs(ns).Delete(ctx, SetupJobName(sessionID), opts)
	_ = c.kube.CoreV1().ConfigMaps(ns).Delete(ctx, setupConfigMapName(sessionID), metav1.DeleteOptions{})
}
