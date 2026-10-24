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
	"sync"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/testutil"
	"github.com/ic3software/vtafarm-api/internal/vault"
	"github.com/pelletier/go-toml/v2"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
)

func TestRegistryHeadlessRecipeAndCompletion(t *testing.T) {
	s := &model.SetupSession{ID: 9, VtaDid: "did:webvh:vta", MediatorDid: "did:webvh:mediator", VtcName: "community"}
	recipe, err := renderRegistrySetupTOML(s)
	if err != nil {
		t.Fatal(err)
	}
	var values map[string]any
	if err := toml.Unmarshal(recipe, &values); err != nil {
		t.Fatal(err)
	}
	for key, want := range map[string]string{
		"vta_did": s.VtaDid, "context": "trust-registry-9-headless", "setup_key_file": "setup-key.json",
		"credential_out": "/work/trust-registry/vta-credential.json", "env_out": "vta.env", "mediator_did": s.MediatorDid,
	} {
		if values[key] != want {
			t.Errorf("recipe %s = %v, want %s", key, values[key], want)
		}
	}
	if fmt.Sprint(values["transports"]) != "[didcomm]" || values["public_url"] != nil {
		t.Fatal("recipe advertises a transport without a reachable public endpoint")
	}
	webvh := values["webvh"].(map[string]any)
	if webvh["server_id"] != "dids" || webvh["path"] != "community-trust-registry" {
		t.Fatal("recipe does not use the existing registered DID host")
	}
	output := "Trust Registry setup complete.\nregistry_did=did:webvh:registry\nadmin_did=did:key:admin\nvta_did=did:webvh:vta\ncontext_id=trust-registry-9-headless\ncredential_path=/work/trust-registry/vta-credential.json\nenv_path=vta.env\n"
	if did, err := parseRegistrySetupOutput(output, s); err != nil || did != "did:webvh:registry" {
		t.Fatalf("completion: did=%q err=%v", did, err)
	}
	for _, invalid := range []string{
		strings.ReplaceAll(output, "vta_did=did:webvh:vta", "vta_did=did:webvh:other"),
		strings.ReplaceAll(output, "context_id=trust-registry-9-headless", "context_id=another-context"),
		strings.ReplaceAll(output, "credential_path=/work/trust-registry/vta-credential.json", "credential_path=/tmp/credential.json"),
		strings.ReplaceAll(output, "admin_did=did:key:admin\n", ""),
		output + "registry_did=did:webvh:other\n",
	} {
		if _, err := parseRegistrySetupOutput(invalid, s); err == nil {
			t.Fatal("accepted an incomplete or mismatched setup receipt")
		}
	}
}

