package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type Config struct {
	AppPort          string
	AppEnv           string
	JWTSecret        string
	ClusterIngressIP string
	ClusterDomain    string
	// ACMEClusterIssuer names the cert-manager ClusterIssuer that signs custom
	// domains' certificates. The same one in every environment — see
	// DefaultACMEIssuer for why there is no staging variant to pick between.
	ACMEClusterIssuer string
	// OrchestratorResume re-attaches interrupted sessions and upgrades at startup.
	// Crash recovery, so it defaults to true; false only against the shared dev
	// database, where every API would resume the same rows.
	// See docs/shared-dev-database.md.
	OrchestratorResume bool
	DB                 DBConfig
	K8s                K8sConfig
	Cloudflare         CloudflareConfig
	GHCR               GHCRConfig
	DidHosting         DidHostingConfig
	WebAuthn           WebAuthnConfig
	Vault              VaultConfig
	Monitor            MonitorConfig
}

// MonitorConfig configures the token-gated /api/v1/monitor/* endpoints polled
// by an external uptime service (UptimeRobot). Empty Token disables them.
type MonitorConfig struct {
	Token          string // shared secret, passed as ?token= by the poller
	CPUPercent     int    // per-node CPU used/allocatable alarm threshold
	MemPercent     int    // per-node memory used/allocatable alarm threshold
	StoragePercent int    // per-node Longhorn disk usage alarm threshold
	// RestartWindowMin: a container restart within this many minutes counts as
	// an alarm. Time-based (not restartCount) so alarms self-clear once stable.
	RestartWindowMin int
	// PendingGraceMin: how long a pod may sit Pending / not-Ready before it
	// alarms — headroom for normal deploys and setup churn.
	PendingGraceMin int
	// ExtraNamespaces: infra namespaces watched in addition to the
	// {K8S_NAMESPACE_PREFIX}-* user namespaces.
	ExtraNamespaces []string
}

// VaultConfig configures the farm's HashiCorp Vault. RoleID/SecretID come from
// the vtafarm-api-vault Secret created by helm/vtafarm-vault/bootstrap.sh.
type VaultConfig struct {
	Addr         string // how THIS API reaches Vault (port-forward locally, svc in-cluster)
	VTAAddr      string // address rendered into VTA pod configs — always in-cluster svc DNS
	RoleID       string // AppRole role_id (from the vtafarm-api-vault Secret)
	SecretID     string // AppRole secret_id (from the vtafarm-api-vault Secret)
	KVMount      string // KV v2 mount, default "secret"
	K8sAuthMount string // kubernetes auth mount, default "kubernetes"
	AppRoleMount string // approle auth mount, default "approle"
	SkipVerify   bool   // skip TLS verification (self-signed in-cluster CA)
}

type WebAuthnConfig struct {
	RPID          string
	RPOrigins     []string
	RPDisplayName string
}

// CookieSecure returns true when running in production (requires HTTPS).
func (c *Config) CookieSecure() bool { return c.AppEnv == "production" }

// DidHostingConfig is vtafarm-api's OWN identity for talking to a DID-hosting
// control API: a keypair from `make gen-keypair`, enrolled in that daemon's ACL
// with role=admin. It is not anything a daemon issued, so one keypair serves
// every host it is enrolled in.
//
// The URLs are deliberately absent. Which daemon to talk to is a property of the
// session — the shared one comes from the platform stack, a full_stack's from
// itself — so it is recorded on the row (setup_sessions.did_hosting_*_url) and
// resolved per use through didhosting.Factory, not fixed here at startup.
type DidHostingConfig struct {
	Did        string // did:key:z6Mk... of vtafarm-api itself
	PrivateKey string // base64 ed25519 seed
}

type CloudflareConfig struct {
	APIToken string
	ZoneID   string
}

type GHCRConfig struct {
	Token                       string // GitHub PAT — optional for public packages
	Owner                       string // e.g. "ic3software"
	PackageName                 string // e.g. "vta"
	MediatorPackageName         string // e.g. "mediator"
	DIDHostingDaemonPackageName string // e.g. "did-hosting-daemon"
	VtcPackageName              string // e.g. "vtc"
}

type DBConfig struct {
	Host     string
	Port     string
	User     string
	Password string
	Name     string
	SSLMode  string
}

// DSN returns the GORM-style connection string.
func (d *DBConfig) DSN() string {
	return fmt.Sprintf(
		"host=%s port=%s user=%s password=%s dbname=%s sslmode=%s TimeZone=UTC",
		d.Host, d.Port, d.User, d.Password, d.Name, d.SSLMode,
	)
}

// URL returns the postgres:// connection URL used by golang-migrate.
func (d *DBConfig) URL() string {
	return fmt.Sprintf(
		"postgres://%s:%s@%s:%s/%s?sslmode=%s",
		d.User, d.Password, d.Host, d.Port, d.Name, d.SSLMode,
	)
}

type K8sConfig struct {
	// Kubeconfig is the path to the kubeconfig file.
	// Empty → falls back to ~/.kube/config for out-of-cluster dev.
	Kubeconfig      string
	NamespacePrefix string
}

