// Package delivery performs the outbound HTTP call that delivers a webhook,
// and decides what the result means.
package delivery

import (
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/singha105/webhook-relay/internal/models"
	"github.com/singha105/webhook-relay/pkg/webhook"
)

// Transport and timeout defaults.
const (
	// DefaultTimeout bounds a single delivery. Ten seconds is generous for a
	// receiver that should be enqueueing and returning, and short enough that
	// a hung endpoint cannot hold a worker for long.
	DefaultTimeout = 10 * time.Second

	// DefaultMaxIdleConnsPerHost is deliberately well above Go's default of 2.
	// A relay concentrates traffic on a small number of hosts, so the stock
	// value would close and reopen a connection for almost every delivery —
	// a fresh TCP and TLS handshake per webhook, which is most of the latency
	// budget for a fast endpoint.
	DefaultMaxIdleConnsPerHost = 32

	// DefaultMaxIdleConns bounds the pool across all hosts.
	DefaultMaxIdleConns = 256

	// maxResponseReadBytes bounds how much of a response we read. We only ever
	// store 2 KiB, but read slightly more so truncation is visible rather than
	// coincidental. A receiver streaming a large body cannot make us buffer it.
	maxResponseReadBytes = 8 * 1024
)

// Client delivers webhooks over HTTP.
//
// One Client is shared by every worker goroutine. http.Client is safe for
// concurrent use and the connection pool only works if it is shared — a client
// per delivery would defeat keep-alive entirely.
type Client struct {
	httpClient *http.Client
	timeout    time.Duration
	maxDelay   time.Duration
	userAgent  string
	now        func() time.Time
}

// ClientConfig configures the delivery client.
type ClientConfig struct {
	Timeout             time.Duration
	MaxIdleConns        int
	MaxIdleConnsPerHost int
	// MaxRetryAfter clamps a server-supplied Retry-After.
	MaxRetryAfter time.Duration
	UserAgent     string
	// InsecureSkipVerify disables TLS verification. Test-only; there is no
	// configuration path that sets it in production.
	InsecureSkipVerify bool
}

// NewClient builds a delivery client with an explicit transport.
//
// http.DefaultClient is never used, for three reasons: it has no timeout at
// all, so a hung endpoint holds a goroutine forever; its transport is global,
// so any library in the process can mutate settings underneath us; and its
// MaxIdleConnsPerHost of 2 is wrong for this workload.
func NewClient(cfg ClientConfig) *Client {
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	maxIdle := cfg.MaxIdleConns
	if maxIdle <= 0 {
		maxIdle = DefaultMaxIdleConns
	}
	maxIdlePerHost := cfg.MaxIdleConnsPerHost
	if maxIdlePerHost <= 0 {
		maxIdlePerHost = DefaultMaxIdleConnsPerHost
	}
	ua := cfg.UserAgent
	if ua == "" {
		ua = "webhook-relay/1.0"
	}

	transport := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout: 5 * time.Second,
			// Detects a peer that has silently gone away, which a plain read
			// timeout would only notice at the end of the request timeout.
			KeepAlive: 30 * time.Second,
		}).DialContext,

		MaxIdleConns:        maxIdle,
		MaxIdleConnsPerHost: maxIdlePerHost,
		MaxConnsPerHost:     maxIdlePerHost * 2,

		// Bounded independently of the overall timeout so a slow TLS handshake
		// is distinguishable from a slow application response.
		TLSHandshakeTimeout:   5 * time.Second,
		ResponseHeaderTimeout: timeout,
		ExpectContinueTimeout: time.Second,

		// Close idle connections rather than holding them against an endpoint
		// that only receives an event an hour.
		IdleConnTimeout: 90 * time.Second,

		// Compression is off. Webhook payloads are small, and negotiating gzip
		// costs a round trip's worth of CPU on both ends for no benefit.
		DisableCompression: true,

		//nolint:gosec // InsecureSkipVerify is test-only; no production config path sets it.
		TLSClientConfig: &tls.Config{
			MinVersion:         tls.VersionTLS12,
			InsecureSkipVerify: cfg.InsecureSkipVerify,
		},
	}

	return &Client{
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   timeout,

			// Redirects are not followed. A 3xx from a webhook endpoint is a
			// misconfiguration, and following one would silently deliver a
			// signed payload to a host the customer never registered — an SSRF
			// primitive handed to whoever controls the original URL.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		timeout:   timeout,
		maxDelay:  cfg.MaxRetryAfter,
		userAgent: ua,
		now:       time.Now,
	}
}

// Request is one delivery attempt.
type Request struct {
	Endpoint *models.Endpoint
	Event    *models.Event
	// Attempt is 1-based.
	Attempt int
}

// Deliver performs one attempt and classifies the result.
//
// It never returns an error for a failed delivery: a 500 or a refused
// connection is an outcome, not an exception. An error is returned only when
// the attempt could not be constructed at all.
func (c *Client) Deliver(ctx context.Context, req Request) (Result, error) {
	body := []byte(req.Event.Payload)
	ts := c.now()

	// Timeout is applied to the request context as well as to http.Client, so
	// cancellation propagates into the dial and the body read rather than only
	// bounding the round trip.
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, req.Endpoint.URL, bytes.NewReader(body))
	if err != nil {
		return Result{}, fmt.Errorf("build delivery request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("User-Agent", c.userAgent)
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set(webhook.HeaderID, req.Event.ID.String())
	httpReq.Header.Set(webhook.HeaderTimestamp, strconv.FormatInt(ts.Unix(), 10))
	httpReq.Header.Set(webhook.HeaderSignature, webhook.Sign(req.Endpoint.SigningSecret, ts, body))
	httpReq.Header.Set(webhook.HeaderAttempt, strconv.Itoa(req.Attempt))
	// Set explicitly so a receiver behind a proxy that buffers does not have to
	// guess, and so the request is never sent chunked.
	httpReq.ContentLength = int64(len(body))

	start := c.now()
	resp, err := c.httpClient.Do(httpReq)
	elapsed := c.now().Sub(start)

	if err != nil {
		outcome, msg := ClassifyTransportError(err)
		return Result{
			Outcome:      outcome,
			Err:          err,
			ErrorMessage: msg,
			Duration:     elapsed,
			// StatusCode stays nil: there was no HTTP response at all, which
			// is a materially different signal from a 5xx.
			ResponseBody: "",
		}, nil
	}
	defer func() {
		// Drain before closing so the connection can be reused. Closing an
		// undrained body forces the transport to discard the connection, which
		// silently disables keep-alive for every subsequent delivery.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseReadBytes))
		_ = resp.Body.Close()
	}()

	respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponseReadBytes))
	if readErr != nil {
		// The status line arrived, so we know the outcome; we just could not
		// capture the body. Keep the status and note the read failure.
		respBody = []byte(fmt.Sprintf("<response body unreadable: %v>", readErr))
	}

	status := resp.StatusCode
	result := Result{
		Outcome:      Classify(status),
		StatusCode:   &status,
		ResponseBody: models.TruncateResponseBody(string(respBody)),
		Duration:     elapsed,
	}

	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		result.RetryAfter = ParseRetryAfter(resp.Header.Get("Retry-After"), c.now(), c.maxDelay)
	}
	if result.Outcome != OutcomeSuccess {
		result.Err = fmt.Errorf("endpoint returned %s", describeStatus(status))
		result.ErrorMessage = result.Err.Error()
	}
	return result, nil
}

// CloseIdleConnections releases pooled connections, for shutdown.
func (c *Client) CloseIdleConnections() {
	c.httpClient.CloseIdleConnections()
}
