package k8s

import (
	"context"
	"strings"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestRecoverDeploymentImageReplacesImageAndForcesRestart(t *testing.T) {
	deployment := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "vtc", Namespace: "test"},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "vtc", Image: "example/vtc:bad"}}},
		}},
	}
	client := &Client{kube: fake.NewSimpleClientset(deployment)}

	if err := client.RecoverDeploymentImage(context.Background(), "test", "vtc", "example/vtc:good"); err != nil {
		t.Fatalf("RecoverDeploymentImage() error = %v", err)
	}
	got, err := client.kube.AppsV1().Deployments("test").Get(context.Background(), "vtc", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	if got.Spec.Template.Spec.Containers[0].Image != "example/vtc:good" {
		t.Fatalf("image = %q", got.Spec.Template.Spec.Containers[0].Image)
	}
	if got.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("strategy = %q", got.Spec.Strategy.Type)
	}
	if got.Spec.Template.Annotations[recoveryRestartAnnotation] == "" {
		t.Fatal("recovery restart annotation is empty")
	}
}

func TestRecoverDeploymentImageAllowsMissingDeployment(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset()}
	if err := client.RecoverDeploymentImage(context.Background(), "test", "missing", "example/vtc:good"); err != nil {
		t.Fatalf("RecoverDeploymentImage() error = %v", err)
	}
}

func tuningTestDeployment(name, app string, env ...corev1.EnvVar) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "test", Labels: map[string]string{"app": app}},
		Spec: appsv1.DeploymentSpec{Template: corev1.PodTemplateSpec{
			Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: name, Image: "example/" + name + ":old", Env: env}}},
		}},
	}
}

func deploymentEnv(t *testing.T, client *Client, name string) map[string]string {
	t.Helper()
	got, err := client.kube.AppsV1().Deployments("test").Get(context.Background(), name, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	env := map[string]string{}
	for _, item := range got.Spec.Template.Spec.Containers[0].Env {
		if _, duplicate := env[item.Name]; duplicate {
			t.Fatalf("env %s is set twice", item.Name)
		}
		env[item.Name] = item.Value
	}
	return env
}

func TestSetDeploymentImageAddsMissingVtaTuningEnv(t *testing.T) {
	for _, app := range []string{"vta", "fs-vta"} {
		t.Run(app, func(t *testing.T) {
			// A Deployment created before MALLOC_ARENA_MAX joined the template,
			// with one storage value an operator tuned by hand.
			client := &Client{kube: fake.NewSimpleClientset(tuningTestDeployment("vta-7", app,
				corev1.EnvVar{Name: "NO_COLOR", Value: "1"},
				corev1.EnvVar{Name: "STORAGE_FJALL_BLOCK_CACHE", Value: "4194304"},
			))}

			if err := client.SetDeploymentImage(context.Background(), "test", "vta-7", "example/vta:new"); err != nil {
				t.Fatalf("SetDeploymentImage() error = %v", err)
			}
			env := deploymentEnv(t, client, "vta-7")
			if env["MALLOC_ARENA_MAX"] != "2" {
				t.Fatalf("MALLOC_ARENA_MAX = %q, want 2", env["MALLOC_ARENA_MAX"])
			}
			if env["STORAGE_FJALL_BLOCK_CACHE"] != "4194304" {
				t.Fatalf("hand-tuned STORAGE_FJALL_BLOCK_CACHE was overwritten: %q", env["STORAGE_FJALL_BLOCK_CACHE"])
			}
			if env["NO_COLOR"] != "1" {
				t.Fatalf("unrelated env was lost: %v", env)
			}
			for _, want := range VtaTuningEnv() {
				if _, ok := env[want.Name]; !ok {
					t.Fatalf("%s is missing after the upgrade", want.Name)
				}
			}
		})
	}
}

func TestRecoverDeploymentImageAddsMissingVtaTuningEnv(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset(tuningTestDeployment("vta-7", "vta"))}

	if err := client.RecoverDeploymentImage(context.Background(), "test", "vta-7", "example/vta:new"); err != nil {
		t.Fatalf("RecoverDeploymentImage() error = %v", err)
	}
	if env := deploymentEnv(t, client, "vta-7"); env["MALLOC_ARENA_MAX"] != "2" {
		t.Fatalf("MALLOC_ARENA_MAX = %q, want 2", env["MALLOC_ARENA_MAX"])
	}
}

