package setup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"

	"github.com/ic3software/vtafarm-api/internal/k8s"
	"github.com/ic3software/vtafarm-api/internal/model"
	"github.com/ic3software/vtafarm-api/internal/resourceprofile"
	"github.com/ic3software/vtafarm-api/internal/vault"
	"github.com/ic3software/vtafarm-api/internal/webvh"
)

const RegistryPVCSize = "200Mi"

func (o *Orchestrator) RegistryAvailable() bool {
	return o != nil && o.k8s != nil && o.vault != nil
}

func (o *Orchestrator) RegistryPhase(s *model.SetupSession, phase string) error {
	s.TrustRegistryPhase = phase
	return o.db.Model(s).Updates(map[string]any{"trust_registry_phase": phase, "updated_at": time.Now()}).Error
}

func RegistryJobName(id uint, phase string) string { return fmt.Sprintf("fs-%d-tr-%s", id, phase) }

func registryJobNames(id uint) []string {
	var names []string
	for _, phase := range []string{"preflight", "identity", "setup-key", "acl-grant", "setup", "referral", "config", "restore"} {
		names = append(names, RegistryJobName(id, phase))
	}
	return names
}

func (o *Orchestrator) CleanupFailedRegistryJobs(s *model.SetupSession) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	return o.k8s.CleanupFailedRegistryJobs(ctx, o.k8s.UserNamespace(fmt.Sprint(s.UserID)), registryJobNames(s.ID))
}

var (
	jobPrivateKeyBlock = regexp.MustCompile(`(?s)-----BEGIN [^-]*PRIVATE KEY-----.*?(?:-----END [^-]*PRIVATE KEY-----|$)`)
	jobSensitiveLine   = regexp.MustCompile(`(?i)(?:private[ _-]?key(?:multibase|hex|jwk)?(?:[ \t]*\([^\r\n)]*\))?|password|passwd|(?:client[ _-]?)?secret|secret[ _-]?id|(?:(?:access|refresh|vault|api)[ _-]?)?token|api[ _-]?key|authorization|cookie|credential)["']?[ \t]*[:=]|bearer[ \t]+[a-z0-9._~+/-]{16,}=*|://[^\s/@]+:[^\s/@]+@`)
)

func registryJobFailure(phase, reason, output string) error {
	// Keep arbitrary diagnostics, but redact credential output before bounding
	// the stored tail so truncation cannot split a private-key block.
	output = jobPrivateKeyBlock.ReplaceAllString(stripANSI(output), "[redacted private key]")
	lines := strings.Split(output, "\n")
	for i, line := range lines {
		if jobSensitiveLine.MatchString(line) {
			lines[i] = "[redacted sensitive log line]"
		}
	}
	output = strings.TrimSpace(strings.Join(lines, "\n"))
	const maxBytes = 16 * 1024
	if len(output) > maxBytes {
		output = output[len(output)-maxBytes:]
		for len(output) > 0 && !utf8.RuneStart(output[0]) {
			output = output[1:]
		}
		output = "[earlier job output truncated]\n" + output
	}
	message := fmt.Sprintf("%s job failed (%s)", phase, reason)
	if output != "" {
		message += "\n\n--- Job Logs ---\n" + output
	}
	return errors.New(message)
}

func (o *Orchestrator) registryJob(ctx context.Context, ns string, s *model.SetupSession, phase, image, dir string, command []string, mounts []k8s.PVCMount, inputs map[string][]byte) (string, error) {
	name := RegistryJobName(s.ID, phase)
	if err := o.k8s.PrepareRegistryJob(ctx, ns, name, phase == "identity" || phase == "setup-key" || phase == "acl-grant" || phase == "setup"); err != nil {
		return "", err
	}
	if err := o.k8s.CreateComponentJob(ctx, ns, k8s.ComponentJobSpec{Name: name, Image: image, Command: command, WorkingDir: dir, ServiceAccount: k8s.VtaServiceAccount, PVCMounts: mounts, SecretData: inputs, Env: fsNoColorEnv(), RetainForRecovery: true}); err != nil {
		return "", err
	}
	ok, reason, err := o.k8s.WaitForJob(ctx, ns, name)
	if err != nil || !ok {
		// A timed-out job still has useful logs; read them before cleanup using
		// a fresh context because the operation context may already be expired.
		logsCtx, cancelLogs := context.WithTimeout(context.Background(), 15*time.Second)
		output, logsErr := o.k8s.JobLogs(logsCtx, ns, name)
		cancelLogs()
		if err != nil {
			reason = err.Error()
		}
		failure := registryJobFailure(phase, reason, output)
		if logsErr != nil {
			failure = errors.Join(failure, fmt.Errorf("could not read job logs: %w", logsErr))
		}
		if err != nil {
			cleanup, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
			defer cancel()
			o.k8s.DeleteComponentJob(cleanup, ns, name)
			stopErr := o.k8s.WaitForComponentPodsGone(cleanup, ns, "job-name="+name, time.Minute)
			return "", errors.Join(failure, stopErr)
		}
		return "", failure
	}
	return o.k8s.JobLogs(ctx, ns, name)
}

