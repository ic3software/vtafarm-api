package setup

import (
	"bytes"
	"text/template"

	"github.com/ic3software/vtafarm-api/internal/model"
)

// ── vtc-setup.toml ───────────────────────────────────────────────────────────

// Renders the schema vtc-service's `vtc setup --from` deserializes
// (VtcWizardInputs — design §9). [secrets].backend explicitly selects the
// Vault backend (matching the VTA's own setup TOML shape) — it wins
// outright over the legacy "whichever field is set" resolution and fails
// closed on a mismatch, rather than relying on vault_addr's mere presence
// to activate Vault implicitly.
//
// [webvh].server_id = "dids" matches the `--id dids` registered by
// full_stack's step_vta_register_dids; path pins the DID's path component to
// <vtc_name>-vtc (did:webvh:<scid>:<host>:<vtc_name>-vtc) instead of letting
// the daemon auto-assign a random one — same name-based convention as the
// VTA's <vta_name>-vta and mediator's <vta_name>-mediator paths, and the
// -vtc suffix keeps it distinct from those even if vtc_name == vta_name.
// domain is left unset (the daemon resolves its default). setup_key_file is
// relative, resolved against the Job's /work/vtc workingDir where
// step_vtc_setup_key wrote it. vault_secret_key = "bundle" stores the
// serialized VtcKeyBundle — vti_secrets' seed store is byte-agnostic.
var vtcSetupTmpl = template.Must(template.New("fs-vtc-setup").Parse(`config_path    = "config.toml"
base_url       = "{{ .VtcPublicURL }}"
vta_did        = "{{ .VtaDid }}"
context        = "{{ .VtcName }}"
setup_key_file = "setup-key.json"

[webvh]
server_id = "dids"
path      = "{{ .VtcDidPath }}"

[messaging]
mediator_did = "{{ .MediatorDid }}"
mediator_url = "{{ .MediatorURL }}"
transports   = ["tsp", "didcomm"]

[secrets]
backend           = "vault"
vault_addr        = "{{ .Vault.Addr }}"
vault_kv_mount    = "{{ .Vault.KVMount }}"
vault_secret_path = "{{ .Vault.SecretPath }}"
vault_secret_key  = "bundle"
vault_auth_method = "kubernetes"
vault_k8s_role    = "{{ .Vault.K8sRole }}"
vault_skip_verify = {{ .Vault.SkipVerify }}
`))

// VtcVaultSecrets carries the [secrets] values for the VTC's setup TOML —
// kubernetes auth via the same per-user role the other three components use
// (design §10), just with the VTC's implicit-selection field shape.
type VtcVaultSecrets struct {
	Addr       string // e.g. https://vault.vault.svc:8200
	KVMount    string // KV v2 mount, e.g. "secret"
	SecretPath string // vtc/user-<id>/session-<id>/key-bundle
	K8sRole    string // Vault kubernetes-auth role, e.g. vta-user-<id>
	SkipVerify bool
}

type vtcSetupData struct {
	VtcPublicURL string
	VtaDid       string
	VtcName      string
	VtcDidPath   string
	MediatorDid  string
	MediatorURL  string
	Vault        VtcVaultSecrets
}

// RenderVtcSetupTOML renders the VTC's non-interactive setup recipe
// (design §9), wiring it to the session's own VTA (1a), mediator (1b), and —
// via the registered "dids" server id — its own DID-hosting daemon.
// mediator_url is informational only; the endpoint is resolved from the
// mediator's DID document.
func RenderVtcSetupTOML(s *model.SetupSession, vault VtcVaultSecrets) (string, error) {
	var buf bytes.Buffer
	err := vtcSetupTmpl.Execute(&buf, vtcSetupData{
		VtcPublicURL: "https://" + s.VtcFQDN(),
		VtaDid:       s.VtaDid,
		VtcName:      s.VtcName,
		VtcDidPath:   VtcDidPath(s.VtcName),
		MediatorDid:  s.MediatorDid,
		MediatorURL:  "https://" + s.MediatorFQDN() + "/mediator/v1",
		Vault:        vault,
	})
	return buf.String(), err
}
