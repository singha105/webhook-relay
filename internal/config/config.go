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

	// --- delivery (day 2) ---

	// WorkerConcurrency is the number of delivery goroutines per process.
	WorkerConcurrency int
	// DeliveryTimeout bounds one outbound webhook call.
	DeliveryTimeout time.Duration
	// MaxAttempts is the total delivery attempts before the dead letter queue.
	MaxAttempts int
	// RetryBaseDelay and RetryMaxDelay bound the full-jitter backoff window.
	RetryBaseDelay time.Duration
	RetryMaxDelay  time.Duration
	// StaleClaimTimeout is how long a queue entry may be held unacknowledged
	// before another worker may reclaim it.
	StaleClaimTimeout time.Duration
	// RelayPollInterval is how often the outbox relay looks for due events.
	RelayPollInterval time.Duration
	// RelayBatchSize bounds one relay claim.
	RelayBatchSize int
	// DeliveryLease is how long a claimed event may sit in 'delivering' before
	// it is presumed abandoned.
	DeliveryLease time.Duration

	// DeliveryDedupEnabled guards against dispatching the same (event,
	// attempt) twice after a message is reclaimed.
	//
	// Switchable OFF on purpose. Day 5 turns it off to demonstrate the
	// duplicate deliveries it prevents — a safety control that is never
	// observed failing is indistinguishable from one that does nothing.
	DeliveryDedupEnabled bool
	// DeliveryDedupTTL is how long a dispatch marker lives.
	DeliveryDedupTTL time.Duration
}