func (o *Orchestrator) registryStopped(ctx context.Context, ns string, s *model.SetupSession, component string, fn func() error) (err error) {
	name := fmt.Sprintf("fs-%d-%s", s.ID, component)
	selector := fmt.Sprintf("app=fs-%s,session-id=%d", component, s.ID)
	if err = o.k8s.ScaleComponentDeployment(ctx, ns, name, 0); err != nil {
		return err
	}
	defer func() {
		restartCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
		defer cancel()
		e := o.k8s.ScaleComponentDeployment(restartCtx, ns, name, 1)
		if e == nil {
			e = o.k8s.WaitForComponentDeploymentReadyOrRestarts(restartCtx, ns, name, selector, 2*time.Minute, 3)
		}
		if e != nil {
			err = errors.Join(err, fmt.Errorf("%s did not restart: %w", component, e))
		}
	}()
	if err = o.k8s.WaitForComponentPodsGone(ctx, ns, selector, 2*time.Minute); err != nil {
		return err
	}
	return fn()
}

func (o *Orchestrator) registryVtcConfig(ctx context.Context, ns string, s *model.SetupSession) (string, error) {
	pod, container, err := o.k8s.RunningPod(ctx, ns, fmt.Sprintf("app=fs-vtc,session-id=%d", s.ID))
	if err != nil {
		return "", err
	}
	return o.k8s.ExecCapture(ctx, ns, pod, container, []string{"cat", "/work/vtc/config.toml"})
}

// EnableTrustRegistry is shared by initial setup and in-place enablement. Its
// caller owns the session maintenance claim; it never re-runs VTC setup.
func (o *Orchestrator) EnableTrustRegistry(ctx context.Context, s *model.SetupSession) error {
	if !o.RegistryAvailable() {
		return errors.New("Trust Registry requires Kubernetes and Vault")
	}
	ns := o.k8s.UserNamespace(fmt.Sprint(s.UserID))
	if err := o.k8s.FinishRegistryJobs(ctx, ns, registryJobNames(s.ID)); err != nil {
		return err
	}
	for _, component := range []string{"vta", "vtc"} {
		name := fmt.Sprintf("fs-%d-%s", s.ID, component)
		if err := o.k8s.ScaleComponentDeployment(ctx, ns, name, 1); err != nil {
			return err
		}
		if err := o.k8s.WaitForComponentDeploymentReady(ctx, ns, name, 2*time.Minute); err != nil {
			if component == "vtc" {
				restoreErr := o.restoreRegistryVtc(ns, s)
				if restoreErr == nil {
					return errors.New("interrupted VTC configuration was restored; retry registry enablement")
				}
				return errors.Join(fmt.Errorf("recover VTC before registry enablement: %w", err), restoreErr)
			}
			return fmt.Errorf("recover %s before registry enablement: %w", component, err)
		}
	}
	if err := o.RegistryPhase(s, "preflight"); err != nil {
		return err
	}
	if s.VtcDid == "" || s.VtaDid == "" || s.MediatorDid == "" {
		return errors.New("VTA, VTC and mediator identities must already exist")
	}
	current, err := o.registryVtcConfig(ctx, ns, s)
	if err != nil {
		return errors.New("cannot read existing VTC configuration")
	}
	// Validate before provisioning or stopping any existing component.
	placeholder := s.TrustRegistryDid
	if placeholder == "" {
		placeholder = "did:webvh:pending:registry:pending"
	}
	if _, err := MergeRegistryConfig(current, placeholder); err != nil {
		return err
	}
	document, err := webvh.NewResolver(webvh.DefaultTimeout).ResolveDocument(ctx, s.VtcDid)
	if err != nil {
		return fmt.Errorf("cannot validate existing VTC DID history: %w", err)
	}
	if _, _, err = RegistryReferral(document, s.VtcDid, placeholder); err != nil {
		return err
	}
	if err = o.provisionRegistry(ctx, ns, s); err != nil {
		return err
	}
	if err = webvh.NewResolver(webvh.DefaultTimeout).ResolveDID(ctx, s.TrustRegistryDid); err != nil {
		return fmt.Errorf("registry DID did not resolve: %w", err)
	}
	if err = o.RegistryPhase(s, "deploy_registry"); err != nil {
		return err
	}
	if err = o.deployTrustRegistry(ctx, ns, s); err != nil {
		return err
	}
	if err = o.RegistryPhase(s, "publish_referral"); err != nil {
		return err
	}
	// Through the running VTA, which reads its own head immediately before
	// editing; the preflight snapshot above is never applied.
	if err = o.publishRegistryReferral(ctx, ns, s); err != nil {
		return err
	}
	if err = o.RegistryPhase(s, "configure_vtc"); err != nil {
		return err
	}
	current, err = o.registryVtcConfig(ctx, ns, s)
	if err != nil {
		return err
	}
	merged, err := MergeRegistryConfig(current, s.TrustRegistryDid)
	if err != nil {
		return err
	}
	if current != merged {
		err = o.registryStopped(ctx, ns, s, "vtc", func() error {
			_, e := o.registryJob(ctx, ns, s, "config", s.VtcImage, "/work/vtc", []string{"sh", "-ec", `set -eu; umask 077
cp -p config.toml config.toml.vtafarm-registry-backup
cp -p config.toml config.toml.vtafarm-registry-next
cat /config/vtc.toml > config.toml.vtafarm-registry-next
mv config.toml.vtafarm-registry-next config.toml`}, []k8s.PVCMount{{Name: "vtc-data", ClaimName: k8s.FSVtcName(s.ID), MountPath: "/work/vtc"}}, map[string][]byte{"vtc.toml": []byte(merged)})
			return e
		})
		if err != nil {
			return errors.Join(err, o.restoreRegistryVtc(ns, s))
		}
	}
	if err = o.RegistryPhase(s, "verify"); err != nil {
		return err
	}
	document, err = webvh.NewResolver(webvh.DefaultTimeout).ResolveDocument(ctx, s.VtcDid)
	if err != nil {
		return err
	}
	if _, changed, err := RegistryReferral(document, s.VtcDid, s.TrustRegistryDid); err != nil || changed {
		return errors.New("published VTC referral has not converged; retry after DID hosting is updated")
	}
	s.TrustRegistryState = "active"
	return o.db.Model(s).Updates(map[string]any{"trust_registry_state": "active", "trust_registry_phase": "complete", "trust_registry_error": "", "updated_at": time.Now()}).Error
}

