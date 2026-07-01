package setup

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/ic3software/vtafarm-api/internal/model"
)

// ── vta-setup.toml (full_stack variant) ─────────────────────────────────────

var fullStackVtaSetupTmpl = template.Must(template.New("fs-vta-setup").Parse(`config_path = "config.toml"
data_dir    = "data/vta"
vta_name    = "{{ .VtaName }}"
public_url  = "{{ .VtaPublicURL }}"

[services]
rest    = true
didcomm = true

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
kind      = "create_mediator"
context   = "mediator"
url       = "{{ .MediatorURL }}"
webvh_url = "{{ .MediatorWebvhURL }}"

[vta_did]
kind               = "create_webvh"
url                = "{{ .VtaDidWebvhURL }}"
portable           = {{ .Portable }}
pre_rotation_count = {{ .PreRotationCount }}
`))

type fullStackVtaSetupData struct {
	VtaName          string
	VtaPublicURL     string
	MediatorURL      string
	MediatorWebvhURL string
	VtaDidWebvhURL   string
	Portable         bool
	PreRotationCount int
	Vault            VaultSecrets
}

// RenderFullStackVtaSetupTOML renders the VTA setup recipe for full_stack
// mode (design §7) — identical [secrets] block to vta_only's
// RenderVtaSetupTOML, but [messaging] creates the in-cluster mediator
// instead of pointing at the shared external one, and [vta_did] points at
// the in-cluster dids host instead of the external DID-hosting server.
func RenderFullStackVtaSetupTOML(s *model.SetupSession, vault VaultSecrets) (string, error) {
	var buf bytes.Buffer
	err := fullStackVtaSetupTmpl.Execute(&buf, fullStackVtaSetupData{
		VtaName:          s.VtaName,
		VtaPublicURL:     "https://" + s.VtaFQDN(),
		MediatorURL:      "https://" + s.MediatorFQDN() + "/mediator/v1",
		MediatorWebvhURL: "https://" + s.DidsFQDN() + "/mediator",
		VtaDidWebvhURL:   "https://" + s.DidsFQDN() + "/vta",
		Portable:         s.Portable,
		PreRotationCount: s.PreRotationCount,
		Vault:            vault,
	})
	return buf.String(), err
}

// ── mediator-recipe.toml ─────────────────────────────────────────────────────

var mediatorRecipeTmpl = template.Must(template.New("fs-mediator-recipe").Parse(`[deployment]
type      = "server"
protocols = ["didcomm"]
use_vta   = true
vta_mode  = "sealed-export"

[vta]
context = "mediator"

[secrets]
storage = "vault://{{ .Vault.HostPort }}/{{ .Vault.KVMount }}/{{ .Vault.Prefix }}"

[security]
ssl          = "none"
admin        = "generate"
jwt_mode     = "generate"
network_mode = "open"

[database]
url = "redis://127.0.0.1/"

[storage]
backend  = "fjall"
data_dir = "./data/mediator"

[output]
config_path    = "conf/mediator.toml"
listen_address = "0.0.0.0:7037"
`))

// MediatorVaultSecrets carries the [secrets] values for the mediator's
// vault:// storage URL — token auth (VAULT_TOKEN env), not kubernetes auth
// like the VTA, so there's no k8s_role/auth_method here (design §9).
type MediatorVaultSecrets struct {
	HostPort string // e.g. vault.vault.svc:8200 (no scheme — vault:// URL form)
	KVMount  string // KV v2 mount, e.g. "secret"
	Prefix   string // mediator/user-<id>/session-<id>
}

type mediatorRecipeData struct {
	Vault MediatorVaultSecrets
}

// RenderMediatorRecipeTOML renders the mediator's setup recipe (design §7) —
// fjall message storage on the mediator PVC, secrets in Vault via the
// injected VAULT_TOKEN (not part of this recipe; see the orchestrator's Job
// env wiring).
func RenderMediatorRecipeTOML(s *model.SetupSession, vault MediatorVaultSecrets) (string, error) {
	var buf bytes.Buffer
	err := mediatorRecipeTmpl.Execute(&buf, mediatorRecipeData{Vault: vault})
	return buf.String(), err
}

// ── webvh-recipe.toml ────────────────────────────────────────────────────────

const (
	WebvhPhasePrepare  = "offline-prepare"  // phase 1 — step_dids_p1
	WebvhPhaseComplete = "offline-complete" // phase 3 — step_dids_p2
)

var webvhRecipeTmpl = template.Must(template.New("fs-webvh-recipe").Parse(`[deployment]
service  = "daemon"
vta_mode = "{{ .Phase }}"

[output]
config_path = "config.toml"

[server]
host       = "0.0.0.0"
port       = 8534
log_level  = "info"
log_format = "text"
data_dir   = "data/daemon"

[identity]
public_url   = "{{ .PublicURL }}"
mediator_did = "{{ .MediatorDid }}"

[vta]
{{- if eq .Phase "offline-prepare" }}
request_path  = "bootstrap-request.json"
{{- else }}
bundle_path   = "bundle.armor"
expect_digest = "{{ .Digest }}"
{{- end }}

[daemon]
enable_control = true
enable_server  = true
enable_witness = true
enable_watcher = false

[secrets]
backend           = "plaintext"
confirm_plaintext = true

[admin]
mode = "generate"

[reprovision]
force = false
`))

type webvhRecipeData struct {
	Phase       string
	PublicURL   string
	MediatorDid string
	Digest      string
}

// RenderWebvhRecipeTOML renders the DID-hosting daemon's setup recipe
// (design §7). phase is WebvhPhasePrepare (step_dids_p1, no digest needed)
// or WebvhPhaseComplete (step_dids_p2, digest is the 3a bundle digest from
// step_dids_provision).
func RenderWebvhRecipeTOML(s *model.SetupSession, phase, digest string) (string, error) {
	if phase != WebvhPhasePrepare && phase != WebvhPhaseComplete {
		return "", fmt.Errorf("render webvh recipe: invalid phase %q", phase)
	}
	var buf bytes.Buffer
	err := webvhRecipeTmpl.Execute(&buf, webvhRecipeData{
		Phase:       phase,
		PublicURL:   "https://" + s.DidsFQDN(),
		MediatorDid: s.MediatorDid,
		Digest:      digest,
	})
	return buf.String(), err
}
