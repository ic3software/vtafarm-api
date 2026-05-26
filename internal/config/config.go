package config

import (
	"fmt"
	"os"
)

type Config struct {
	AppPort string
	AppEnv  string
	DB      DBConfig
	K8s     K8sConfig
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
		AppPort: getEnv("APP_PORT", "8080"),
		AppEnv:  getEnv("APP_ENV", "development"),
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
	}
}

func getEnv(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}
