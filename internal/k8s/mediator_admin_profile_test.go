package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

const testMediatorAdminProfile = `{"alias":"admin","did":"did:key:z6MkAdmin","mediator":"did:webvh:example","secrets":[{"id":"did:key:z6MkAdmin#key","type":"Multikey","privateKeyMultibase":"z3u2en7t5LR2WtQH5PfFqMqtVcSdd7ELrcFtnP63HKq4KLg"}]}`

func TestReadMediatorAdminProfileUsesPrivateEphemeralJob(t *testing.T) {
	kube := fakeMediatorAdminReaderClient(t, corev1.PodRunning)
	client := &Client{kube: kube}
	var captured *batchv1.Job

	profile, err := client.readMediatorAdminProfile(
		context.Background(), "tenant", 14, "example/mediator:0.54",
		func(_ context.Context, ns, pod, container string, command []string) (string, error) {
			if ns != "tenant" || pod != FSMediatorAdminReaderName(14)+"-pod" || container != "reader" {
				t.Fatalf("exec target = %s/%s:%s", ns, pod, container)
			}
			want := []string{"sh", "-c", "cat " + mediatorAdminProfilePath + " && touch " + mediatorAdminReaderSignal}
			if !reflect.DeepEqual(command, want) {
				t.Fatalf("exec command = %#v, want %#v", command, want)
			}
			var getErr error
			captured, getErr = kube.BatchV1().Jobs("tenant").Get(context.Background(), FSMediatorAdminReaderName(14), metav1.GetOptions{})
			if getErr != nil {
				t.Fatalf("get helper Job during exec: %v", getErr)
			}
			markMediatorAdminReaderSucceeded(t, kube, captured)
			return testMediatorAdminProfile, nil
		},
	)
	if err != nil {
		t.Fatalf("readMediatorAdminProfile() error = %v", err)
	}
	if profile != testMediatorAdminProfile {
		t.Fatal("readMediatorAdminProfile() returned a different profile")
	}
	assertSecureMediatorAdminReader(t, captured)
	assertMediatorAdminReaderGone(t, kube)
}

func TestReadMediatorAdminProfileDeletesJobAfterExecFailure(t *testing.T) {
	kube := fakeMediatorAdminReaderClient(t, corev1.PodRunning)
	client := &Client{kube: kube}

	profile, err := client.readMediatorAdminProfile(
		context.Background(), "tenant", 14, "example/mediator:0.54",
		func(context.Context, string, string, string, []string) (string, error) {
			return "", errors.New("synthetic exec failure")
		},
	)
	if err == nil || !strings.Contains(err.Error(), "read mediator admin profile") {
		t.Fatalf("readMediatorAdminProfile() error = %v", err)
	}
	if profile != "" {
		t.Fatal("readMediatorAdminProfile() returned profile after exec failure")
	}
	assertMediatorAdminReaderGone(t, kube)
}

func TestReadMediatorAdminProfileDeletesJobWhosePodStopsBeforeExec(t *testing.T) {
	kube := fakeMediatorAdminReaderClient(t, corev1.PodFailed)
	client := &Client{kube: kube}
	execCalled := false

	_, err := client.readMediatorAdminProfile(
		context.Background(), "tenant", 14, "example/mediator:0.54",
		func(context.Context, string, string, string, []string) (string, error) {
			execCalled = true
			return "", nil
		},
	)
	if err == nil || !strings.Contains(err.Error(), "stopped before exec") {
		t.Fatalf("readMediatorAdminProfile() error = %v", err)
	}
	if execCalled {
		t.Fatal("exec called for failed helper Job Pod")
	}
	assertMediatorAdminReaderGone(t, kube)
}