// Defaults. Chosen to make `make up` work with no environment at all, which is
// what keeps a fresh clone to one command.
const (
	defaultHTTPAddr = ":8080"
	// Matches the credentials the local compose stack creates. This is a
	// development default, not a secret: the real value arrives from a
	// Kubernetes Secret via DATABASE_URL.
	//nolint:gosec // G101: not a credential, a local-only default.
	defaultDatabaseURL      = "postgres://webhook_relay:webhook_relay@localhost:5432/webhook_relay?sslmode=disable"
	defaultValkeyURL        = "redis://localhost:6379"
	defaultLogLevel         = "info"
	defaultDBMaxConns       = 10
	defaultDBConnectTimeout = 10 * time.Second
	defaultShutdownTimeout  = 15 * time.Second
	defaultRequestTimeout   = 15 * time.Second

	defaultWorkerConcurrency = 10
	defaultDeliveryTimeout   = 10 * time.Second
	defaultMaxAttempts       = 6
	defaultRetryBaseDelay    = time.Second
	defaultRetryMaxDelay     = time.Hour
	defaultStaleClaimTimeout = 60 * time.Second
	defaultRelayPollInterval = 250 * time.Millisecond
	defaultRelayBatchSize    = 100
	defaultDeliveryLease     = 5 * time.Minute
	defaultDedupTTL          = 15 * time.Minute

	// maxDBConns caps the pool. Postgres allocates memory per backend, so an
	// unbounded pool exhausts the server rather than queueing at the client.
	// It also keeps the value inside int32, which is what pgxpool takes.
	maxDBConns = 10000
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

	// Bounded on both sides before the int32 conversion. pgxpool takes an
	// int32, so an unchecked cast of a large value would silently wrap to a
	// negative pool size.
	maxConns, err := envInt("DB_MAX_CONNS", defaultDBMaxConns)
	switch {
	case err != nil:
		problems = append(problems, err.Error())
	case maxConns < 1:
		problems = append(problems, "DB_MAX_CONNS: must be at least 1")
	case maxConns > maxDBConns:
		problems = append(problems, fmt.Sprintf("DB_MAX_CONNS: must be at most %d", maxDBConns))
	default:
		cfg.DBMaxConns = int32(maxConns)
	}

	for _, n := range []struct {
		name string
		def  int
		dst  *int
		min  int
		max  int
	}{
		{"WORKER_CONCURRENCY", defaultWorkerConcurrency, &cfg.WorkerConcurrency, 1, 1000},
		{"MAX_ATTEMPTS", defaultMaxAttempts, &cfg.MaxAttempts, 1, 100},
		{"RELAY_BATCH_SIZE", defaultRelayBatchSize, &cfg.RelayBatchSize, 1, 10000},
	} {
		v, err := envInt(n.name, n.def)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		if v < n.min || v > n.max {
			problems = append(problems, fmt.Sprintf("%s: must be between %d and %d", n.name, n.min, n.max))
			continue
		}
		*n.dst = v
	}

	dedup, dedupErr := envBool("DELIVERY_DEDUP_ENABLED", true)
	if dedupErr != nil {
		problems = append(problems, dedupErr.Error())
	}
	cfg.DeliveryDedupEnabled = dedup

	for _, d := range []struct {
		name string
		def  time.Duration
		dst  *time.Duration
	}{
		{"DB_CONNECT_TIMEOUT", defaultDBConnectTimeout, &cfg.DBConnectTimeout},
		{"SHUTDOWN_TIMEOUT", defaultShutdownTimeout, &cfg.ShutdownTimeout},
		{"REQUEST_TIMEOUT", defaultRequestTimeout, &cfg.RequestTimeout},
		{"DELIVERY_TIMEOUT", defaultDeliveryTimeout, &cfg.DeliveryTimeout},
		{"RETRY_BASE_DELAY", defaultRetryBaseDelay, &cfg.RetryBaseDelay},
		{"RETRY_MAX_DELAY", defaultRetryMaxDelay, &cfg.RetryMaxDelay},
		{"STALE_CLAIM_TIMEOUT", defaultStaleClaimTimeout, &cfg.StaleClaimTimeout},
		{"RELAY_POLL_INTERVAL", defaultRelayPollInterval, &cfg.RelayPollInterval},
		{"DELIVERY_LEASE", defaultDeliveryLease, &cfg.DeliveryLease},
		{"DELIVERY_DEDUP_TTL", defaultDedupTTL, &cfg.DeliveryDedupTTL},
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
	if strings.TrimSpace(cfg.ValkeyURL) == "" {
		problems = append(problems, "VALKEY_URL: must not be empty")
	}

	// A stale-claim timeout at or below the delivery timeout would let a
	// slow-but-healthy delivery be reclaimed and duplicated while it is still
	// in flight. Catching that here beats discovering it as mysterious
	// duplicate deliveries under load.
	if cfg.StaleClaimTimeout > 0 && cfg.DeliveryTimeout > 0 && cfg.StaleClaimTimeout <= cfg.DeliveryTimeout {
		problems = append(problems, fmt.Sprintf(
			"STALE_CLAIM_TIMEOUT (%s) must be greater than DELIVERY_TIMEOUT (%s), or in-flight deliveries will be reclaimed and duplicated",
			cfg.StaleClaimTimeout, cfg.DeliveryTimeout))
	}
	// Likewise the lease has to outlast a delivery plus the reclaim sweep, or
	// the relay would requeue an event the worker is still delivering.
	if cfg.DeliveryLease > 0 && cfg.DeliveryLease <= cfg.StaleClaimTimeout {
		problems = append(problems, fmt.Sprintf(
			"DELIVERY_LEASE (%s) must be greater than STALE_CLAIM_TIMEOUT (%s), or the relay will requeue events the workers still hold",
			cfg.DeliveryLease, cfg.StaleClaimTimeout))
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

		"worker_concurrency":     c.WorkerConcurrency,
		"delivery_timeout":       c.DeliveryTimeout.String(),
		"max_attempts":           c.MaxAttempts,
		"retry_base_delay":       c.RetryBaseDelay.String(),
		"retry_max_delay":        c.RetryMaxDelay.String(),
		"stale_claim_timeout":    c.StaleClaimTimeout.String(),
		"relay_poll_interval":    c.RelayPollInterval.String(),
		"relay_batch_size":       c.RelayBatchSize,
		"delivery_lease":         c.DeliveryLease.String(),
		"delivery_dedup_enabled": c.DeliveryDedupEnabled,
		"delivery_dedup_ttl":     c.DeliveryDedupTTL.String(),
	}
}

// envBool parses a boolean environment variable.
func envBool(key string, def bool) (bool, error) {
	raw, ok := os.LookupEnv(key)
	if !ok || raw == "" {
		return def, nil
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		return def, fmt.Errorf("%s: %q is not a boolean (want true/false/1/0)", key, raw)
	}
	return v, nil
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
