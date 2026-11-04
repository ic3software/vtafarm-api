package k8s

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

const testActivationID = "00112233445566778899aabbccddeeff"

func TestCreateVtaDeploymentUsesHealthReadinessProbe(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset()}

	if err := client.CreateVtaDeployment(context.Background(), "test", 42, "example/vta:test", ComponentResources("10m", "32Mi", "128Mi")); err != nil {
		t.Fatalf("CreateVtaDeployment() error = %v", err)
	}

	deployment, err := client.kube.AppsV1().Deployments("test").Get(
		context.Background(), VtaDeploymentName(42), metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	if len(deployment.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("container count = %d, want 1", len(deployment.Spec.Template.Spec.Containers))
	}

	resources := deployment.Spec.Template.Spec.Containers[0].Resources
	if resources.Requests.Memory().String() != "32Mi" || resources.Limits.Memory().String() != "128Mi" {
		t.Fatalf("deployment did not use configured memory: %+v", resources)
	}
	probe := deployment.Spec.Template.Spec.Containers[0].ReadinessProbe
	if probe == nil || probe.HTTPGet == nil {
		t.Fatal("readiness probe is not an HTTP probe")
	}
	if probe.HTTPGet.Path != "/health" {
		t.Fatalf("readiness path = %q, want /health", probe.HTTPGet.Path)
	}
	if probe.HTTPGet.Port.IntVal != 8100 {
		t.Fatalf("readiness port = %d, want 8100", probe.HTTPGet.Port.IntVal)
	}
	if probe.InitialDelaySeconds != 1 || probe.PeriodSeconds != 2 ||
		probe.TimeoutSeconds != 2 || probe.FailureThreshold != 3 {
		t.Fatalf("unexpected readiness timing: %+v", probe)
	}
}

func TestCreateParkedVtaDeploymentHasNoReplicas(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset()}

	if err := client.CreateParkedVtaDeployment(context.Background(), "test", 43, "example/vta:test", ComponentResources("10m", "32Mi", "128Mi")); err != nil {
		t.Fatalf("CreateParkedVtaDeployment() error = %v", err)
	}

	deployment, err := client.kube.AppsV1().Deployments("test").Get(
		context.Background(), VtaDeploymentName(43), metav1.GetOptions{},
	)
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		t.Fatalf("replicas = %v, want 0", deployment.Spec.Replicas)
	}
}

func TestActivateParkedVtaDeploymentPatchesTemplateAndReplicasTogether(t *testing.T) {
	ctx := context.Background()
	client := &Client{kube: fake.NewSimpleClientset()}
	if err := client.CreateParkedVtaDeployment(ctx, "test", 43, "example/vta:test", ComponentResources("10m", "32Mi", "128Mi")); err != nil {
		t.Fatal(err)
	}
	spec := VtaActivationSpec{
		SessionID: 43, Image: "example/vta:test", AdminDID: "did:key:z6MkAdmin",
		AdminLabel: "mobile integration", ControlDID: "did:webvh:example.com:control", ActivationID: testActivationID,
	}
	if err := client.ActivateParkedVtaDeployment(ctx, "test", spec); err != nil {
		t.Fatal(err)
	}

	deployment, err := client.kube.AppsV1().Deployments("test").Get(ctx, VtaDeploymentName(43), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want 1", deployment.Spec.Replicas)
	}
	if deployment.Annotations[PoolActivationAnnotation] != testActivationID ||
		deployment.Spec.Template.Annotations[PoolActivationAnnotation] != testActivationID {
		t.Fatalf("activation annotations = %#v / %#v", deployment.Annotations, deployment.Spec.Template.Annotations)
	}
	if len(deployment.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("init container count = %d, want 1", len(deployment.Spec.Template.Spec.InitContainers))
	}
	init := deployment.Spec.Template.Spec.InitContainers[0]
	if init.Name != VtaActivationContainerName || init.Image != spec.Image || init.WorkingDir != "/work/vta" {
		t.Fatalf("unexpected activation container: %+v", init)
	}
	command := strings.Join(init.Command, " ")
	for _, required := range []string{
		"vta acl get", "vta import-did", "admin-imported", "vta did-mgmt servers add", "control-server-added",
	} {
		if !strings.Contains(command, required) {
			t.Errorf("activation command missing %q:\n%s", required, command)
		}
	}

	// Recovery of the same durable claim is a no-op; another claim is refused.
	if err := client.ActivateParkedVtaDeployment(ctx, "test", spec); err != nil {
		t.Fatalf("repeat activation: %v", err)
	}
	legacyRecovery := spec
	legacyRecovery.GateURL = "https://api.example.com/api/v1/internal/pool-activations/43/" + testActivationID
	if err := client.ActivateParkedVtaDeployment(ctx, "test", legacyRecovery); err != nil {
		t.Fatalf("recover already-authorized pre-gate activation: %v", err)
	}
	spec.ActivationID = "ffeeddccbbaa99887766554433221100"
	if err := client.ActivateParkedVtaDeployment(ctx, "test", spec); !errors.Is(err, ErrPoolActivationConflict) {
		t.Fatalf("different activation error = %v, want conflict", err)
	}
}

