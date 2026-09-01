package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/singha105/webhook-relay/internal/config"
	"github.com/singha105/webhook-relay/internal/store"
)

// runHealthcheck handles `worker -healthcheck`.
//
// The worker serves no HTTP, so unlike the API it cannot probe an endpoint of
// its own. It checks what it actually needs instead: that Postgres and Valkey
// are both reachable with the process's real configuration. A worker that can
// reach neither cannot deliver anything, and reporting healthy in that state
// would hide a total outage behind a green container.
//
// This is a liveness-adjacent check by necessity. A genuine liveness probe
// would not touch dependencies, but a worker with no listener has nothing else
// to answer with; Day 4 adds a metrics listener, at which point this becomes a
// real HTTP probe.
func runHealthcheck() (bool, int) {
	found := false
	for _, arg := range os.Args[1:] {
		if arg == "-healthcheck" || arg == "--healthcheck" {
			found = true
			break
		}
	}
	if !found {
		return false, 0
	}

	cfg, err := config.Load()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "unhealthy: %v\n", err)
		return true, 1
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	st, err := store.New(ctx, cfg.DatabaseURL, 1, 2*time.Second)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "unhealthy: postgres unreachable: %v\n", err)
		return true, 1
	}
	defer st.Close()

	opt, err := redis.ParseURL(cfg.ValkeyURL)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "unhealthy: %v\n", err)
		return true, 1
	}
	client := redis.NewClient(opt)
	defer func() { _ = client.Close() }()

	if err := client.Ping(ctx).Err(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "unhealthy: valkey unreachable: %v\n", err)
		return true, 1
	}

	_, _ = fmt.Fprintln(os.Stdout, "healthy")
	return true, 0
}
