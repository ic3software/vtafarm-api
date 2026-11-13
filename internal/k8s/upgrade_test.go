package k8s

import (
	"context"
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

func TestSetDeploymentImageLeavesOtherComponentsEnvAlone(t *testing.T) {
	client := &Client{kube: fake.NewSimpleClientset(tuningTestDeployment("fs-7-mediator", "fs-mediator",
		corev1.EnvVar{Name: "NO_COLOR", Value: "1"},
	))}

	if err := client.SetDeploymentImage(context.Background(), "test", "fs-7-mediator", "example/mediator:new"); err != nil {
		t.Fatalf("SetDeploymentImage() error = %v", err)
	}
	env := deploymentEnv(t, client, "fs-7-mediator")
	if len(env) != 1 || env["NO_COLOR"] != "1" {
		t.Fatalf("mediator env changed: %v", env)
	}
}
