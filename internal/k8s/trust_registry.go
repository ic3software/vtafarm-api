package k8s

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/wait"
)

// CleanupFailedRegistryJobs leaves active work and successful identity receipts
// intact. The caller must persist the operation error before removing its Jobs.
func (c *Client) CleanupFailedRegistryJobs(ctx context.Context, ns string, names []string) error {
	for _, name := range names {
		job, err := c.kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		failed := false
		for _, condition := range job.Status.Conditions {
			failed = failed || condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue
		}
		if !failed || job.Status.Active > 0 || job.Status.Succeeded > 0 {
			continue
		}
		propagation := metav1.DeletePropagationForeground
		uid := job.UID
		err = c.kube.BatchV1().Jobs(ns).Delete(ctx, name, metav1.DeleteOptions{
			PropagationPolicy: &propagation,
			Preconditions:     &metav1.Preconditions{UID: &uid},
		})
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete failed registry Job %s: %w", name, err)
		}
		if err = wait.PollUntilContextTimeout(ctx, 500*time.Millisecond, time.Minute, true, func(ctx context.Context) (bool, error) {
			_, err := c.kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				return false, nil
			}
			if !apierrors.IsNotFound(err) {
				return false, err
			}
			pods, err := c.kube.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + name})
			return err == nil && len(pods.Items) == 0, err
		}); err != nil {
			return fmt.Errorf("wait for failed registry Job %s cleanup: %w", name, err)
		}
		if err = c.kube.CoreV1().Secrets(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		if err = c.kube.CoreV1().ConfigMaps(ns).Delete(ctx, name, metav1.DeleteOptions{}); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
	}
	return nil
}

// FinishRegistryJobs reattaches to Jobs left by an interrupted API before a
// daemon can reopen the same exclusive fjall store.
func (c *Client) FinishRegistryJobs(ctx context.Context, ns string, names []string) error {
	for _, name := range names {
		job, err := c.kube.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		if job.Status.Succeeded > 0 || job.Status.Failed > 0 && job.Status.Active == 0 {
			continue
		}
		if _, _, err = c.WaitForJob(ctx, ns, name); err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			c.DeleteComponentJob(cleanup, ns, name)
			stopErr := c.WaitForComponentPodsGone(cleanup, ns, "job-name="+name, time.Minute)
			cancel()
			if stopErr != nil {
				return fmt.Errorf("registry Job could not stop: %w", stopErr)
			}
			return err
		}
	}
	return nil
}

// RegistryEnvironment exports only the non-secret settings set by the Farm.
// The credential is supplied through a memory volume, never through this list.
func (c *Client) RegistryEnvironment(ctx context.Context, ns, selector string) (string, error) {
	deployments, err := c.kube.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return "", err
	}
	if len(deployments.Items) != 1 || len(deployments.Items[0].Spec.Template.Spec.Containers) != 1 {
		return "", fmt.Errorf("registry deployment is missing or ambiguous")
	}
	allowed := map[string]bool{"MEDIATOR_DID": true, "ADMIN_DIDS": true, "ACL_MODE": true, "LISTEN_ADDRESS": true, "ENABLE_DIDCOMM": true, "ENABLE_TSP": true, "TR_VTA_CONTEXT_ID": true, "TR_STORAGE_BACKEND": true, "TR_FJALL_PATH": true, "TR_SECRETS_VAULT_ADDR": true, "TR_SECRETS_VAULT_KV_MOUNT": true, "TR_SECRETS_VAULT_SECRET_PATH": true, "TR_SECRETS_VAULT_SECRET_KEY": true, "TR_SECRETS_VAULT_AUTH_METHOD": true, "TR_SECRETS_VAULT_K8S_ROLE": true, "TR_SECRETS_VAULT_K8S_MOUNT": true, "TR_SECRETS_VAULT_SKIP_VERIFY": true}
	var lines []string
	for _, env := range deployments.Items[0].Spec.Template.Spec.Containers[0].Env {
		if env.ValueFrom != nil {
			continue
		}
		if allowed[env.Name] {
			lines = append(lines, env.Name+"="+strconv.Quote(env.Value))
		}
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n") + "\n", nil
}
