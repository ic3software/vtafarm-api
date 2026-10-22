package setup

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/pelletier/go-toml/v2"
)

const registryWorkDir = "/work/trust-registry"
const registryCredentialPath = registryWorkDir + "/vta-credential.json"

// The earlier offline flow created contexts without the one-time admin handoff.
// Use a distinct context; never repurpose or delete a legacy context's identity.
func registryContextID(id uint) string { return fmt.Sprintf("trust-registry-%d-headless", id) }

func renderRegistrySetupTOML(s *model.SetupSession) ([]byte, error) {
	return toml.Marshal(struct {
		VTADID        string   `toml:"vta_did"`
		Context       string   `toml:"context"`
		SetupKeyFile  string   `toml:"setup_key_file"`
		CredentialOut string   `toml:"credential_out"`
		EnvOut        string   `toml:"env_out"`
		Transports    []string `toml:"transports"`
		MediatorDID   string   `toml:"mediator_did"`
		WebVH         struct {
			ServerID string `toml:"server_id"`
			Path     string `toml:"path"`
		} `toml:"webvh"`
	}{
		VTADID: s.VtaDid, Context: registryContextID(s.ID),
		SetupKeyFile: "setup-key.json", CredentialOut: registryCredentialPath, EnvOut: "vta.env",
		Transports: []string{"didcomm"}, MediatorDID: s.MediatorDid,
		WebVH: struct {
			ServerID string `toml:"server_id"`
			Path     string `toml:"path"`
		}{ServerID: "dids", Path: s.VtcName + "-trust-registry"},
	})
}

func parseRegistrySetupOutput(output string, s *model.SetupSession) (string, error) {
	values := map[string]string{}
	for _, line := range strings.Split(stripANSI(output), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		switch key {
		case "registry_did", "admin_did", "vta_did", "context_id", "credential_path":
			if _, exists := values[key]; exists {
				return "", fmt.Errorf("duplicate %s in registry setup output", key)
			}
			values[key] = value
		}
	}
	if !strings.HasPrefix(values["registry_did"], "did:webvh:") || !strings.HasPrefix(values["admin_did"], "did:key:") ||
		values["vta_did"] != s.VtaDid || values["context_id"] != registryContextID(s.ID) || values["credential_path"] != registryCredentialPath {
		return "", errors.New("registry setup output is incomplete or does not match this session")
	}
	return values["registry_did"], nil
}

// Provision before VTC setup on new stacks, or during opt-in on existing stacks.
// Successful Jobs are receipts: retries reuse their key, grant and output rather
// than replacing credentials or minting another registry DID.
func (o *Orchestrator) provisionRegistry(ctx context.Context, ns string, s *model.SetupSession) error {
	if !o.RegistryAvailable() {
		return errors.New("Trust Registry requires Kubernetes and Vault")
	}
	if s.TrustRegistryDid != "" {
		output, err := o.k8s.JobLogs(ctx, ns, RegistryJobName(s.ID, "setup"))
		if err != nil {
			return fmt.Errorf("cannot verify the headless setup receipt for this Registry; reconcile its existing credential and context before retrying: %w", err)
		}
		did, err := parseRegistrySetupOutput(output, s)
		if err != nil || did != s.TrustRegistryDid {
			return errors.New("existing Registry does not match its headless setup receipt; migrate legacy credentials and context before retrying")
		}
		return nil
	}
	if s.VtaDid == "" || s.MediatorDid == "" {
		return errors.New("VTA and mediator identities must already exist")
	}
	if err := o.k8s.FinishRegistryJobs(ctx, ns, registryJobNames(s.ID)); err != nil {
		return err
	}
	if err := o.RegistryPhase(s, "preflight"); err != nil {
		return err
	}
	output, err := o.registryJob(ctx, ns, s, "preflight", s.TrustRegistryImage, "/tmp",
		[]string{"trust-registry", "setup", "--help"}, nil, nil)
	if err != nil {
		return fmt.Errorf("registry image needs headless setup built with the vta feature: %w", err)
	}
	if !strings.Contains(output, "--setup-key-out") || !strings.Contains(output, "--from") {
		return errors.New("registry image does not support the two-phase headless setup")
	}
	if err := o.vault.EnsureUserAccess(ctx, s.UserID, ns, k8s.VtaServiceAccount); err != nil {
		return err
	}
	if err := o.k8s.CreateComponentPVC(ctx, ns, k8s.FSTrustRegistryName(s.ID), RegistryPVCSize); err != nil {
		return err
	}
	mounts := []k8s.PVCMount{{Name: "registry-data", ClaimName: k8s.FSTrustRegistryName(s.ID), MountPath: registryWorkDir}}
	if err := o.RegistryPhase(s, "setup_key"); err != nil {
		return err
	}
	output, err = o.registryJob(ctx, ns, s, "setup-key", s.TrustRegistryImage, registryWorkDir,
		[]string{"trust-registry", "setup", "--setup-key-out", "setup-key.json", "--context", registryContextID(s.ID)}, mounts, nil)
	if err != nil {
		return err
	}
	setupDID, err := ParseVtcSetupKeyDid(stripANSI(output))
	if err != nil {
		return err
	}
	if err := o.RegistryPhase(s, "grant_access"); err != nil {
		return err
	}
	if err := o.registryStopped(ctx, ns, s, "vta", func() error {
		_, err := o.registryJob(ctx, ns, s, "acl-grant", s.VtaImage, "/work/vta",
			[]string{"vta", "contexts", "create", "--id", registryContextID(s.ID), "--name", "Trust Registry", "--admin-did", setupDID, "--admin-expires", "1h", "--admin-handoff"},
			[]k8s.PVCMount{{Name: "vta-data", ClaimName: k8s.FSVtaName(s.ID), MountPath: "/work/vta"}}, nil)
		return err
	}); err != nil {
		return err
	}
	recipe, err := renderRegistrySetupTOML(s)
	if err != nil {
		return err
	}
	if err := o.RegistryPhase(s, "provision_identity"); err != nil {
		return err
	}
	output, err = o.registryJob(ctx, ns, s, "setup", s.TrustRegistryImage, registryWorkDir,
		[]string{"trust-registry", "setup", "--from", "/config/tr-setup.toml"}, mounts, map[string][]byte{"tr-setup.toml": recipe})
	if err != nil {
		return err
	}
	did, err := parseRegistrySetupOutput(output, s)
	if err != nil {
		return err
	}
	if err := o.db.Model(s).Update("trust_registry_did", did).Error; err != nil {
		return err
	}
	s.TrustRegistryDid = did
	return nil
}

func (o *Orchestrator) prepareRegistryForVtc(ctx context.Context, ns string, s *model.SetupSession) error {
	if !s.TrustRegistryEnabled || s.TrustRegistryState == "failed" || s.VtcDid != "" {
		return nil
	}
	if err := o.provisionRegistry(ctx, ns, s); err != nil {
		if ctx.Err() != nil {
			return err
		}
		if saveErr := o.db.Model(s).Updates(map[string]any{"trust_registry_state": "failed", "trust_registry_error": err.Error()}).Error; saveErr != nil {
			return saveErr
		}
		s.TrustRegistryState, s.TrustRegistryError = "failed", err.Error()
		return o.CleanupFailedRegistryJobs(s)
	}
	return nil
}
