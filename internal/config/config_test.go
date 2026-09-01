package config

import (
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	// t.Setenv on no keys still isolates: the test binary's env is restored.
	for _, k := range []string{"HTTP_ADDR", "DATABASE_URL", "VALKEY_URL", "LOG_LEVEL", "DB_MAX_CONNS", "SHUTDOWN_TIMEOUT", "REQUEST_TIMEOUT", "DB_CONNECT_TIMEOUT"} {
		t.Setenv(k, "")
	}
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.HTTPAddr != defaultHTTPAddr {
		t.Errorf("HTTPAddr = %q, want %q", cfg.HTTPAddr, defaultHTTPAddr)
	}
	if cfg.LogLevel != slog.LevelInfo {
		t.Errorf("LogLevel = %v, want info", cfg.LogLevel)
	}
	if cfg.DBMaxConns != defaultDBMaxConns {
		t.Errorf("DBMaxConns = %d, want %d", cfg.DBMaxConns, defaultDBMaxConns)
	}
	if cfg.ShutdownTimeout != defaultShutdownTimeout {
		t.Errorf("ShutdownTimeout = %v, want %v", cfg.ShutdownTimeout, defaultShutdownTimeout)
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Setenv("HTTP_ADDR", ":9999")
	t.Setenv("LOG_LEVEL", "DEBUG")
	t.Setenv("DB_MAX_CONNS", "42")
	t.Setenv("SHUTDOWN_TIMEOUT", "3s")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.HTTPAddr != ":9999" {
		t.Errorf("HTTPAddr = %q", cfg.HTTPAddr)
	}
	if cfg.LogLevel != slog.LevelDebug {
		t.Errorf("LogLevel = %v, want debug (case-insensitive parse)", cfg.LogLevel)
	}
	if cfg.DBMaxConns != 42 {
		t.Errorf("DBMaxConns = %d", cfg.DBMaxConns)
	}
	if cfg.ShutdownTimeout != 3*time.Second {
		t.Errorf("ShutdownTimeout = %v", cfg.ShutdownTimeout)
	}
}

func TestLoadInvalid(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		wantSub string
	}{
		{"bad log level", map[string]string{"LOG_LEVEL": "loud"}, "LOG_LEVEL"},
		{"non-numeric max conns", map[string]string{"DB_MAX_CONNS": "many"}, "DB_MAX_CONNS"},
		{"zero max conns", map[string]string{"DB_MAX_CONNS": "0"}, "DB_MAX_CONNS"},
		{"bad duration", map[string]string{"SHUTDOWN_TIMEOUT": "soon"}, "SHUTDOWN_TIMEOUT"},
		{"negative duration", map[string]string{"REQUEST_TIMEOUT": "-5s"}, "REQUEST_TIMEOUT"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for k, v := range tt.env {
				t.Setenv(k, v)
			}
			_, err := Load()
			if err == nil {
				t.Fatal("Load() = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error %q does not mention %q", err, tt.wantSub)
			}
		})
	}
}

// A misconfigured deployment should not need one restart per bad variable.
func TestLoadReportsEveryProblemAtOnce(t *testing.T) {
	t.Setenv("LOG_LEVEL", "loud")
	t.Setenv("DB_MAX_CONNS", "many")
	t.Setenv("SHUTDOWN_TIMEOUT", "soon")

	_, err := Load()
	if err == nil {
		t.Fatal("Load() = nil, want error")
	}
	for _, want := range []string{"LOG_LEVEL", "DB_MAX_CONNS", "SHUTDOWN_TIMEOUT"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error is missing %q; got:\n%s", want, err)
		}
	}
}

func TestRedactedHidesPasswords(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://user:hunter2@db.internal:5432/wr?sslmode=disable")
	t.Setenv("VALKEY_URL", "redis://default:s3cr3t@cache:6379")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	dump := cfg.Redacted()

	for _, leaked := range []string{"hunter2", "s3cr3t"} {
		for k, v := range dump {
			if s, ok := v.(string); ok && strings.Contains(s, leaked) {
				t.Errorf("Redacted()[%q] leaked the password %q: %s", k, leaked, s)
			}
		}
	}
	// The non-secret parts must survive, or the log line is useless.
	if got := dump["database_url"].(string); !strings.Contains(got, "db.internal:5432") {
		t.Errorf("redaction destroyed the host: %s", got)
	}
	if got := dump["database_url"].(string); !strings.Contains(got, "user") {
		t.Errorf("redaction destroyed the username: %s", got)
	}
}