func TestRegistryPreparationLeavesOptedOutAndExistingCommunitiesAlone(t *testing.T) {
	o := &Orchestrator{}
	for _, s := range []*model.SetupSession{
		{},
		{TrustRegistryEnabled: true, TrustRegistryState: "failed"},
		{TrustRegistryEnabled: true, VtcDid: "did:webvh:existing-community"},
	} {
		if err := o.prepareRegistryForVtc(context.Background(), "unused", s); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHeadlessRegistryProvisioningAndRecovery(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	for _, fails := range []bool{false, true} {
		t.Run(fmt.Sprintf("setup failure=%t", fails), func(t *testing.T) {
			db := testutil.Postgres(t)
			s := testutil.SetupSession(t, db, model.ModeFullStack)
			s.VtaDid, s.MediatorDid, s.VtcName = "did:webvh:vta", "did:webvh:mediator", "community"
			s.TrustRegistryEnabled, s.TrustRegistryState = true, "enabling"
			s.TrustRegistryImage, s.VtaImage = "registry:test", "vta:test"
			if err := db.Save(&s).Error; err != nil {
				t.Fatal(err)
			}
			var mu sync.Mutex
			jobs := map[string]*batchv1.Job{}
			creations := map[string]int{}
			var order []string
			one := int32(1)
			deployment := &appsv1.Deployment{TypeMeta: metav1.TypeMeta{APIVersion: "apps/v1", Kind: "Deployment"},
				ObjectMeta: metav1.ObjectMeta{Name: k8s.FSVtaName(s.ID)}, Spec: appsv1.DeploymentSpec{Replicas: &one}, Status: appsv1.DeploymentStatus{ReadyReplicas: 1}}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Path == "/v1/auth/approle/login":
					fmt.Fprint(w, `{"auth":{"client_token":"test-only-token","lease_duration":3600}}`)
				case strings.HasPrefix(r.URL.Path, "/v1/"):
					if strings.Contains(r.URL.Path, "/credential") {
						t.Error("Farm must not generate or transfer a Registry credential")
					}
					w.WriteHeader(http.StatusNoContent)
				case r.Method == http.MethodDelete:
					delete(jobs, filepath.Base(r.URL.Path))
					fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
				case r.Method == http.MethodPost || r.Method == http.MethodPut:
					body, _ := io.ReadAll(r.Body)
					object, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, nil)
					if err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					switch obj := object.(type) {
					case *batchv1.Job:
						if jobs[obj.Name] != nil {
							w.WriteHeader(http.StatusConflict)
							fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","reason":"AlreadyExists","code":409}`)
							return
						}
						jobs[obj.Name], creations[obj.Name] = obj, creations[obj.Name]+1
						order = append(order, obj.Name)
						if strings.HasSuffix(obj.Name, "-acl-grant") && *deployment.Spec.Replicas != 0 {
							t.Error("offline context grant ran with the VTA still running")
						}
						if strings.HasSuffix(obj.Name, "-setup") && *deployment.Spec.Replicas != 1 {
							t.Error("headless setup ran before the VTA restarted")
						}
						obj.Status = batchv1.JobStatus{Succeeded: 1, Conditions: []batchv1.JobCondition{{Type: batchv1.JobComplete, Status: corev1.ConditionTrue}}}
						if fails && strings.HasSuffix(obj.Name, "-setup") {
							obj.Status = batchv1.JobStatus{Failed: 1, Conditions: []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Message: "backoff limit"}}}
						}
					case *appsv1.Deployment:
						deployment = obj
					}
					_ = json.NewEncoder(w).Encode(object)
				case strings.Contains(r.URL.Path, "/jobs/"):
					if job := jobs[filepath.Base(r.URL.Path)]; job != nil {
						_ = json.NewEncoder(w).Encode(job)
					} else {
						w.WriteHeader(http.StatusNotFound)
						fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","reason":"NotFound","code":404}`)
					}
				case strings.Contains(r.URL.Path, "/deployments/"):
					_ = json.NewEncoder(w).Encode(deployment)
				case strings.HasSuffix(r.URL.Path, "/pods"):
					list := corev1.PodList{TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "PodList"}}
					selector := r.URL.Query().Get("labelSelector")
					if name, ok := strings.CutPrefix(selector, "job-name="); ok {
						if jobs[name] != nil {
							list.Items = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{Name: name}}}
						}
					} else if *deployment.Spec.Replicas == 1 {
						list.Items = []corev1.Pod{{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "vta"}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: "vta", Ready: true}}}}}
					}
					_ = json.NewEncoder(w).Encode(list)
				case strings.HasSuffix(r.URL.Path, "/log"):
					w.Header().Set("Content-Type", "text/plain")
					name := filepath.Base(filepath.Dir(r.URL.Path))
					switch {
					case strings.HasSuffix(name, "-preflight"):
						fmt.Fprint(w, "--setup-key-out --from")
					case strings.HasSuffix(name, "-setup-key"):
						fmt.Fprint(w, "Setup DID (ephemeral):\n  did:key:setup")
					case strings.HasSuffix(name, "-setup"):
						if fails {
							fmt.Fprint(w, "Error: headless provisioning failed at the DID host")
						} else {
							fmt.Fprintf(w, "registry_did=did:webvh:registry\nadmin_did=did:key:admin\nvta_did=%s\ncontext_id=%s\ncredential_path=%s\n", s.VtaDid, registryContextID(s.ID), registryCredentialPath)
						}
					}
				default:
					t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
				}
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "config")
			cfg := fmt.Sprintf(`{"apiVersion":"v1","kind":"Config","clusters":[{"name":"test","cluster":{"server":%q}}],"contexts":[{"name":"test","context":{"cluster":"test","user":"test"}}],"current-context":"test","users":[{"name":"test","user":{}}]}`, server.URL)
			if err := os.WriteFile(path, []byte(cfg), 0600); err != nil {
				t.Fatal(err)
			}
			client, err := k8s.NewClient(&config.Config{K8s: config.K8sConfig{Kubeconfig: path}})
			if err != nil {
				t.Fatal(err)
			}
			v, err := vault.New(vault.Config{Addr: server.URL, RoleID: "unused", SecretID: "unused"})
			if err != nil {
				t.Fatal(err)
			}
			o := &Orchestrator{db: db, k8s: client, vault: v}
			if err := o.prepareRegistryForVtc(context.Background(), "test", &s); err != nil {
				t.Fatal(err)
			}
			var stored model.SetupSession
			if err := db.First(&stored, s.ID).Error; err != nil {
				t.Fatal(err)
			}
			if fails {
				if stored.TrustRegistryState != "failed" || !strings.Contains(stored.TrustRegistryError, "headless provisioning failed") || stored.Status != s.Status || o.finishRegistrySetup(context.Background(), &s) {
					t.Fatal("Registry failure was not persisted or did not stop session completion")
				}
				if err := db.First(&stored, s.ID).Error; err != nil {
					t.Fatal(err)
				}
				if stored.Status != "failed" || stored.FailedStage != "step_trust_registry" || !strings.Contains(stored.ErrorMsg, "headless provisioning failed") {
					t.Fatal("Registry failure did not surface as a failed Trust Registry setup step")
				}
				return
			}
			if stored.TrustRegistryDid != "did:webvh:registry" || stored.VtcDid != "" {
				t.Fatal("Registry identity was not saved before VTC creation")
			}
			// Simulate a lost DB write after the setup Job completed.
			if err := db.Model(&s).Update("trust_registry_did", "").Error; err != nil {
				t.Fatal(err)
			}
			s.TrustRegistryDid = ""
			if err := o.provisionRegistry(context.Background(), "test", &s); err != nil {
				t.Fatal(err)
			}
			if err := o.provisionRegistry(context.Background(), "test", &s); err != nil {
				t.Fatalf("persisted identity could not reuse its setup receipt: %v", err)
			}
			s.TrustRegistryDid = "did:webvh:legacy"
			if err := o.provisionRegistry(context.Background(), "test", &s); err == nil {
				t.Fatal("accepted a legacy identity with a different headless credential")
			}
			mu.Lock()
			for _, phase := range []string{"setup-key", "acl-grant", "setup"} {
				job := jobs[RegistryJobName(s.ID, phase)]
				if creations[job.Name] != 1 || job.Spec.TTLSecondsAfterFinished != nil || job.Spec.Template.Spec.Containers[0].Command[0] == "sh" {
					t.Errorf("%s did not preserve a direct-CLI setup receipt", phase)
				}
			}
			grant := strings.Join(jobs[RegistryJobName(s.ID, "acl-grant")].Spec.Template.Spec.Containers[0].Command, " ")
			if !strings.Contains(grant, "--admin-did did:key:setup --admin-expires 1h --admin-handoff") || len(order) < 4 || order[1] != RegistryJobName(s.ID, "setup-key") || order[2] != RegistryJobName(s.ID, "acl-grant") || order[3] != RegistryJobName(s.ID, "setup") {
				t.Error("headless handshake order or temporary admin grant is incorrect")
			}
			delete(jobs, RegistryJobName(s.ID, "setup"))
			mu.Unlock()
			if err := o.provisionRegistry(context.Background(), "test", &s); err == nil || !strings.Contains(err.Error(), "reconcile") {
				t.Fatal("missing headless receipt did not prevent overwriting a legacy deployment")
			}
		})
	}
}
