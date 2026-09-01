package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"
)

// SelfCheck probes a local readiness endpoint and reports the result.
//
// This exists because the runtime image is distroless: there is no shell, no
// curl, and no wget for a container healthcheck to invoke. The binary probes
// itself instead, which also means the healthcheck cannot drift from the code
// it is checking.
//
// It is invoked as `<binary> -healthcheck` and exits 0 for healthy, 1
// otherwise, which is exactly the contract Docker's HEALTHCHECK and
// Kubernetes' exec probe expect.
//
// attacker-controlled input. It is a loopback address by default and only
// overridable by the operator who already controls the process environment.
//
//nolint:gosec // G704: the URL is this process's own readiness endpoint, not
func SelfCheck(url string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("build healthcheck request: %w", err)
	}

	// A dedicated client, not http.DefaultClient: a probe that can hang
	// forever is worse than no probe, because the orchestrator sees neither a
	// pass nor a fail.
	client := &http.Client{Timeout: timeout}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("healthcheck request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("healthcheck returned %d", resp.StatusCode)
	}
	return nil
}

// RunSelfCheckFlag handles the -healthcheck flag if present.
//
// It parses os.Args directly rather than using the flag package, because the
// flag package would need to be wired into every binary's normal startup path
// just to support a mode that exits immediately.
//
// Returns (handled, exitCode).
func RunSelfCheckFlag(defaultURL string) (bool, int) {
	for _, arg := range os.Args[1:] {
		if arg != "-healthcheck" && arg != "--healthcheck" {
			continue
		}
		url := defaultURL
		if v := os.Getenv("HEALTHCHECK_URL"); v != "" {
			url = v
		}
		if err := SelfCheck(url, 3*time.Second); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "unhealthy: %v\n", err)
			return true, 1
		}
		_, _ = fmt.Fprintln(os.Stdout, "healthy")
		return true, 0
	}
	return false, 0
}
