package setup

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"unicode/utf8"

	"github.com/ic3software/vtafarm-api/internal/config"
	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
)

func TestRegistryJobFailurePreservesArbitraryOutput(t *testing.T) {
	const reason = "Job has reached the specified backoff limit"
	for _, output := range []string{
		"Error: no transport in common with `did:webvh:test:example.com`: it advertises [tsp, didcomm] and this agent can initiate [rest]",
		"Error: a previously unknown failure\n\nCaused by:\n    0: could not write database\n    1: disk full",
		"sh: custom-tool: not found",
		"last operation never finished",
		"Error: password authentication failed for user test",
		"Error: could not load private key from the credential file",
	} {
		got := registryJobFailure("identity", reason, "\x1b[31m"+output+"\x1b[0m\n").Error()
		want := "identity job failed (" + reason + ")\n\n--- Job Logs ---\n" + output
		if got != want {
			t.Fatalf("failure = %q, want %q", got, want)
		}
	}
	if got := registryJobFailure("identity", reason, " \n").Error(); got != "identity job failed ("+reason+")" {
		t.Fatalf("empty log fallback = %q", got)
	}
}

func TestRegistryJobFailureRedactsCredentials(t *testing.T) {
	const diagnostic = "Error: could not complete the last operation"
	for _, secretOutput := range []string{
		"privateKeyMultibase=test-key-never-persist",
		`{"private_key": "test-key-never-persist"}`,
		"Private key (multibase): test-key-never-persist",
		"VAULT_TOKEN=test-key-never-persist",
		`{"token": "test-key-never-persist"}`,
		"Authorization: Bearer test-key-never-persist",
		"database: postgres://user:test-key-never-persist@localhost/db",
		"-----BEGIN PRIVATE KEY-----\ntest-key-never-persist\n-----END PRIVATE KEY-----",
	} {
		got := registryJobFailure("identity", "failed", secretOutput+"\n"+diagnostic).Error()
		if strings.Contains(got, "test-key-never-persist") || !strings.Contains(got, diagnostic) {
			t.Fatalf("failure did not preserve the diagnostic safely: %q", got)
		}
	}
}

func TestRegistryJobFailureRetainsBoundedLogTail(t *testing.T) {
	const finalError = "Error: final operation failed"
	output := "old output\n" + strings.Repeat("測", 10000) + "\n" + finalError
	got := registryJobFailure("identity", "failed", output).Error()
	if !strings.HasSuffix(got, finalError) || strings.Contains(got, "old output") || len(got) > 17*1024 || !utf8.ValidString(got) {
		t.Fatal("failure did not retain a bounded UTF-8 log tail")
	}
}

func TestRegistryJobCapturesLatestLogsBeforeCleanup(t *testing.T) {
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	for _, scenario := range []string{"failed", "cancelled", "logs unavailable"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var created, deleted, logsRead atomic.Bool
			const output = "Error: last job failed for an unrecognized reason\nCaused by:\n    storage unavailable"
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/jobs"):
					created.Store(true)
					fmt.Fprint(w, `{"kind":"Job","apiVersion":"batch/v1","metadata":{"name":"fs-9-tr-identity"}}`)
				case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/jobs/"):
					if !created.Load() {
						w.WriteHeader(http.StatusNotFound)
						fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","reason":"NotFound","code":404}`)
					} else if scenario == "cancelled" {
						cancel()
						fmt.Fprint(w, `{"kind":"Job","apiVersion":"batch/v1","status":{"active":1}}`)
					} else {
						fmt.Fprint(w, `{"kind":"Job","apiVersion":"batch/v1","status":{"conditions":[{"type":"Failed","status":"True","message":"Job has reached the specified backoff limit"}]}}`)
					}
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/pods"):
					if r.URL.Query().Get("labelSelector") != "job-name=fs-9-tr-identity" {
						t.Error("log lookup did not target the failing Job")
					}
					if deleted.Load() {
						fmt.Fprint(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
					} else {
						fmt.Fprint(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"name":"old-attempt","creationTimestamp":"2026-01-01T00:00:00Z"}},{"metadata":{"name":"latest-attempt","creationTimestamp":"2026-01-01T00:01:00Z"}}]}`)
					}
				case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/latest-attempt/log"):
					logsRead.Store(true)
					if scenario == "logs unavailable" {
						w.WriteHeader(http.StatusForbidden)
						fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","reason":"Forbidden","code":403}`)
					} else {
						w.Header().Set("Content-Type", "text/plain")
						fmt.Fprint(w, output)
					}
				case r.Method == http.MethodDelete:
					if !logsRead.Load() {
						t.Error("Job was deleted before its logs were read")
					}
					deleted.Store(true)
					fmt.Fprint(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
				default:
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
					http.NotFound(w, r)
				}
			}))
			defer server.Close()
			kubeconfig := filepath.Join(t.TempDir(), "config")
			contents := fmt.Sprintf(`{"apiVersion":"v1","kind":"Config","clusters":[{"name":"test","cluster":{"server":%q}}],"contexts":[{"name":"test","context":{"cluster":"test","user":"test"}}],"current-context":"test","users":[{"name":"test","user":{}}]}`, server.URL)
			if err := os.WriteFile(kubeconfig, []byte(contents), 0600); err != nil {
				t.Fatal(err)
			}
			client, err := k8s.NewClient(&config.Config{K8s: config.K8sConfig{Kubeconfig: kubeconfig}})
			if err != nil {
				t.Fatal(err)
			}
			o := &Orchestrator{k8s: client}
			_, err = o.registryJob(ctx, "test", &model.SetupSession{ID: 9}, "identity", "test:latest", "/tmp", []string{"false"}, nil, nil)
			if err == nil || !logsRead.Load() {
				t.Fatalf("failure did not read the latest Job logs: %v", err)
			}
			if scenario == "logs unavailable" {
				if !strings.Contains(err.Error(), "could not read job logs") || !strings.Contains(err.Error(), "backoff limit") {
					t.Fatalf("lost log retrieval error or Job failure reason: %v", err)
				}
			} else if !strings.Contains(err.Error(), output) {
				t.Fatalf("lost the final Job output: %v", err)
			}
			if deleted.Load() != (scenario == "cancelled") {
				t.Fatal("terminal Job must remain until the caller saves its error; cancelled Job must stop")
			}
		})
	}
}