func TestPrewarmAuthorizesWithoutRollingPodTemplate(t *testing.T) {
	ctx := context.Background()
	client := &Client{kube: fake.NewSimpleClientset()}
	if err := client.CreateParkedVtaDeployment(ctx, "test", 44, "example/vta:test", ComponentResources("10m", "32Mi", "128Mi")); err != nil {
		t.Fatal(err)
	}
	spec := VtaActivationSpec{
		SessionID: 44, Image: "example/vta:test", AdminDID: "did:key:z6MkAdmin",
		AdminLabel: "mobile integration", ControlDID: "did:webvh:example.com:control",
		ActivationID: testActivationID,
		GateURL:      "https://api.example.com/api/v1/internal/pool-activations/44/" + testActivationID,
	}
	if err := client.PrewarmParkedVtaDeployment(ctx, "test", spec); err != nil {
		t.Fatal(err)
	}
	prewarmed, err := client.kube.AppsV1().Deployments("test").Get(ctx, VtaDeploymentName(44), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if prewarmed.Spec.Replicas == nil || *prewarmed.Spec.Replicas != 1 {
		t.Fatalf("replicas = %v, want 1", prewarmed.Spec.Replicas)
	}
	if prewarmed.Annotations[PoolPrewarmAnnotation] != testActivationID || prewarmed.Labels[PoolPrewarmLabel] != "true" {
		t.Fatalf("missing prewarm metadata: %#v %#v", prewarmed.Annotations, prewarmed.Labels)
	}
	if prewarmed.Annotations[PoolActivationAnnotation] != "" {
		t.Fatal("prewarm crossed the activation authorization boundary")
	}
	if len(prewarmed.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("init containers = %d, want 1", len(prewarmed.Spec.Template.Spec.InitContainers))
	}
	init := prewarmed.Spec.Template.Spec.InitContainers[0]
	command := strings.Join(init.Command, " ")
	for _, required := range []string{"curl", spec.GateURL, "vta import-did"} {
		if !strings.Contains(command, required) {
			t.Errorf("gated activation command missing %q:\n%s", required, command)
		}
	}
	if strings.Contains(command, "Authorization:") {
		t.Fatal("read-only activation gate unexpectedly requires workload credentials")
	}
	templateBefore := prewarmed.Spec.Template.DeepCopy()

	if err := client.ActivateParkedVtaDeployment(ctx, "test", spec); err != nil {
		t.Fatal(err)
	}
	authorized, err := client.kube.AppsV1().Deployments("test").Get(ctx, VtaDeploymentName(44), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if authorized.Annotations[PoolActivationAnnotation] != testActivationID {
		t.Fatal("activation authorization annotation was not committed")
	}
	if _, ok := authorized.Labels[PoolPrewarmLabel]; ok {
		t.Fatal("authorized Deployment still advertises an unconfirmed prewarm")
	}
	if !reflect.DeepEqual(*templateBefore, authorized.Spec.Template) {
		t.Fatal("authorization changed the Pod template and would trigger a rollout")
	}
	if err := client.CancelPoolPrewarm(ctx, "test", 44, testActivationID); err != nil {
		t.Fatal(err)
	}
	stillAuthorized, err := client.kube.AppsV1().Deployments("test").Get(ctx, VtaDeploymentName(44), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if stillAuthorized.Spec.Replicas == nil || *stillAuthorized.Spec.Replicas != 1 {
		t.Fatal("cancellation scaled down an authorized activation")
	}
}

func TestCancelPoolPrewarmRestoresParkedDeployment(t *testing.T) {
	ctx := context.Background()
	client := &Client{kube: fake.NewSimpleClientset()}
	if err := client.CreateParkedVtaDeployment(ctx, "test", 45, "example/vta:test", ComponentResources("10m", "32Mi", "128Mi")); err != nil {
		t.Fatal(err)
	}
	spec := VtaActivationSpec{
		SessionID: 45, Image: "example/vta:test", AdminDID: "did:key:z6MkAdmin",
		ActivationID: testActivationID,
		GateURL:      "https://api.example.com/api/v1/internal/pool-activations/45/" + testActivationID,
	}
	if err := client.PrewarmParkedVtaDeployment(ctx, "test", spec); err != nil {
		t.Fatal(err)
	}
	if err := client.CancelPoolPrewarm(ctx, "test", 45, testActivationID); err != nil {
		t.Fatal(err)
	}
	deployment, err := client.kube.AppsV1().Deployments("test").Get(ctx, VtaDeploymentName(45), metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
		t.Fatalf("replicas = %v, want 0", deployment.Spec.Replicas)
	}
	if len(deployment.Spec.Template.Spec.InitContainers) != 0 || deployment.Spec.Template.Annotations[PoolActivationAnnotation] != "" {
		t.Fatalf("parked template retained activation state: %+v", deployment.Spec.Template)
	}
}

func TestVtaActivationCommandIsValidShell(t *testing.T) {
	command, err := vtaActivationCommand(VtaActivationSpec{
		SessionID: 42, Image: "example/vta:test",
		AdminDID: "did:key:z6MkAdmin'; still-data", AdminLabel: "owner's phone",
		ControlDID: "did:webvh:example.com:control", ActivationID: testActivationID,
	})
	if err != nil {
		t.Fatal(err)
	}
	check := exec.Command("sh", "-n")
	check.Stdin = strings.NewReader(command)
	if output, err := check.CombinedOutput(); err != nil {
		t.Fatalf("activation shell syntax: %v\n%s\n%s", err, output, command)
	}
}

func TestHasProvisionJobProtectsLegacyRecovery(t *testing.T) {
	ctx := context.Background()
	client := &Client{kube: fake.NewSimpleClientset()}
	found, err := client.HasProvisionJob(ctx, "test", 42)
	if err != nil || found {
		t.Fatalf("missing job = %v, %v", found, err)
	}
	if _, err := client.kube.BatchV1().Jobs("test").Create(ctx, &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: ProvisionJobName(42), Namespace: "test"},
	}, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	found, err = client.HasProvisionJob(ctx, "test", 42)
	if err != nil || !found {
		t.Fatalf("existing job = %v, %v", found, err)
	}
}

func TestWaitForVtaActivationObservesReadyAndInitFailure(t *testing.T) {
	for _, test := range []struct {
		name       string
		deployment appsv1.DeploymentStatus
		initStatus corev1.ContainerStatus
		want       string
	}{
		{
			name:       "ready",
			deployment: appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1},
		},
		{
			name: "init failed",
			initStatus: corev1.ContainerStatus{
				Name: VtaActivationContainerName, RestartCount: activationRestartLimit,
				State:                corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}},
				LastTerminationState: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}},
			},
			want: "activation init failed",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := &Client{kube: fake.NewSimpleClientset(
				&appsv1.Deployment{
					ObjectMeta: metav1.ObjectMeta{Name: VtaDeploymentName(42), Namespace: "test", Generation: 1},
					Status:     test.deployment,
				},
				&corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{Name: "vta-42-pod", Namespace: "test", Labels: map[string]string{"app": "vta", "session-id": "42"}},
					Status:     corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{test.initStatus}},
				},
			)}
			err := client.WaitForVtaActivation(context.Background(), "test", 42, 20*time.Millisecond, nil)
			if test.want == "" && err != nil {
				t.Fatal(err)
			}
			if test.want != "" && (err == nil || !strings.Contains(err.Error(), test.want)) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestWaitForVtaActivationReportsInitStages(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset(
		&appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: VtaDeploymentName(42), Namespace: "test", Generation: 1},
			Status:     appsv1.DeploymentStatus{ObservedGeneration: 1, ReadyReplicas: 1},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "vta-42-pod", Namespace: "test", Labels: map[string]string{"app": "vta", "session-id": "42"}},
			Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
				Name:  VtaActivationContainerName,
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
			}}},
		},
	)}
	var stages []string
	if err := client.WaitForVtaActivation(context.Background(), "test", 42, time.Second, func(stage string) {
		stages = append(stages, stage)
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(stages, ",") != "activation_init_started,activation_init_completed" {
		t.Fatalf("stages = %v", stages)
	}
}