func (o *Orchestrator) restoreRegistryVtc(ns string, s *model.SetupSession) error {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	return o.registryStopped(ctx, ns, s, "vtc", func() error {
		_, err := o.registryJob(ctx, ns, s, "restore", s.VtcImage, "/work/vtc", []string{"sh", "-ec", `set -eu; test -f config.toml.vtafarm-registry-backup; cp -p config.toml.vtafarm-registry-backup config.toml`}, []k8s.PVCMount{{Name: "vtc-data", ClaimName: k8s.FSVtcName(s.ID), MountPath: "/work/vtc"}}, nil)
		return err
	})
}

func (o *Orchestrator) deployTrustRegistry(ctx context.Context, ns string, s *model.SetupSession) error {
	name := k8s.FSTrustRegistryName(s.ID)
	if err := o.k8s.CreateComponentPVC(ctx, ns, name, RegistryPVCSize); err != nil {
		return err
	}
	labels := fsLabels("trust-registry", s.ID)
	if err := o.k8s.CreateComponentService(ctx, ns, name, labels, 3232); err != nil {
		return err
	}
	values := map[string]string{
		// REST must stay on for /health. Without TR_PUBLIC_URL it adds no
		// public DID service, matching the setup recipe's DIDComm-only DID.
		"ENABLE_REST":                  "true",
		"ENABLE_DIDCOMM":               "true",
		"ENABLE_TSP":                   "false",
		"MEDIATOR_DID":                 s.MediatorDid,
		"ADMIN_DIDS":                   s.VtcDid,
		"ACL_MODE":                     "ExplicitDeny",
		"TR_VTA_CREDENTIAL":            "file://" + registryCredentialPath,
		"TR_VTA_CONTEXT_ID":            registryContextID(s.ID),
		"TR_STORAGE_BACKEND":           "fjall",
		"TR_FJALL_PATH":                "/work/trust-registry/data",
		"TR_SECRETS_VAULT_ADDR":        o.vaultAddr,
		"TR_SECRETS_VAULT_KV_MOUNT":    o.vault.KVMount(),
		"TR_SECRETS_VAULT_SECRET_PATH": vault.TrustRegistryPrefix(s.UserID, s.ID) + "/identity-cache",
		"TR_SECRETS_VAULT_SECRET_KEY":  "bundle",
		"TR_SECRETS_VAULT_AUTH_METHOD": "kubernetes",
		"TR_SECRETS_VAULT_K8S_MOUNT":   o.vault.K8sAuthMount(),
		"TR_SECRETS_VAULT_K8S_ROLE":    vault.UserName(s.UserID),
		"TR_SECRETS_VAULT_SKIP_VERIFY": "true",
		"LISTEN_ADDRESS":               "0.0.0.0:3232",
		"RUST_LOG":                     "info",
	}
	env := fsNoColorEnv()
	for key, value := range values {
		env = append(env, corev1.EnvVar{Name: key, Value: value})
	}
	profiles, err := resourceprofile.Load(ctx, o.db)
	if err != nil {
		return err
	}
	if err = o.k8s.CreateComponentDeployment(ctx, ns, k8s.ComponentDeploymentSpec{
		Name: name, Image: s.TrustRegistryImage,
		Command: []string{"trust-registry"}, WorkingDir: "/work/trust-registry",
		ServiceAccount: k8s.VtaServiceAccount, Env: env,
		Port: 3232, HealthCheckPath: "/health", Labels: labels,
		Resources: profiles.Requirements(k8s.ComponentTrustRegistry),
		PVCMounts: []k8s.PVCMount{{Name: "registry-data", ClaimName: name, MountPath: "/work/trust-registry"}},
		Recreate:  true,
	}); err != nil {
		return err
	}
	if err = o.k8s.RecoverDeploymentImage(ctx, ns, name, s.TrustRegistryImage); err != nil {
		return err
	}
	if err = o.k8s.WaitForComponentDeploymentReadyOrRestarts(ctx, ns, name, fmt.Sprintf("app=fs-trust-registry,session-id=%d", s.ID), 3*time.Minute, 3); err != nil {
		return err
	}
	// HTTP status alone does not report the independently failing write path.
	pod, container, err := o.k8s.RunningPod(ctx, ns, fmt.Sprintf("app=fs-trust-registry,session-id=%d", s.ID))
	if err != nil {
		return err
	}
	body, err := o.k8s.ExecCapture(ctx, ns, pod, container, []string{"curl", "--fail", "--silent", "--max-time", "10", "http://127.0.0.1:3232/health"})
	if err != nil {
		return errors.New("registry health request failed")
	}
	var health struct {
		Status string `json:"status"`
		Writes string `json:"writes"`
	}
	if json.Unmarshal([]byte(body), &health) != nil || health.Status != "OK" || health.Writes != "ok" {
		return errors.New("registry messaging write path is not healthy; inspect registry logs")
	}
	return nil
}

