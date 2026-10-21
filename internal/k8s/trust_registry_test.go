package k8s

import (
	"context"
	"errors"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
)

func TestFailedRegistryJobCleanupIsScopedAndPreservesRecovery(t *testing.T) {
	for _, tt := range []struct {
		name    string
		status  batchv1.JobStatus
		denied  bool
		removed bool
	}{
		{"failed", batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}}, false, true},
		{"successful receipt", batchv1.JobStatus{Succeeded: 1}, false, false},
		{"active", batchv1.JobStatus{Active: 1}, false, false},
		{"controller still retrying", batchv1.JobStatus{Failed: 1}, false, false},
		{"delete denied", batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}}, true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			kube := fake.NewSimpleClientset(
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "ns", UID: "job-uid"}, Status: tt.status},
				&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "unrelated", Namespace: "ns"}},
				&corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "pod", Namespace: "ns", Labels: map[string]string{"job-name": "job"}}},
				&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "ns"}},
				&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "ns"}},
				&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "vta-data", Namespace: "ns"}},
			)
			kube.PrependReactor("delete", "jobs", func(action ktesting.Action) (bool, runtime.Object, error) {
				if tt.denied {
					return true, nil, apierrors.NewForbidden(schema.GroupResource{Group: "batch", Resource: "jobs"}, "job", errors.New("denied"))
				}
				options := action.(ktesting.DeleteAction).GetDeleteOptions()
				if options.PropagationPolicy == nil || *options.PropagationPolicy != metav1.DeletePropagationForeground || options.Preconditions == nil || options.Preconditions.UID == nil || *options.Preconditions.UID != "job-uid" {
					t.Error("cleanup must target this exact Job and cascade to its Pods")
				}
				// Fake clients have no garbage collector; emulate the dependent Pod deletion.
				_ = kube.Tracker().Delete(corev1.SchemeGroupVersion.WithResource("pods"), "ns", "pod")
				return false, nil, nil
			})
			client := &Client{kube: kube}
			if err := client.CleanupFailedRegistryJobs(context.Background(), "ns", []string{"missing", "job"}); (err != nil) != tt.denied {
				t.Fatalf("unexpected cleanup result: %v", err)
			}
			_, jobErr := kube.BatchV1().Jobs("ns").Get(context.Background(), "job", metav1.GetOptions{})
			_, podErr := kube.CoreV1().Pods("ns").Get(context.Background(), "pod", metav1.GetOptions{})
			_, secretErr := kube.CoreV1().Secrets("ns").Get(context.Background(), "job", metav1.GetOptions{})
			_, configErr := kube.CoreV1().ConfigMaps("ns").Get(context.Background(), "job", metav1.GetOptions{})
			for _, err := range []error{jobErr, podErr, secretErr, configErr} {
				if apierrors.IsNotFound(err) != tt.removed {
					t.Fatal("incorrect retention of Job, Pod, or temporary inputs")
				}
			}
			if _, err := kube.BatchV1().Jobs("ns").Get(context.Background(), "unrelated", metav1.GetOptions{}); err != nil {
				t.Fatal("cleanup touched an unrelated Job")
			}
			if _, err := kube.CoreV1().PersistentVolumeClaims("ns").Get(context.Background(), "vta-data", metav1.GetOptions{}); err != nil {
				t.Fatal("cleanup touched persistent identity data")
			}
			if tt.removed {
				if err := client.CleanupFailedRegistryJobs(context.Background(), "ns", []string{"job"}); err != nil {
					t.Fatal("cleanup is not idempotent")
				}
			}
		})
	}
}

func TestRegistryJobRetryRetainsIdentityAndActiveJobs(t *testing.T) {
	for _, tt := range []struct {
		name    string
		keep    bool
		status  batchv1.JobStatus
		removed bool
	}{
		{"identity receipt", true, batchv1.JobStatus{Succeeded: 1}, false},
		{"reapply runtime config", false, batchv1.JobStatus{Succeeded: 1}, true},
		{"failed identity", true, batchv1.JobStatus{Failed: 1}, true},
		{"active identity", true, batchv1.JobStatus{Active: 1}, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			kube := fake.NewSimpleClientset(&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "ns"}, Status: tt.status}, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "job", Namespace: "ns"}})
			client := &Client{kube: kube}
			if err := client.PrepareRegistryJob(context.Background(), "ns", "job", tt.keep); err != nil {
				t.Fatal(err)
			}
			_, err := kube.BatchV1().Jobs("ns").Get(context.Background(), "job", metav1.GetOptions{})
			if apierrors.IsNotFound(err) != tt.removed {
				t.Fatal("wrong job retry decision")
			}
			_, err = kube.CoreV1().Secrets("ns").Get(context.Background(), "job", metav1.GetOptions{})
			if apierrors.IsNotFound(err) != tt.removed {
				t.Fatal("stale input secret was retained")
			}
		})
	}
}

func TestRegistryDeploymentKeepsCredentialInMemoryAndOutOfExports(t *testing.T) {
	kube := fake.NewSimpleClientset()
	client := &Client{kube: kube}
	spec := ComponentDeploymentSpec{Name: "registry", Labels: map[string]string{"app": "registry"}, Image: "registry:image", ServiceAccount: VtaServiceAccount, Command: []string{"trust-registry"}, Port: 3232, HealthCheckPath: "/health", Recreate: true,
		Env:            []corev1.EnvVar{{Name: "MEDIATOR_DID", Value: "did:webvh:mediator"}, {Name: "TR_VTA_CONTEXT_ID", Value: "registry-7"}, {Name: "TR_SECRETS_SEED", Value: "test-only-secret"}},
		ExtraVolumes:   []corev1.Volume{{Name: "credential", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{Medium: corev1.StorageMediumMemory}}}},
		ExtraMounts:    []corev1.VolumeMount{{Name: "credential", MountPath: "/run/trust-registry", ReadOnly: true}},
		InitContainers: []corev1.Container{{Name: "credential", Image: "hashicorp/vault:2.0.2", Command: []string{"sh", "-ec", "vault kv get"}}},
	}
	if err := client.CreateComponentDeployment(context.Background(), "ns", spec); err != nil {
		t.Fatal(err)
	}
	deployment, err := kube.AppsV1().Deployments("ns").Get(context.Background(), "registry", metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatal("registry may roll two fjall writers concurrently")
	}
	pod := deployment.Spec.Template.Spec
	if len(pod.InitContainers) != 1 || pod.Volumes[0].EmptyDir.Medium != corev1.StorageMediumMemory || !pod.Containers[0].VolumeMounts[0].ReadOnly {
		t.Fatal("credential memory mount is not isolated")
	}
	content, err := client.RegistryEnvironment(context.Background(), "ns", "app=registry")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "MEDIATOR_DID") || strings.Contains(content, "test-only-secret") {
		t.Fatal("registry export omitted references or leaked secret material")
	}
}
