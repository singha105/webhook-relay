// Package config loads runtime configuration from the environment.
//
// Environment variables only, no config file. The service is deployed as a
// container into Kubernetes, where config arrives as env vars from a ConfigMap
// and secrets from a Secret. A file would mean building an image per
// environment or mounting a volume, and would give us two sources of truth.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config is the fully resolved runtime configuration.
type Config struct {
	// HTTPAddr is the listen address for the API server.
	HTTPAddr string
	// DatabaseURL is a libpq-style Postgres connection string.
	DatabaseURL string
	// ValkeyURL is unused on Day 1 — the queue is an interface with no
	// implementation yet — but is parsed and validated now so a
	// misconfiguration surfaces at boot rather than on Day 2.
	ValkeyURL string

	// LogLevel is one of debug, info, warn, error.
	LogLevel slog.Level

	// DBMaxConns bounds the pgx pool. Postgres reserves memory per backend, so
	// an unbounded pool under load exhausts the server rather than queueing.
	DBMaxConns int32
	// DBConnectTimeout bounds the initial connectivity probe at startup.
	DBConnectTimeout time.Duration

	// ShutdownTimeout is how long we let in-flight requests drain before
	// forcing the listener closed.
	ShutdownTimeout time.Duration
	// RequestTimeout bounds any single HTTP request.
	RequestTimeout time.Duration
}

// Defaults. Chosen to make `make up` work with no environment at all, which is
// what keeps a fresh clone to one command.
const (
	defaultHTTPAddr         = ":8080"
	defaultDatabaseURL      = "postgres://webhook_relay:webhook_relay@localhost:5432/webhook_relay?sslmode=disable"
	defaultValkeyURL        = "redis://localhost:6379"
	defaultLogLevel         = "info"
	defaultDBMaxConns       = 10
	defaultDBConnectTimeout = 10 * time.Second
	defaultShutdownTimeout  = 15 * time.Second
	defaultRequestTimeout   = 15 * time.Second
)

// Load reads configuration from the environment, applying defaults. It returns
// an error listing every problem at once rather than failing on the first, so
// a misconfigured deployment does not need one restart per bad variable.
func Load() (*Config, error) {
	var problems []string

	cfg := &Config{
		HTTPAddr:    envString("HTTP_ADDR", defaultHTTPAddr),
		DatabaseURL: envString("DATABASE_URL", defaultDatabaseURL),
		ValkeyURL:   envString("VALKEY_URL", defaultValkeyURL),
	}

	level, err := parseLogLevel(envString("LOG_LEVEL", defaultLogLevel))
	if err != nil {
		problems = append(problems, err.Error())
	}
	cfg.LogLevel = level

	maxConns, err := envInt("DB_MAX_CONNS", defaultDBMaxConns)
	if err != nil {
		problems = append(problems, err.Error())
	} else if maxConns < 1 {
		problems = append(problems, "DB_MAX_CONNS: must be at least 1")
	}
	cfg.DBMaxConns = int32(maxConns)

	for _, d := range []struct {
		name string
		def  time.Duration
		dst  *time.Duration
	}{
		{"DB_CONNECT_TIMEOUT", defaultDBConnectTimeout, &cfg.DBConnectTimeout},
		{"SHUTDOWN_TIMEOUT", defaultShutdownTimeout, &cfg.ShutdownTimeout},
		{"REQUEST_TIMEOUT", defaultRequestTimeout, &cfg.RequestTimeout},
	} {
		v, err := envDuration(d.name, d.def)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if v <= 0 {
			problems = append(problems, d.name+": must be positive")
			continue
		}
		*d.dst = v
	}

	if strings.TrimSpace(cfg.DatabaseURL) == "" {
		problems = append(problems, "DATABASE_URL: must not be empty")
	}

	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid configuration:\n  - %s", strings.Join(problems, "\n  - "))
	}
	return cfg, nil
}

func envString(key, def string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) (int, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", key, raw)
	}
	return n, nil
}

func envDuration(key string, def time.Duration) (time.Duration, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a duration (want e.g. 15s, 2m)", key, raw)
	}
	return d, nil
}

func parseLogLevel(raw string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("LOG_LEVEL: %q is not one of debug, info, warn, error", raw)
	}
}

// Redacted returns the config with credentials masked, safe to log at boot.
func (c *Config) Redacted() map[string]any {
	return map[string]any{
		"http_addr":        c.HTTPAddr,
		"database_url":     redactURL(c.DatabaseURL),
		"valkey_url":       redactURL(c.ValkeyURL),
		"log_level":        c.LogLevel.String(),
		"db_max_conns":     c.DBMaxConns,
		"shutdown_timeout": c.ShutdownTimeout.String(),
		"request_timeout":  c.RequestTimeout.String(),
	}
}

// redactURL strips the password from a connection URL. Connection strings are
// the single most common way a credential ends up in a log aggregator.
func redactURL(raw string) string {
	at := strings.LastIndex(raw, "@")
	if at < 0 {
		return raw
	}
	scheme := strings.Index(raw, "://")
	if scheme < 0 {
		return raw
	}
	creds := raw[scheme+3 : at]
	colon := strings.Index(creds, ":")
	if colon < 0 {
		return raw
	}
	return raw[:scheme+3] + creds[:colon] + ":***" + raw[at:]
}
