// Package config resolves the emulator's runtime configuration from
// environment variables (FABRIC_*) with flag overrides applied by cmd. The
// docker-compose contract (FABRIC_ENTRA_ISSUER, FABRIC_ENTRA_JWKS_URL,
// FABRIC_ENTRA_TLS_INSECURE) is the canonical wiring to entra-emulator.
package config

import (
	"fmt"
	"os"
	"strings"
)

// Config is the resolved emulator configuration.
type Config struct {
	// Addr is the listen address, e.g. ":9443".
	Addr string
	// DataDir holds SQLite and TLS state. Empty means in-memory DB and
	// ephemeral TLS keys.
	DataDir string

	// EntraIssuer is the exact iss expected in bearer tokens, e.g.
	// https://entra-emulator:8443/{tenant}/v2.0 — or a real Entra issuer.
	EntraIssuer string
	// EntraJWKSURL is where signing keys are fetched. Derived from
	// EntraIssuer when unset ({issuer minus /v2.0}/discovery/v2.0/keys).
	EntraJWKSURL string
	// EntraTLSInsecure skips TLS verification when fetching JWKS — for the
	// compose network where entra-emulator serves a self-signed cert.
	EntraTLSInsecure bool

	// LRODelaySeconds is the virtual time an async operation stays Running
	// before it succeeds. 0 = completes on the next poll.
	LRODelaySeconds int64
	// RetryAfterSeconds is advertised in 202 Retry-After headers.
	RetryAfterSeconds int

	// DisableTLS serves plain HTTP (useful behind a TLS-terminating proxy
	// or for curl-based exploration). Default is self-signed TLS, matching
	// entra-emulator.
	DisableTLS bool
}

// FromEnv builds a validated Config from FABRIC_* environment variables.
func FromEnv() (*Config, error) {
	c := FromEnvPartial()
	return c, c.Finish()
}

// FromEnvPartial reads the environment without validating — cmd applies flag
// overrides first, then calls Finish.
func FromEnvPartial() *Config {
	return &Config{
		Addr:              envOr("FABRIC_ADDR", ":9443"),
		DataDir:           os.Getenv("FABRIC_DATA_DIR"),
		EntraIssuer:       os.Getenv("FABRIC_ENTRA_ISSUER"),
		EntraJWKSURL:      os.Getenv("FABRIC_ENTRA_JWKS_URL"),
		EntraTLSInsecure:  boolEnv("FABRIC_ENTRA_TLS_INSECURE"),
		DisableTLS:        boolEnv("FABRIC_DISABLE_TLS"),
		RetryAfterSeconds: 1,
	}
}

// Finish validates and derives dependent fields. Call after flag overrides.
func (c *Config) Finish() error {
	if c.EntraIssuer == "" {
		return fmt.Errorf("FABRIC_ENTRA_ISSUER is required: the issuer bearer tokens must carry (an entra-emulator or real Entra v2.0 issuer URL)")
	}
	if c.EntraJWKSURL == "" {
		c.EntraJWKSURL = DeriveJWKSURL(c.EntraIssuer)
	}
	if c.RetryAfterSeconds <= 0 {
		c.RetryAfterSeconds = 1
	}
	return nil
}

// DeriveJWKSURL maps a v2.0 issuer to its JWKS endpoint using the Entra
// convention: {origin}/{tenant}/v2.0 → {origin}/{tenant}/discovery/v2.0/keys.
// Issuers not ending in /v2.0 get /discovery/v2.0/keys appended.
func DeriveJWKSURL(issuer string) string {
	base := strings.TrimSuffix(issuer, "/")
	base = strings.TrimSuffix(base, "/v2.0")
	return base + "/discovery/v2.0/keys"
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func boolEnv(key string) bool {
	switch strings.ToLower(os.Getenv(key)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}
