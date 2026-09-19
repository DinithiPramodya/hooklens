// Package config loads the process's runtime configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// Config is the entire runtime configuration of the process.
//
// Everything comes from environment variables. The same binary runs in
// development and production with no build tags and no config file compiled in,
// which is what lets the Docker image we ship be the exact artifact we tested.
type Config struct {
	// Env is "dev" or "prod". Today it only selects the log format.
	Env string

	// Addr is the TCP address the HTTP server listens on, e.g. ":8080".
	Addr string

	// DatabaseURL is the Postgres connection string used by migrations and, from
	// Phase 1, by the application itself.
	DatabaseURL string

	// SweepInterval is how often expired captures are deleted. Configurable
	// mainly so tests can drive it fast.
	SweepInterval time.Duration

	// BaseDomain is the domain the app itself is served from, e.g.
	// "hooklens.dev". A single label in front of it -- "a7f3.hooklens.dev" --
	// is a capture inbox. See server.Resolve.
	BaseDomain string
}

// Load reads configuration from the environment and validates it.
//
// Defaults are chosen so that `go run ./cmd/hooklens` works on a clean machine
// with nothing exported. A config that requires setup before it will start is a
// config people work around.
func Load() (Config, error) {
	cfg := Config{
		Env:        env("HOOKLENS_ENV", "dev"),
		Addr:       env("HOOKLENS_ADDR", ":8080"),
		BaseDomain: strings.ToLower(env("HOOKLENS_BASE_DOMAIN", "localhost")),
		// Default matches compose.yaml so a fresh clone works after one
		// `docker compose up -d` with nothing exported.
		DatabaseURL:   env("DATABASE_URL", "postgres://hooklens:hooklens@localhost:5432/hooklens?sslmode=disable"),
		SweepInterval: envDuration("HOOKLENS_SWEEP_INTERVAL", sweepIntervalDefault),
	}

	// Validate at startup, not at first use. A process that boots, reports
	// healthy, and then fails on the first real request is far worse to operate
	// than one that refuses to start with a clear reason.
	if cfg.Env != "dev" && cfg.Env != "prod" {
		return Config{}, fmt.Errorf(`HOOKLENS_ENV must be "dev" or "prod", got %q`, cfg.Env)
	}
	if cfg.BaseDomain == "" {
		return Config{}, fmt.Errorf("HOOKLENS_BASE_DOMAIN must not be empty")
	}
	if strings.ContainsAny(cfg.BaseDomain, "/:") {
		return Config{}, fmt.Errorf("HOOKLENS_BASE_DOMAIN must be a bare domain without scheme or port, got %q", cfg.BaseDomain)
	}

	return cfg, nil
}

// env returns the value of key, or def when the variable is unset or empty.
//
// Empty is treated as unset on purpose: container orchestrators routinely inject
// an empty string for a variable that was never given a value, and an empty
// BaseDomain would be far more confusing than the default.
func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

const sweepIntervalDefault = 5 * time.Minute

// envDuration reads a Go duration string ("30s", "5m") or falls back.
//
// An unparseable value falls back rather than failing startup: this setting is
// an operational knob, and refusing to boot over a typo in it would trade a
// slightly-wrong sweep interval for an outage.
func envDuration(key string, def time.Duration) time.Duration {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return def
	}
	return d
}
