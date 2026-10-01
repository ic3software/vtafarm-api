package k8s

import (
	"context"
	"os/exec"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func TestQuoteShellArgPreservesUntrustedDID(t *testing.T) {
	value := "did:webvh:example.com:person'; printf injected; #"
	out, err := exec.Command("sh", "-c", "printf '%s' "+quoteShellArg(value)).Output()
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != value {
		t.Fatalf("shell argument = %q, want %q", out, value)
	}
}

func TestInitialProvisionJobsSurviveRecovery(t *testing.T) {
	ctx := context.Background()
	client := &Client{kube: fake.NewSimpleClientset()}
	for _, fullStack := range []bool{false, true} {
		name := ProvisionJobName(42)
		create := func(did string) error {
			return client.CreateProvisionJob(ctx, "test", 42, "example/vta:test", did, "mobile integration", "")
		}
		if fullStack {
			name = FSJobImportAdminDid(42)
			create = func(did string) error {
				return client.CreateComponentJob(ctx, "test", ComponentJobSpec{
					Name: name, Image: "example/vta:test", Command: []string{"import", did}, RetainForRecovery: true,
				})
			}
		}
		if err := create("did:key:first"); err != nil {
			t.Fatal(err)
		}
		job, err := client.kube.BatchV1().Jobs("test").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if job.Spec.TTLSecondsAfterFinished != nil {
			t.Fatal("authorization receipt would be garbage-collected")
		}
		original := strings.Join(job.Spec.Template.Spec.Containers[0].Command, " ")
		job.Status.Succeeded = 1
		if _, err := client.kube.BatchV1().Jobs("test").UpdateStatus(ctx, job, metav1.UpdateOptions{}); err != nil {
			t.Fatal(err)
		}
		if err := create("did:key:second"); err != nil {
			t.Fatal(err)
		}
		job, err = client.kube.BatchV1().Jobs("test").Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if job.Status.Succeeded != 1 || strings.Join(job.Spec.Template.Spec.Containers[0].Command, " ") != original {
			t.Fatal("recovery recreated or changed an already completed authorization")
		}
	}
}
