package setup

import (
	"bytes"
	"text/template"

	"github.com/ic3software/vtafarm-api/internal/model"
)

var vtaSetupTmpl = template.Must(template.New("vta-setup").Parse(`config_path = "config.toml"
data_dir    = "data/vta"
vta_name    = "{{ .VtaName }}"
public_url  = "{{ .PublicURL }}"

[services]
rest    = true
didcomm = true
tsp     = true

[server]
host = "0.0.0.0"
port = 8100

[log]
level  = "info"
format = "text"

[secrets]
backend     = "vault"
addr        = "{{ .Vault.Addr }}"
secret_path = "{{ .Vault.SecretPath }}"
kv_mount    = "{{ .Vault.KVMount }}"
secret_key  = "seed"
auth_method = "kubernetes"
k8s_role    = "{{ .Vault.K8sRole }}"
skip_verify = {{ .Vault.SkipVerify }}

[messaging]
kind = "existing"
did  = "{{ .MediatorDid }}"

[vta_did]
kind               = "create_webvh"
url                = "{{ .VtaDidUrl }}"
portable           = {{ .Portable }}
pre_rotation_count = {{ .PreRotationCount }}
`))

// VaultSecrets carries the per-session [secrets] values for the VTA config.
// The seed lives in HashiCorp Vault; the VTA pod authenticates with its
// ServiceAccount JWT via the kubernetes auth role the API provisioned.
type VaultSecrets struct {
	Addr       string // https://vault.vault.svc:8200
	SecretPath string // vta/user-<id>/session-<id>/master-seed
	KVMount    string // KV v2 mount, e.g. "secret"
	K8sRole    string // kubernetes-auth role, e.g. "vta-user-<id>"
	SkipVerify bool   // self-signed in-cluster CA → true for now
}

type vtaSetupData struct {
	VtaName          string
	PublicURL        string
	MediatorDid      string
	VtaDidUrl        string
	Portable         bool
	PreRotationCount int
	Vault            VaultSecrets
}

func RenderVtaSetupTOML(s *model.SetupSession, vault VaultSecrets) (string, error) {
	var buf bytes.Buffer
	err := vtaSetupTmpl.Execute(&buf, vtaSetupData{
		VtaName:          s.VtaName,
		PublicURL:        s.PublicURL(),
		MediatorDid:      s.MediatorDid,
		VtaDidUrl:        s.VtaDidUrl,
		Portable:         s.Portable,
		PreRotationCount: s.PreRotationCount,
		Vault:            vault,
	})
	return buf.String(), err
}