func fakeMediatorAdminReaderClient(t *testing.T, phase corev1.PodPhase) *fake.Clientset {
	t.Helper()
	kube := fake.NewSimpleClientset()
	kube.Fake.PrependReactor("create", "jobs", func(action k8stesting.Action) (bool, runtime.Object, error) {
		create := action.(k8stesting.CreateAction)
		job := create.GetObject().(*batchv1.Job).DeepCopy()
		job.UID = types.UID("reader-job-uid")
		if err := kube.Tracker().Create(batchv1.SchemeGroupVersion.WithResource("jobs"), job, create.GetNamespace()); err != nil {
			return true, nil, err
		}

		labels := make(map[string]string, len(job.Spec.Template.Labels)+1)
		for key, value := range job.Spec.Template.Labels {
			labels[key] = value
		}
		labels["job-name"] = job.Name
		pod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      job.Name + "-pod",
				Namespace: create.GetNamespace(),
				UID:       types.UID("reader-pod-uid"),
				Labels:    labels,
			},
			Spec:   *job.Spec.Template.Spec.DeepCopy(),
			Status: corev1.PodStatus{Phase: phase},
		}
		if err := kube.Tracker().Create(corev1.SchemeGroupVersion.WithResource("pods"), pod, create.GetNamespace()); err != nil {
			return true, nil, err
		}
		return true, job, nil
	})
	return kube
}

func markMediatorAdminReaderSucceeded(t *testing.T, kube *fake.Clientset, job *batchv1.Job) {
	t.Helper()
	completed := job.DeepCopy()
	completed.Status.Succeeded = 1
	if _, err := kube.BatchV1().Jobs(completed.Namespace).UpdateStatus(context.Background(), completed, metav1.UpdateOptions{}); err != nil {
		t.Fatalf("mark helper Job succeeded: %v", err)
	}
}

func assertSecureMediatorAdminReader(t *testing.T, job *batchv1.Job) {
	t.Helper()
	if job == nil {
		t.Fatal("helper Job was not captured")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Fatalf("helper Job backoff limit = %#v", job.Spec.BackoffLimit)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != mediatorAdminReaderTimeout {
		t.Fatalf("helper Job deadline = %#v", job.Spec.ActiveDeadlineSeconds)
	}
	if job.Spec.TTLSecondsAfterFinished == nil || *job.Spec.TTLSecondsAfterFinished != mediatorAdminReaderTimeout {
		t.Fatalf("helper Job TTL = %#v", job.Spec.TTLSecondsAfterFinished)
	}

	pod := job.Spec.Template.Spec
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Fatal("helper Job mounts a ServiceAccount token")
	}
	if len(pod.Containers) != 1 || len(pod.Volumes) != 2 {
		t.Fatalf("helper Job shape = %d containers, %d volumes", len(pod.Containers), len(pod.Volumes))
	}
	container := pod.Containers[0]
	if container.Image != "example/mediator:0.54" {
		t.Fatalf("helper image = %q", container.Image)
	}
	if len(container.VolumeMounts) != 2 || !container.VolumeMounts[0].ReadOnly || container.VolumeMounts[0].MountPath != mediatorAdminReaderMount {
		t.Fatalf("helper volume mounts = %#v", container.VolumeMounts)
	}
	pvc := pod.Volumes[0].PersistentVolumeClaim
	if pvc == nil || !pvc.ReadOnly || pvc.ClaimName != FSMediatorName(14) {
		t.Fatalf("helper PVC = %#v", pvc)
	}
	if pod.Volumes[1].EmptyDir == nil || pod.Volumes[1].EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Fatalf("helper completion volume = %#v", pod.Volumes[1])
	}
	if container.SecurityContext == nil || container.SecurityContext.AllowPrivilegeEscalation == nil || *container.SecurityContext.AllowPrivilegeEscalation {
		t.Fatalf("helper security context = %#v", container.SecurityContext)
	}
	encoded, err := json.Marshal(job)
	if err != nil {
		t.Fatalf("marshal helper Job: %v", err)
	}
	if strings.Contains(string(encoded), "z3u2en7t5LR2WtQH5PfFqMqtVcSdd7ELrcFtnP63HKq4KLg") {
		t.Fatal("private key leaked into helper Job object")
	}
}

func assertMediatorAdminReaderGone(t *testing.T, kube *fake.Clientset) {
	t.Helper()
	name := FSMediatorAdminReaderName(14)
	if _, err := kube.BatchV1().Jobs("tenant").Get(context.Background(), name, metav1.GetOptions{}); !apierrors.IsNotFound(err) {
		t.Fatalf("helper Job remains: %v", err)
	}
	pods, err := kube.CoreV1().Pods("tenant").List(context.Background(), metav1.ListOptions{LabelSelector: "job-name=" + name})
	if err != nil {
		t.Fatalf("list helper Job Pods: %v", err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("helper Job Pods remain: %#v", pods.Items)
	}
}
