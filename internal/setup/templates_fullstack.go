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
// The webvh URL paths become the DIDs' path components
// (did:webvh:<scid>:<host>:<vta_name>-vta / <vta_name>-mediator) — derived
// from the session's name, same convention as the VTC's <vtc_name>-vtc.
//
// `[messaging]` carries no `protocols` key on purpose: omitted, setup derives
// the minted mediator's transports from `[services]`, which is the only value
// that keeps the VTA's `#tsp` and the mediator it names in step.
func RenderFullStackVtaSetupTOML(s *model.SetupSession, vault VaultSecrets) (string, error) {
	var buf bytes.Buffer
	err := fullStackVtaSetupTmpl.Execute(&buf, fullStackVtaSetupData{
		VtaName:          s.VtaName,
		VtaPublicURL:     s.PublicURL(),
		MediatorURL:      "https://" + s.MediatorFQDN() + "/mediator/v1",
		MediatorWebvhURL: "https://" + s.DidsFQDN() + "/" + MediatorDidPath(s.VtaName),
		VtaDidWebvhURL:   "https://" + s.DidsFQDN() + "/" + VtaDidPath(s.VtaName),
		Portable:         s.Portable,
		PreRotationCount: s.PreRotationCount,
		Vault:            vault,
	})
	return buf.String(), err
}

// ── mediator-recipe.toml ─────────────────────────────────────────────────────

// `protocols` has no runtime effect on a prebuilt image — the mediator's TSP
// is compile-time and mediator-setup writes no TSP key into
// conf/mediator.toml. Stated because it records which image this session
// expects: one built without `--features tsp` accepts this recipe and then
// never answers the `#tsp` entry the VTA published for it.
var mediatorRecipeTmpl = template.Must(template.New("fs-mediator-recipe").Parse(`[deployment]
type      = "server"
protocols = ["didcomm", "tsp"]
use_vta   = true
vta_mode  = "sealed-export"

[vta]
context = "mediator"

[secrets]
storage = "vault://{{ .Vault.HostPort }}/{{ .Vault.KVMount }}/{{ .Vault.Prefix }}?auth=kubernetes&role={{ .Vault.K8sRole }}{{ if .Vault.SkipVerify }}&insecure=1{{ end }}"

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
// vault:// storage URL. Kubernetes auth, same mechanism as the VTA — the
// mediator binary exchanges its pod's own ServiceAccount JWT for a Vault
// token, so there's no VAULT_TOKEN to mint or inject (design §9).
type MediatorVaultSecrets struct {
	HostPort   string // e.g. vault.vault.svc:8200 (no scheme — vault:// URL form)
	KVMount    string // KV v2 mount, e.g. "secret"
	Prefix     string // mediator/user-<id>/session-<id>
	K8sRole    string // Vault kubernetes-auth role, e.g. vta-user-<id>
	SkipVerify bool   // self-signed in-cluster CA → ?insecure=1
}

type mediatorRecipeData struct {
	Vault MediatorVaultSecrets
}

// RenderMediatorRecipeTOML renders the mediator's setup recipe (design §7) —
// fjall message storage on the mediator PVC, secrets in Vault via kubernetes
// auth (the mediator's own pod ServiceAccount, same as the VTA's).
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

// `identity.transport` is written out because omitting it does not mean
// "DIDComm" — the daemon reads absent as `both`, so a TSP-carrying build has
// been advertising `TSPTransport` here all along. `both` matches the rest of
// the stack; the point is that it is now stated rather than inherited.
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
transport    = "both"

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

[secrets]                              # identical mechanism to the VTA/mediator — kubernetes auth
backend           = "vault"
vault_addr        = "{{ .Vault.Addr }}"
vault_kv_mount    = "{{ .Vault.KVMount }}"
vault_secret_path = "{{ .Vault.SecretPath }}"
vault_auth_method = "kubernetes"
vault_k8s_role    = "{{ .Vault.K8sRole }}"
vault_skip_verify = {{ .Vault.SkipVerify }}

[admin]
mode = "generate"

[reprovision]
force = false
`))

// WebvhVaultSecrets carries the [secrets] values for the dids daemon's setup
// recipe — same tagged-backend, kubernetes-auth shape as the VTA's own
// [secrets] block (design §9), not the plaintext backend the daemon used
// before.
type WebvhVaultSecrets struct {
	Addr       string // e.g. https://vault.vault.svc:8200
	KVMount    string // KV v2 mount, e.g. "secret"
	SecretPath string // dids/user-<id>/session-<id>/server-secrets
	K8sRole    string // Vault kubernetes-auth role, e.g. vta-user-<id>
	SkipVerify bool
}

type webvhRecipeData struct {
	Phase       string
	PublicURL   string
	MediatorDid string
	Digest      string
	Vault       WebvhVaultSecrets
}

// RenderWebvhRecipeTOML renders the DID-hosting daemon's setup recipe
// (design §7). phase is WebvhPhasePrepare (step_dids_p1, no digest needed)
// or WebvhPhaseComplete (step_dids_p2, digest is the 3a bundle digest from
// step_dids_provision).
func RenderWebvhRecipeTOML(s *model.SetupSession, phase, digest string, vault WebvhVaultSecrets) (string, error) {
	if phase != WebvhPhasePrepare && phase != WebvhPhaseComplete {
		return "", fmt.Errorf("render webvh recipe: invalid phase %q", phase)
	}
	var buf bytes.Buffer
	err := webvhRecipeTmpl.Execute(&buf, webvhRecipeData{
		Phase:       phase,
		PublicURL:   "https://" + s.DidsFQDN(),
		MediatorDid: s.MediatorDid,
		Digest:      digest,
		Vault:       vault,
	})
	return buf.String(), err
}