func (o *Orchestrator) TeardownTrustRegistryVault(ctx context.Context, userID, sessionID uint) {
	if o.vault != nil {
		if err := o.vault.DeleteTrustRegistrySecrets(ctx, userID, sessionID); err != nil {
			log.Printf("[orchestrator] warn: registry Vault cleanup failed for session %d: %v", sessionID, err)
		}
	}
}

func (o *Orchestrator) finishRegistrySetup(ctx context.Context, s *model.SetupSession) bool {
	if !s.TrustRegistryEnabled || s.TrustRegistryState == "active" {
		return true
	}
	if s.TrustRegistryState == "failed" {
		message := s.TrustRegistryError
		if message == "" {
			message = "Trust Registry setup failed"
		}
		o.markFailedAt(s.ID, "step_trust_registry", message)
		return false
	}
	o.fsSetStatus(s.ID, "step_trust_registry")
	if err := o.EnableTrustRegistry(ctx, s); err != nil {
		if ctx.Err() != nil {
			return false
		}
		if saveErr := o.db.Model(s).Updates(map[string]any{"trust_registry_state": "failed", "trust_registry_error": err.Error()}).Error; saveErr != nil {
			log.Printf("[orchestrator] registry failure for session %d could not be recorded; retaining Job", s.ID)
			o.markFailedAt(s.ID, "step_trust_registry", "Trust Registry setup failed and its error could not be saved")
			return false
		}
		s.TrustRegistryState, s.TrustRegistryError = "failed", err.Error()
		if cleanupErr := o.CleanupFailedRegistryJobs(s); cleanupErr != nil {
			log.Printf("[orchestrator] failed registry Job cleanup for session %d: %v", s.ID, cleanupErr)
		}
		o.markFailedAt(s.ID, "step_trust_registry", err.Error())
		return false
	}
	return true
}
