package config

import (
	"fmt"
	"os"
	"strings"
)

type Config struct {
	AppPort          string
	AppEnv           string
	JWTSecret        string
	ClusterIngressIP string
	ClusterDomain    string
	MediatorDid      string
	DB               DBConfig
	K8s              K8sConfig
	Cloudflare       CloudflareConfig
	GHCR             GHCRConfig
	DidHosting       DidHostingConfig
	WebAuthn         WebAuthnConfig
	Vault            VaultConfig
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

type DidHostingConfig struct {
	ControlUrl string // e.g. https://control.fpp2.ic3.dev — management API (auth + upload)
	ServerUrl  string // e.g. https://dids.fpp2.ic3.dev — public DID resolution (used in vta_did_url)
	Did        string // did:key:z6Mk... of this server
	PrivateKey string // base64 ed25519 seed
}

type CloudflareConfig struct {
	APIToken string
	ZoneID   string
}

type GHCRConfig struct {
	Token       string // GitHub PAT — optional for public packages
	Owner       string // e.g. "ic3software"
	PackageName string // e.g. "vta"
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

func Load() *Config {
	return &Config{
		AppPort:          getEnv("APP_PORT", "8080"),
		AppEnv:           getEnv("APP_ENV", "development"),
		JWTSecret:        getEnv("JWT_SECRET", "change-me-in-production"),
		ClusterIngressIP: getEnv("CLUSTER_INGRESS_IP", ""),
		ClusterDomain:    getEnv("CLUSTER_DOMAIN", ""),
		MediatorDid:      getEnv("MEDIATOR_DID", ""),
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
			Token:       getEnv("GITHUB_TOKEN", ""),
			Owner:       getEnv("GITHUB_PACKAGE_OWNER", ""),
			PackageName: getEnv("GITHUB_PACKAGE_NAME", ""),
		},
		DidHosting: DidHostingConfig{
			ControlUrl: getEnv("DID_HOSTING_CONTROL_URL", ""),
			ServerUrl:  getEnv("DID_HOSTING_SERVER_URL", ""),
			Did:        getEnv("DID_HOSTING_DID", ""),
			PrivateKey: getEnv("DID_HOSTING_PRIVATE_KEY", ""),
		},
		WebAuthn: WebAuthnConfig{
			RPID:          getEnv("WEBAUTHN_RP_ID", "localhost"),
			RPOrigins:     splitComma(getEnv("WEBAUTHN_RP_ORIGINS", "http://localhost:5173")),
			RPDisplayName: getEnv("WEBAUTHN_RP_DISPLAY_NAME", "VTA Farm"),
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
