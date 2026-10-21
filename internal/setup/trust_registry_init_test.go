package setup

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/vault"
)

func TestRegistryAvailableWithoutHelperImageConfiguration(t *testing.T) {
	o := &Orchestrator{k8s: &k8s.Client{}, vault: &vault.Client{}}
	if !o.RegistryAvailable() {
		t.Fatal("Registry must not require a separately configured helper image")
	}
	o.vault = nil
	if o.RegistryAvailable() {
		t.Fatal("Registry must still require Vault")
	}
}

func TestRegistryCredentialInitKeepsSecretsPrivateAndCleansUpFailures(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "failed read"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			bin := filepath.Join(dir, "vault")
			fakeCLI := `#!/bin/sh
set -eu
case "$1" in
write)
  test "$2" = -field=token
  test "$3" = auth/farm-auth/login
  test "$4" = role=vtafarm-user-7
  test "$5" = jwt=@/var/run/secrets/kubernetes.io/serviceaccount/token
  printf '%s' "$TEST_TOKEN"
  ;;
kv)
  test "$VAULT_TOKEN" = "$TEST_TOKEN"
  test "$2" = get
  test "$3" = -mount=secret
  test "$4" = -field=credential
  test "$5" = trust-registry/user-7/session-9/credential
  printf '%s' "$TEST_CREDENTIAL"
  test "$TEST_FAIL" = false
  ;;
token)
  test "$2" = revoke
  test "$3" = -self
  touch "$TEST_REVOKED"
  ;;
*) exit 99 ;;
esac
`
			if err := os.WriteFile(bin, []byte(fakeCLI), 0700); err != nil {
				t.Fatal(err)
			}
			outputDir := filepath.Join(dir, "credential")
			credentialFile := filepath.Join(outputDir, "credential.json")
			const token = "test-token-never-log"
			const credential = `{"did":"did:key:test","privateKeyMultibase":"test-key-never-log"}`
			failed := "false"
			if fail {
				failed = "true"
			}
			cmd := exec.Command("sh", "-ec", strings.ReplaceAll(registryCredentialInit, "/run/trust-registry", outputDir))
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"),
				"TR_SECRETS_VAULT_K8S_MOUNT=farm-auth", "TR_SECRETS_VAULT_K8S_ROLE=vtafarm-user-7",
				"TR_SECRETS_VAULT_KV_MOUNT=secret", "TR_CREDENTIAL_VAULT_PATH=trust-registry/user-7/session-9/credential",
				"TEST_TOKEN="+token, "TEST_CREDENTIAL="+credential, "TEST_FAIL="+failed, "TEST_REVOKED="+filepath.Join(dir, "revoked"))
			output, err := cmd.CombinedOutput()
			if (err != nil) != fail {
				t.Fatalf("unexpected init result: %v", err)
			}
			if strings.Contains(string(output), token) || strings.Contains(string(output), "test-key-never-log") {
				t.Fatal("init leaked credentials to container logs")
			}
			if _, err := os.Stat(filepath.Join(dir, "revoked")); err != nil {
				t.Fatal("init did not revoke its Vault token")
			}
			if _, err := os.Stat(credentialFile + ".tmp"); !os.IsNotExist(err) {
				t.Fatal("init retained temporary credential material")
			}
			if fail {
				if _, err := os.Stat(credentialFile); !os.IsNotExist(err) {
					t.Fatal("failed init published a partial credential")
				}
				return
			}
			content, err := os.ReadFile(credentialFile)
			if err != nil || string(content) != credential {
				t.Fatal("init did not write the credential")
			}
			info, err := os.Stat(credentialFile)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatal("credential permissions are not 0600")
			}
		})
	}
}
