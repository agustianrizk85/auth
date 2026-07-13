// Package config loads runtime configuration from environment variables with
// sensible defaults so the master auth service runs out of the box for local
// development. Only the database is mandatory (Postgres-only storage).
package config

import (
	"os"
	"strings"
	"time"
)

// Config holds the server runtime configuration.
type Config struct {
	Port        string        // HTTP port to listen on
	AllowOrigin string        // CORS allowed origins (comma-separated, or "*")
	DatabaseURL string        // PostgreSQL DSN (required)
	Issuer      string        // JWT "iss" claim and JWKS issuer identity
	AccessTTL   time.Duration // access-token (JWT) lifetime — keep short
	RefreshTTL  time.Duration // refresh-token lifetime — long, revocable

	PrivateKeyPath string // Ed25519 private key (PKCS#8 PEM); generated if absent
	PublicKeyPath  string // Ed25519 public key (SPKI PEM); written on generation

	// Bootstrap creates the first superadmin on an empty user table so the
	// service is usable immediately after deploy. Ignored once any user exists.
	BootstrapUsername string
	BootstrapPassword string
	BootstrapName     string

	// SeedDemo, when true, ensures two cross-department demo accounts exist:
	// "admin" (admin role in every department) and "viewer" (viewer everywhere).
	// This mirrors the per-department admin/admin123 + viewer/viewer123 accounts
	// the old standalone backends seeded, so migrated dashboards stay usable.
	SeedDemo           bool
	SeedAdminPassword  string
	SeedViewerPassword string
}

// Departments are the known business units the auth service governs. Tokens
// carry per-department roles for exactly these codes. Extend as the org grows.
var Departments = []struct{ Code, Name string }{
	{"finance", "Keuangan"},
	{"marketing", "Marketing"},
	{"digitalmarketing", "Digital Marketing"},
	{"sales", "Sales"},
	{"sdm", "SDM / HR"},
	{"perencanaan", "Perencanaan"},
	{"teknik", "Teknik"},
	{"legalpermit", "Legal & Perizinan"},
	{"cso", "CSO / Customer Complaint"},
	{"departemen", "Departemen"},
}

// Load reads configuration from the environment, applying defaults.
func Load() Config {
	return Config{
		Port:        getenv("AUTH_PORT", "8090"),
		AllowOrigin: getenv("AUTH_ALLOW_ORIGIN", "*"),
		DatabaseURL: getenv("AUTH_DATABASE_URL", ""),
		Issuer:      getenv("AUTH_ISSUER", "greenpark-auth"),
		AccessTTL:   getdur("AUTH_ACCESS_TTL", 15*time.Minute),
		RefreshTTL:  getdur("AUTH_REFRESH_TTL", 30*24*time.Hour),

		PrivateKeyPath: getenv("AUTH_PRIVATE_KEY_PATH", "keys/ed25519_private.pem"),
		PublicKeyPath:  getenv("AUTH_PUBLIC_KEY_PATH", "keys/ed25519_public.pem"),

		BootstrapUsername: getenv("AUTH_BOOTSTRAP_USERNAME", "superadmin"),
		BootstrapPassword: getenv("AUTH_BOOTSTRAP_PASSWORD", ""),
		BootstrapName:     getenv("AUTH_BOOTSTRAP_NAME", "Super Admin"),

		SeedDemo:           getbool("AUTH_SEED_DEMO", false),
		SeedAdminPassword:  getenv("AUTH_SEED_ADMIN_PASSWORD", "admin123"),
		SeedViewerPassword: getenv("AUTH_SEED_VIEWER_PASSWORD", "viewer123"),
	}
}

func getbool(key string, fallback bool) bool {
	switch os.Getenv(key) {
	case "1", "true", "TRUE", "yes", "on":
		return true
	case "0", "false", "FALSE", "no", "off":
		return false
	default:
		return fallback
	}
}

// Origins parses AllowOrigin into a slice; "*" stays as a single wildcard.
func (c Config) Origins() []string {
	out := make([]string, 0, 4)
	for _, p := range strings.Split(c.AllowOrigin, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return []string{"*"}
	}
	return out
}

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func getdur(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
