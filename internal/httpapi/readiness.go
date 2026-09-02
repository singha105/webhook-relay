package httpapi

import (
	"context"
	"fmt"
	"time"
)

// ReadinessChecker decides whether a process should receive traffic.
//
// # Why /healthz and /readyz must be different endpoints
//
// They answer different questions, and Kubernetes does different things with
// the answers.
//
//   - Liveness ("/healthz") asks: is this process broken beyond recovery?
//     A failure gets the container KILLED and restarted.
//   - Readiness ("/readyz") asks: should traffic go here right now?
//     A failure removes the pod from the Service's endpoints. It keeps running,
//     keeps being scraped, and rejoins by itself when it recovers.
//
// # What breaks if you conflate them
//
// Point liveness at the database, then let the database have a thirty-second
// blip. Every replica fails its liveness probe simultaneously and Kubernetes
// kills all of them at once. That:
//
//  1. Drops every in-flight request and every in-flight delivery, converting a
//     recoverable dependency blip into real data movement failures.
//  2. Starts a crash loop. The restarted pods still cannot reach the database,
//     fail liveness again, and get killed again — now with exponential backoff
//     on the restarts, so recovery is delayed well past the database's own.
//  3. Destroys the evidence. The logs and in-memory state of the pods that saw
//     the failure are gone, and you are debugging from the outside.
//
// The failure mode is worst precisely when the dependency recovers: the
// database comes back, and instead of the fleet resuming, it is midway through
// a CrashLoopBackOff cycle that keeps it down for minutes longer.
//
// The reverse conflation — readiness that checks nothing — is quieter but also
// wrong: a pod that cannot reach Postgres stays in the Service and returns
// errors to real traffic that a healthy replica could have served.
//
// So: liveness checks only that the process is running its own event loop.
// Readiness checks the dependencies this process needs to do useful work.
type ReadinessChecker struct {
	checks []namedCheck
}

type namedCheck struct {
	name string
	fn   func(context.Context) error
}

// NewReadinessChecker builds an empty checker.
func NewReadinessChecker() *ReadinessChecker { return &ReadinessChecker{} }

// Add registers a named check. Order matters: cheap checks first, so an
// unreachable database is reported without also waiting out a Valkey timeout.
func (r *ReadinessChecker) Add(name string, fn func(context.Context) error) *ReadinessChecker {
	r.checks = append(r.checks, namedCheck{name: name, fn: fn})
	return r
}

// AddBacklogCheck fails readiness when the oldest undelivered event is older
// than threshold.
//
// This is the interesting one, and it is deliberately a READINESS signal rather
// than a liveness one. A deep backlog does not mean this replica is broken —
// killing it would make the backlog worse by discarding its in-flight work.
// What it means is "do not send this replica more work", which for the API is
// exactly right: shedding ingest load while the workers catch up is preferable
// to accepting events we already know we cannot deliver on time.
//
// A threshold of zero disables the check, because a service that is legitimately
// draining a large backlog should not flap out of its Service and make recovery
// slower.
func (r *ReadinessChecker) AddBacklogCheck(threshold time.Duration, age func(context.Context) (time.Duration, error)) *ReadinessChecker {
	if threshold <= 0 {
		return r
	}
	return r.Add("backlog", func(ctx context.Context) error {
		got, err := age(ctx)
		if err != nil {
			// An unreadable backlog is not itself a reason to shed traffic;
			// the database check ahead of this one will already have failed if
			// Postgres is down, and failing here too would only turn one
			// dependency outage into two confusing reasons.
			//nolint:nilerr // deliberate: this check abstains rather than fails
			return nil
		}
		if got > threshold {
			return fmt.Errorf("oldest undelivered event is %s old, over the %s threshold",
				got.Round(time.Second), threshold)
		}
		return nil
	})
}

// Check runs every registered check, returning the first failure with its name.
func (r *ReadinessChecker) Check(ctx context.Context) error {
	for _, c := range r.checks {
		if err := c.fn(ctx); err != nil {
			return fmt.Errorf("%s: %w", c.name, err)
		}
	}
	return nil
}