func TestSetDeploymentImageMigratesMediatorToVersionIndependentVaultCA(t *testing.T) {
	deployment := tuningTestDeployment("fs-7-mediator", "fs-mediator",
		corev1.EnvVar{Name: "NO_COLOR", Value: "1"},
		corev1.EnvVar{Name: "MEDIATOR_ALLOW_INSECURE_VAULT", Value: "1"},
	)
	deployment.Spec.Template.Spec.Volumes = []corev1.Volume{{
		Name: "mediator-data",
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: "fs-7-mediator",
		}},
	}}
	deployment.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{{
		Name: "mediator-data", MountPath: "/work/mediator",
	}}
	client := &Client{kube: fake.NewSimpleClientset(deployment)}

	if err := client.SetDeploymentImage(context.Background(), "test", "fs-7-mediator", "example/mediator:new"); err != nil {
		t.Fatalf("SetDeploymentImage() error = %v", err)
	}
	env := deploymentEnv(t, client, "fs-7-mediator")
	if len(env) != 2 || env["NO_COLOR"] != "1" || env[VaultCAEnvName] != VaultCAFilePath {
		t.Fatalf("mediator env = %v", env)
	}
	got, err := client.kube.AppsV1().Deployments("test").Get(context.Background(), "fs-7-mediator", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get Deployment: %v", err)
	}
	if got.Spec.Template.Spec.Containers[0].Image != "example/mediator:new" {
		t.Fatalf("mediator image = %q", got.Spec.Template.Spec.Containers[0].Image)
	}
	if len(got.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("init containers = %#v", got.Spec.Template.Spec.InitContainers)
	}
	init := got.Spec.Template.Spec.InitContainers[0]
	if init.Name != mediatorVaultMigrationInit || init.Image != "example/mediator:new" {
		t.Fatalf("migration init container = %#v", init)
	}
	if len(init.Command) != 3 || !strings.Contains(init.Command[2], "&insecure=1") || !strings.Contains(init.Command[2], "&ca_cert=") {
		t.Fatalf("migration command = %#v", init.Command)
	}
	container := got.Spec.Template.Spec.Containers[0]
	if !hasVolumeMount(container.VolumeMounts, VaultCAVolumeName, VaultCAMountPath) {
		t.Fatalf("mediator mounts = %#v", container.VolumeMounts)
	}
	if !hasConfigMapVolume(got.Spec.Template.Spec.Volumes, VaultCAVolumeName, VaultCAConfigMapName) {
		t.Fatalf("mediator volumes = %#v", got.Spec.Template.Spec.Volumes)
	}

	if err := client.SetDeploymentImage(context.Background(), "test", "fs-7-mediator", "example/mediator:new"); err != nil {
		t.Fatalf("second SetDeploymentImage() error = %v", err)
	}
	got, _ = client.kube.AppsV1().Deployments("test").Get(context.Background(), "fs-7-mediator", metav1.GetOptions{})
	if len(got.Spec.Template.Spec.InitContainers) != 1 {
		t.Fatalf("migration init container duplicated: %#v", got.Spec.Template.Spec.InitContainers)
	}

	if err := client.RecoverDeploymentImage(context.Background(), "test", "fs-7-mediator", "example/mediator:0.52.0-6b15a89"); err != nil {
		t.Fatalf("RecoverDeploymentImage() error = %v", err)
	}
	got, _ = client.kube.AppsV1().Deployments("test").Get(context.Background(), "fs-7-mediator", metav1.GetOptions{})
	if got.Spec.Template.Spec.InitContainers[0].Image != "example/mediator:0.52.0-6b15a89" {
		t.Fatalf("rollback migration image = %q", got.Spec.Template.Spec.InitContainers[0].Image)
	}
	env = deploymentEnv(t, client, "fs-7-mediator")
	if env[VaultCAEnvName] != VaultCAFilePath {
		t.Fatalf("rollback mediator env = %v", env)
	}
}

func TestSetDeploymentImageAddsMissingDidsAndVtcTuningEnv(t *testing.T) {
	cases := []struct {
		app  string
		want []corev1.EnvVar
	}{
		{"fs-dids", DidsTuningEnv()},
		{"fs-vtc", VtcTuningEnv()},
	}
	for _, tc := range cases {
		t.Run(tc.app, func(t *testing.T) {
			// Created before MALLOC_ARENA_MAX joined the template, with one
			// budget tuned by hand.
			client := &Client{kube: fake.NewSimpleClientset(tuningTestDeployment("fs-7", tc.app,
				corev1.EnvVar{Name: "NO_COLOR", Value: "1"},
				corev1.EnvVar{Name: "STORAGE_FJALL_BLOCK_CACHE", Value: "4194304"},
			))}

			if err := client.SetDeploymentImage(context.Background(), "test", "fs-7", "example/image:new"); err != nil {
				t.Fatalf("SetDeploymentImage() error = %v", err)
			}
			env := deploymentEnv(t, client, "fs-7")
			if env["MALLOC_ARENA_MAX"] != "2" {
				t.Fatalf("MALLOC_ARENA_MAX = %q, want 2", env["MALLOC_ARENA_MAX"])
			}
			if env["STORAGE_FJALL_BLOCK_CACHE"] != "4194304" {
				t.Fatalf("hand-tuned STORAGE_FJALL_BLOCK_CACHE was overwritten: %q", env["STORAGE_FJALL_BLOCK_CACHE"])
			}
			for _, want := range tc.want {
				if want.Name == "STORAGE_FJALL_BLOCK_CACHE" {
					continue
				}
				if env[want.Name] != want.Value {
					t.Fatalf("%s = %q, want %q", want.Name, env[want.Name], want.Value)
				}
			}
			if len(env) != len(tc.want)+1 {
				t.Fatalf("unexpected env after the upgrade: %v", env)
			}
		})
	}
}

func TestSetDeploymentImageLeavesOtherComponentsEnvAlone(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset(tuningTestDeployment("fs-7-trust-registry", "fs-trust-registry",
		corev1.EnvVar{Name: "NO_COLOR", Value: "1"},
	))}
	if err := client.SetDeploymentImage(context.Background(), "test", "fs-7-trust-registry", "example/trust-registry:new"); err != nil {
		t.Fatalf("SetDeploymentImage() error = %v", err)
	}
	env := deploymentEnv(t, client, "fs-7-trust-registry")
	if len(env) != 1 || env["NO_COLOR"] != "1" {
		t.Fatalf("trust registry env changed: %v", env)
	}
}

func hasVolumeMount(mounts []corev1.VolumeMount, name, path string) bool {
	for _, mount := range mounts {
		if mount.Name == name && mount.MountPath == path && mount.ReadOnly {
			return true
		}
	}
	return false
}

func hasConfigMapVolume(volumes []corev1.Volume, name, configMap string) bool {
	for _, volume := range volumes {
		if volume.Name == name && volume.ConfigMap != nil && volume.ConfigMap.Name == configMap {
			return true
		}
	}
	return false
}