func TestRedactURLWithoutCredentials(t *testing.T) {
	// A URL with no password must pass through untouched, not get mangled.
	in := "postgres://localhost:5432/webhook_relay?sslmode=disable"
	if got := redactURL(in); got != in {
		t.Errorf("redactURL(%q) = %q, want unchanged", in, got)
	}
}

func TestLoadDeliveryDefaults(t *testing.T) {
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() = %v", err)
	}
	if cfg.WorkerConcurrency != defaultWorkerConcurrency {
		t.Errorf("WorkerConcurrency = %d, want %d", cfg.WorkerConcurrency, defaultWorkerConcurrency)
	}
	if cfg.MaxAttempts != defaultMaxAttempts {
		t.Errorf("MaxAttempts = %d, want %d", cfg.MaxAttempts, defaultMaxAttempts)
	}
	// The dedup guard must default ON. Day 5 turns it off explicitly; it must
	// never be off because someone forgot to set it.
	if !cfg.DeliveryDedupEnabled {
		t.Error("DeliveryDedupEnabled defaults to false; it must default to true")
	}
}

func TestDeliveryDedupCanBeDisabled(t *testing.T) {
	for _, raw := range []string{"false", "0", "FALSE"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv("DELIVERY_DEDUP_ENABLED", raw)
			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load() = %v", err)
			}
			if cfg.DeliveryDedupEnabled {
				t.Errorf("DELIVERY_DEDUP_ENABLED=%s did not disable the guard", raw)
			}
		})
	}

	t.Run("a non-boolean is rejected", func(t *testing.T) {
		t.Setenv("DELIVERY_DEDUP_ENABLED", "maybe")
		if _, err := Load(); err == nil {
			t.Error("Load() = nil, want an error for a non-boolean")
		}
	})
}

// These two relationships are the difference between a working system and one
// that duplicates deliveries under load, so they are validated at boot rather
// than left to be discovered in production.
func TestLoadRejectsTimeoutOrderingMistakes(t *testing.T) {
	t.Run("stale claim timeout must exceed the delivery timeout", func(t *testing.T) {
		t.Setenv("DELIVERY_TIMEOUT", "30s")
		t.Setenv("STALE_CLAIM_TIMEOUT", "10s")
		_, err := Load()
		if err == nil {
			t.Fatal("Load() = nil, want an error")
		}
		if !strings.Contains(err.Error(), "STALE_CLAIM_TIMEOUT") {
			t.Errorf("error does not name the offending variable: %v", err)
		}
		if !strings.Contains(err.Error(), "duplicated") {
			t.Errorf("error does not explain the consequence: %v", err)
		}
	})

	t.Run("the lease must exceed the stale claim timeout", func(t *testing.T) {
		t.Setenv("DELIVERY_TIMEOUT", "10s")
		t.Setenv("STALE_CLAIM_TIMEOUT", "60s")
		t.Setenv("DELIVERY_LEASE", "30s")
		_, err := Load()
		if err == nil {
			t.Fatal("Load() = nil, want an error")
		}
		if !strings.Contains(err.Error(), "DELIVERY_LEASE") {
			t.Errorf("error does not name the offending variable: %v", err)
		}
	})

	t.Run("the documented defaults satisfy both", func(t *testing.T) {
		cfg, err := Load()
		if err != nil {
			t.Fatalf("the default configuration is invalid: %v", err)
		}
		if cfg.StaleClaimTimeout <= cfg.DeliveryTimeout {
			t.Error("default StaleClaimTimeout does not exceed DeliveryTimeout")
		}
		if cfg.DeliveryLease <= cfg.StaleClaimTimeout {
			t.Error("default DeliveryLease does not exceed StaleClaimTimeout")
		}
	})
}

func TestLoadRejectsBadWorkerConcurrency(t *testing.T) {
	for _, v := range []string{"0", "-1", "100000", "lots"} {
		t.Run(v, func(t *testing.T) {
			t.Setenv("WORKER_CONCURRENCY", v)
			if _, err := Load(); err == nil {
				t.Errorf("Load() = nil, want an error for WORKER_CONCURRENCY=%s", v)
			}
		})
	}
}
