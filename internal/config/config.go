// Package config loads the process's runtime configuration from the environment.
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/DinithiPramodya/hooklens/internal/capture"
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

	// MaxBody caps how much of a captured body is kept. Configurable because
	// payload sizes are not ours to decide -- some providers legitimately send
	// megabytes -- and a limit nobody can raise is a bug for somebody.
	//
	// It is also the limit the tunnel enforces on a relayed response, so
	// raising it raises both ends together.
	MaxBody int64

	// TrustProxy says whether an X-Forwarded-For header can be believed.
	//
	// Defaults to FALSE, and the default direction matters: getting this
	// wrong one way under-counts distinct clients (a shared limit, annoying),
	// and the other way lets anyone forge their identity by setting a header
	// (no limit at all, while looking protected). Under-counting is the safe
	// failure.
	TrustProxy bool

	// RateCreate is inbox creations allowed per IP per minute.
	RateCreate float64
	// RateCapture is captures allowed per inbox per second.
	RateCapture float64

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
		MaxBody:       envBytes("HOOKLENS_MAX_BODY", capture.DefaultMaxBody),
		TrustProxy:    env("HOOKLENS_TRUST_PROXY", "") == "1",
		RateCreate:    envFloat("HOOKLENS_RATE_CREATE", 10),
		RateCapture:   envFloat("HOOKLENS_RATE_CAPTURE", 50),
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

// envBytes reads a byte count, accepting a plain number or a KB/MB suffix.
//
// "1048576" is unreadable and "1MB" is not, and a config value people get
// wrong by a factor of 1024 is a config value that will be got wrong.
func envBytes(key string, def int64) int64 {
	raw := strings.TrimSpace(strings.ToUpper(os.Getenv(key)))
	if raw == "" {
		return def
	}
	mult := int64(1)
	switch {
	case strings.HasSuffix(raw, "MB"):
		mult, raw = 1<<20, strings.TrimSuffix(raw, "MB")
	case strings.HasSuffix(raw, "KB"):
		mult, raw = 1<<10, strings.TrimSuffix(raw, "KB")
	case strings.HasSuffix(raw, "B"):
		raw = strings.TrimSuffix(raw, "B")
	}
	n, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || n <= 0 {
		// A malformed value falls back to the default rather than failing to
		// start. Debatable -- Load() rejects a bad HOOKLENS_ENV outright --
		// and chosen differently here because there is a safe default and the
		// blast radius of a typo is "captures are 1MB" rather than "the
		// routing is wrong".
		return def
	}
	return n * mult
}

// envFloat reads a positive rate, falling back on anything unusable.
func envFloat(key string, def float64) float64 {
	raw := strings.TrimSpace(os.Getenv(key))
	if raw == "" {
		return def
	}
	n, err := strconv.ParseFloat(raw, 64)
	if err != nil || n <= 0 {
		return def
	}
	return n
}