// DefaultACMEIssuer is the ClusterIssuer that signs custom domains, in every
// environment. See k8s/tls/clusterissuer-http01.yaml.
//
// It used to be derived from APP_ENV, picking a Let's Encrypt *staging* issuer
// outside production to keep real allowances safe. That protected the quota and
// broke the feature: a staging certificate chains to a root nothing trusts, and
// from step_vta_register_dids onward the components resolve each other's
// did:webvh identifiers over HTTPS. tls_provision passed (cert-manager only
// asks whether the certificate was issued), then the mediator crash-looped on
// what looked like a network error. A custom-domain session could not complete
// outside production at all.
//
// So every environment shares this issuer and its allowances — five
// authorization failures per hostname per hour, five certificates per identical
// set of names per week, neither raisable. The four names a domain requests never
// vary, so that weekly limit caps rebuilds per domain across all environments
// at once. ACME_CLUSTER_ISSUER overrides it where a cluster names its issuer
// something else.
const DefaultACMEIssuer = "letsencrypt-http01"

func Load() *Config {
	appEnv := getEnv("APP_ENV", "development")

	return &Config{
		AppPort:          getEnv("APP_PORT", "8080"),
		AppEnv:           appEnv,
		JWTSecret:        getEnv("JWT_SECRET", "change-me-in-production"),
		ClusterIngressIP: getEnv("CLUSTER_INGRESS_IP", ""),
		ClusterDomain:    getEnv("CLUSTER_DOMAIN", ""),

		ACMEClusterIssuer:  getEnv("ACME_CLUSTER_ISSUER", DefaultACMEIssuer),
		OrchestratorResume: getEnvBool("ORCHESTRATOR_RESUME", true),
		DB: DBConfig{
			Host:     getEnv("DB_HOST", "localhost"),
			Port:     getEnv("DB_PORT", "5432"),
			User:     getEnv("DB_USER", "postgres"),
			Password: getEnv("DB_PASSWORD", "postgres"),
			Name:     getEnv("DB_NAME", "vtafarm"),
			SSLMode:  getEnv("DB_SSLMODE", "disable"),
		},
		K8s: K8sConfig{
			Kubeconfig:      getEnv("KUBECONFIG", ""),
			NamespacePrefix: getEnv("K8S_NAMESPACE_PREFIX", "vtafarm-user"),
		},
		Cloudflare: CloudflareConfig{
			APIToken: getEnv("CLOUDFLARE_API_TOKEN", ""),
			ZoneID:   getEnv("CLOUDFLARE_ZONE_ID", ""),
		},
		GHCR: GHCRConfig{
			Token:                       getEnv("GITHUB_TOKEN", ""),
			Owner:                       getEnv("GITHUB_PACKAGE_OWNER", ""),
			PackageName:                 getEnv("GITHUB_PACKAGE_NAME", ""),
			MediatorPackageName:         getEnv("GITHUB_MEDIATOR_PACKAGE_NAME", "mediator"),
			DIDHostingDaemonPackageName: getEnv("GITHUB_DID_HOSTING_DAEMON_PACKAGE_NAME", "did-hosting-daemon"),
			VtcPackageName:              getEnv("GITHUB_VTC_PACKAGE_NAME", "vtc"),
		},
		DidHosting: DidHostingConfig{
			Did:        getEnv("DID_HOSTING_DID", ""),
			PrivateKey: getEnv("DID_HOSTING_PRIVATE_KEY", ""),
		},
		WebAuthn: WebAuthnConfig{
			RPID:          getEnv("WEBAUTHN_RP_ID", "localhost"),
			RPOrigins:     splitComma(getEnv("WEBAUTHN_RP_ORIGINS", "http://localhost:5173")),
			RPDisplayName: getEnv("WEBAUTHN_RP_DISPLAY_NAME", "VTA Farm"),
		},
		Monitor: MonitorConfig{
			Token:            getEnv("MONITOR_TOKEN", ""),
			CPUPercent:       getEnvInt("MONITOR_CPU_PCT", 90),
			MemPercent:       getEnvInt("MONITOR_MEM_PCT", 90),
			StoragePercent:   getEnvInt("MONITOR_STORAGE_PCT", 85),
			RestartWindowMin: getEnvInt("MONITOR_RESTART_WINDOW_MIN", 15),
			PendingGraceMin:  getEnvInt("MONITOR_PENDING_GRACE_MIN", 10),
			ExtraNamespaces:  splitComma(getEnv("MONITOR_EXTRA_NAMESPACES", "vault,vault-transit,longhorn-system")),
		},
		Vault: VaultConfig{
			Addr:         getEnv("VAULT_ADDR", ""),
			VTAAddr:      getEnv("VAULT_VTA_ADDR", "https://vault.vault.svc:8200"),
			RoleID:       getEnv("VAULT_ROLE_ID", ""),
			SecretID:     getEnv("VAULT_SECRET_ID", ""),
			KVMount:      getEnv("VAULT_KV_MOUNT", "secret"),
			K8sAuthMount: getEnv("VAULT_K8S_AUTH_MOUNT", "kubernetes"),
			AppRoleMount: getEnv("VAULT_APPROLE_MOUNT", "approle"),
			SkipVerify:   getEnvBool("VAULT_SKIP_VERIFY", true),
		},
	}
}

func getEnvInt(key string, defaultVal int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return defaultVal
}

func getEnvBool(key string, defaultVal bool) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes":
		return true
	case "0", "false", "no":
		return false
	default:
		return defaultVal
	}
}

func splitComma(s string) []string {
	parts := strings.Split(s, ",")
	result := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			result = append(result, v)
		}
	}
	return result
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
