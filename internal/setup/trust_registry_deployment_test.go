package setup

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/client-go/kubernetes/scheme"

	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/vault"
)

// Capture the actual orchestration request, stopping before pods or exec are
// needed. DryRun supplies factory resource defaults without a database server.
func TestRegistryDeploymentCarriesVTAAndWriteAuthorization(t *testing.T) {
	var deployment appsv1.Deployment
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodPost {
			t.Errorf("unexpected Kubernetes request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		object, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, nil)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/deployments") {
			deployment = *object.(*appsv1.Deployment)
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"kind":"Status","apiVersion":"v1","status":"Failure","message":"captured deployment","code":403}`))
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(object)
	}))
	defer server.Close()

	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	kubeconfig := filepath.Join(t.TempDir(), "config")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- name: test
  cluster:
    server: %s
contexts:
- name: test
  context:
    cluster: test
    user: test
current-context: test
users:
- name: test
  user: {}
`, server.URL)
	if err := os.WriteFile(kubeconfig, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	client, err := k8s.NewClient(&config.Config{K8s: config.K8sConfig{Kubeconfig: kubeconfig, NamespacePrefix: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	db, err := gorm.Open(postgres.Open("host=127.0.0.1 user=unused dbname=unused"), &gorm.Config{DisableAutomaticPing: true, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	pool, _ := db.DB()
	defer pool.Close()
	vaultClient, err := vault.New(vault.Config{Addr: server.URL, RoleID: "unused", SecretID: "unused", K8sAuthMount: "farm-auth"})
	if err != nil {
		t.Fatal(err)
	}
	o := &Orchestrator{db: db, k8s: client, vault: vaultClient, vaultAddr: server.URL}
	session := &model.SetupSession{ID: 9, UserID: 7, VtcDid: "did:webvh:community", MediatorDid: "did:webvh:mediator", TrustRegistryImage: "registry:test-build"}
	err = o.deployTrustRegistry(context.Background(), "test-7", session)
	if err == nil || !strings.Contains(err.Error(), "captured deployment") {
		t.Fatalf("did not reach deployment capture: %v", err)
	}
	pod := deployment.Spec.Template.Spec
	if len(pod.Containers) != 1 || len(pod.InitContainers) != 0 {
		t.Fatal("registry should use its setup credential without a helper init container")
	}
	env := map[string]string{}
	for _, value := range pod.Containers[0].Env {
		env[value.Name] = value.Value
	}
	for key, expected := range map[string]string{
		"TR_VTA_CONTEXT_ID":          "trust-registry-9-headless",
		"TR_VTA_CREDENTIAL":          "file:///work/trust-registry/vta-credential.json",
		"ADMIN_DIDS":                 "did:webvh:community",
		"MEDIATOR_DID":               "did:webvh:mediator",
		"ENABLE_REST":                "true",
		"ENABLE_DIDCOMM":             "true",
		"ENABLE_TSP":                 "false",
		"TR_SECRETS_VAULT_K8S_MOUNT": "farm-auth",
	} {
		if env[key] != expected {
			t.Errorf("runtime %s = %q, want %q", key, env[key], expected)
		}
	}
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatal("concurrent storage writers")
	}
	for _, mount := range pod.Containers[0].VolumeMounts {
		if mount.Name == "registry-data" && mount.MountPath == registryWorkDir {
			return
		}
	}
	t.Fatal("registry cannot read the credential written by its setup Job")
}
