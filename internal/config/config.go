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
	CookieDomain     string // e.g. ".ic3.dev" for subdomain sharing; "" for host-only
	ClusterIngressIP string
	ClusterDomain    string
	MediatorDid      string
	DB               DBConfig
	K8s              K8sConfig
	Cloudflare       CloudflareConfig
	GHCR             GHCRConfig
	DidHosting       DidHostingConfig
	WebAuthn         WebAuthnConfig
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
		CookieDomain:     getEnv("COOKIE_DOMAIN", ""),
		ClusterIngressIP: getEnv("CLUSTER_INGRESS_IP", ""),
		ClusterDomain:    getEnv("CLUSTER_DOMAIN", ""),
		MediatorDid:      getEnv("MEDIATOR_DID", ""),
		DB: DBConfig{
			Host:     getEnv("DB_HOST", "localhost"),
			Port:     getEnv("DB_PORT", "5432"),
			User:     getEnv("DB_USER", "postgres"),
			Password: getEnv("DB_PASSWORD", "postgres"),
			Name:     getEnv("DB_NAME", "cipherportal"),
			SSLMode:  getEnv("DB_SSLMODE", "disable"),
		},
		K8s: K8sConfig{
			Kubeconfig:      getEnv("KUBECONFIG", ""),
			NamespacePrefix: getEnv("K8S_NAMESPACE_PREFIX", "cp-user"),
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
			RPDisplayName: getEnv("WEBAUTHN_RP_DISPLAY_NAME", "CipherPortal"),
		},
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
