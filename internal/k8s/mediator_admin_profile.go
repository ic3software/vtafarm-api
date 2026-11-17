package k8s

import (
	"context"
	"errors"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/utils/ptr"
)

const (
	mediatorAdminProfilePath   = "/work/mediator/conf/admin-monitor.json"
	mediatorAdminReaderMount   = "/work/mediator"
	mediatorAdminReaderSignal  = "/run/vtafarm/done"
	mediatorAdminSignalMount   = "/run/vtafarm"
	mediatorAdminReaderWait    = 2 * time.Minute
	mediatorAdminCompleteWait  = 30 * time.Second
	mediatorAdminCleanupWait   = 30 * time.Second
	mediatorAdminReaderTimeout = 300
)

type execCaptureFunc func(context.Context, string, string, string, []string) (string, error)

// ReadMediatorAdminProfile reads the owner-only profile through the Kubernetes
// exec channel. The private key never passes through a container command,
// environment variable, Kubernetes object, or Job log.
func (c *Client) ReadMediatorAdminProfile(ctx context.Context, ns string, sessionID uint, image string) (string, error) {
	return c.readMediatorAdminProfile(ctx, ns, sessionID, image, c.ExecCapture)
}

func (c *Client) readMediatorAdminProfile(
	ctx context.Context,
	ns string,
	sessionID uint,
	image string,
	execCapture execCaptureFunc,
) (profile string, err error) {
	name := FSMediatorAdminReaderName(sessionID)
	cleanupCtx, cancelCleanup := context.WithTimeout(ctx, mediatorAdminCleanupWait)
	err = c.deleteMediatorAdminReader(cleanupCtx, ns, name, "")
	cancelCleanup()
	if err != nil {
		return "", fmt.Errorf("remove stale mediator admin reader: %w", err)
	}

	job, err := c.kube.BatchV1().Jobs(ns).Create(ctx, mediatorAdminReaderJob(ns, name, FSMediatorName(sessionID), image), metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create mediator admin reader: %w", err)
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), mediatorAdminCleanupWait)
		defer cancel()
		cleanupErr := c.deleteMediatorAdminReader(cleanupCtx, ns, job.Name, job.UID)
		if cleanupErr != nil {
			profile = ""
			err = errors.Join(err, fmt.Errorf("delete mediator admin reader: %w", cleanupErr))
		}
	}()

	waitCtx, cancelWait := context.WithTimeout(ctx, mediatorAdminReaderWait)
	pod, err := c.waitForMediatorAdminReaderPod(waitCtx, ns, job.Name)
	cancelWait()
	if err != nil {
		return "", err
	}

	profile, err = execCapture(ctx, ns, pod.Name, pod.Spec.Containers[0].Name, []string{
		"sh", "-c", "cat " + mediatorAdminProfilePath + " && touch " + mediatorAdminReaderSignal,
	})
	if err != nil {
		return "", fmt.Errorf("read mediator admin profile: %w", err)
	}

	completeCtx, cancelComplete := context.WithTimeout(ctx, mediatorAdminCompleteWait)
	err = c.waitForMediatorAdminReaderCompletion(completeCtx, ns, job.Name)
	cancelComplete()
	if err != nil {
		return "", err
	}
	return profile, nil
}

func mediatorAdminReaderJob(ns, name, pvc, image string) *batchv1.Job {
	labels := map[string]string{
		"app":        "fs-mediator-admin-reader",
		"managed-by": "vtafarm",
	}
	backoff := int32(0)
	ttl := int32(mediatorAdminReaderTimeout)
	deadline := int64(mediatorAdminReaderTimeout)
	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: labels},
		Spec: batchv1.JobSpec{
			ActiveDeadlineSeconds:   &deadline,
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					AutomountServiceAccountToken:  ptr.To(false),
					EnableServiceLinks:            ptr.To(false),
					RestartPolicy:                 corev1.RestartPolicyNever,
					TerminationGracePeriodSeconds: ptr.To[int64](0),
					Containers: []corev1.Container{{
						Name:       "reader",
						Image:      image,
						Command:    []string{"sh", "-c", "while [ ! -f " + mediatorAdminReaderSignal + " ]; do sleep 1; done"},
						WorkingDir: mediatorAdminReaderMount,
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities: &corev1.Capabilities{
								Drop: []corev1.Capability{"ALL"},
							},
						},
						VolumeMounts: []corev1.VolumeMount{
							{Name: "mediator-data", MountPath: mediatorAdminReaderMount, ReadOnly: true},
							{Name: "completion", MountPath: mediatorAdminSignalMount},
						},
					}},
					Volumes: []corev1.Volume{
						{
							Name: "mediator-data",
							VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
								ClaimName: pvc,
								ReadOnly:  true,
							}},
						},
						{Name: "completion", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}},
					},
				},
			},
		},
	}
}

func (c *Client) waitForMediatorAdminReaderPod(ctx context.Context, ns, jobName string) (*corev1.Pod, error) {
	var running *corev1.Pod
	err := wait.PollUntilContextCancel(ctx, 250*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
		if err != nil {
			return false, err
		}
		for i := range pods.Items {
			pod := &pods.Items[i]
			switch pod.Status.Phase {
			case corev1.PodRunning:
				running = pod.DeepCopy()
				return true, nil
			case corev1.PodFailed, corev1.PodSucceeded:
				return false, fmt.Errorf("mediator admin reader stopped before exec (phase %s)", pod.Status.Phase)
			}
		}
		return false, nil
	})
	if err != nil {
		return nil, fmt.Errorf("wait for mediator admin reader: %w", err)
	}
	return running, nil
}

func (c *Client) waitForMediatorAdminReaderCompletion(ctx context.Context, ns, name string) error {
	err := wait.PollUntilContextCancel(ctx, 250*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		job, err := c.kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return false, err
		}
		if job.Status.Failed > 0 {
			return false, fmt.Errorf("mediator admin reader failed after reading the profile")
		}
		return job.Status.Succeeded > 0, nil
	})
	if err != nil {
		return fmt.Errorf("wait for mediator admin reader completion: %w", err)
	}
	return nil
}

func (c *Client) deleteMediatorAdminReader(ctx context.Context, ns, name string, uid types.UID) error {
	grace := int64(0)
	propagation := metav1.DeletePropagationBackground
	opts := metav1.DeleteOptions{GracePeriodSeconds: &grace, PropagationPolicy: &propagation}
	if uid != "" {
		opts.Preconditions = &metav1.Preconditions{UID: &uid}
	}
	if err := c.kube.BatchV1().Jobs(ns).Delete(ctx, name, opts); err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
	if err != nil {
		return err
	}
	for i := range pods.Items {
		pod := &pods.Items[i]
		podUID := pod.UID
		podOpts := metav1.DeleteOptions{
			GracePeriodSeconds: &grace,
			Preconditions:      &metav1.Preconditions{UID: &podUID},
		}
		if err := c.kube.CoreV1().Pods(ns).Delete(ctx, pod.Name, podOpts); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	// Remove a Pod left by the pre-Job implementation if an interrupted setup
	// is retried while this version is rolling out.
	if err := c.kube.CoreV1().Pods(ns).Delete(ctx, name, metav1.DeleteOptions{GracePeriodSeconds: &grace}); err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	return wait.PollUntilContextCancel(ctx, 100*time.Millisecond, true, func(ctx context.Context) (bool, error) {
		_, jobErr := c.kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if jobErr != nil && !apierrors.IsNotFound(jobErr) {
			return false, jobErr
		}
		remaining, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
		if err != nil {
			return false, err
		}
		_, directPodErr := c.kube.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
		if directPodErr != nil && !apierrors.IsNotFound(directPodErr) {
			return false, directPodErr
		}
		return apierrors.IsNotFound(jobErr) && len(remaining.Items) == 0 && apierrors.IsNotFound(directPodErr), nil
	})
}
