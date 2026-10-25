package operator

import (
	"fmt"
	"net"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/metrics"
)

// Action polling. The SDK's default is a CONSTANT 500ms (hcloud.NewClient sets
// pollBackoffFunc to ConstantBackoff(500ms)), and every server create waits on its
// create action plus each next action for the whole boot. A 20s boot is ~40
// requests, a slow one 50+, which made the poll by far the largest consumer of the
// project's hourly request budget -- a budget shared with the cloud controller
// manager and the CSI driver on the same token (#70).
//
// Nobody needs to learn that a boot finished at t+20.0s rather than t+22.5s, so the
// poll starts at 1s and doubles to a 5s ceiling: a typical create drops from ~40
// polls to ~6-8, and a slow one is bounded at one request per 5s instead of two per
// second. The cap is kept short because Create blocks a provisioning worker for the
// whole wait, so overshooting the action's completion by a long backoff step would
// delay node registration for nothing.
const (
	pollBase       = time.Second
	pollMultiplier = 2
	pollCap        = 5 * time.Second
)

// pollBackoff is the BackoffFunc the SDK's action waiter sleeps on between polls.
func pollBackoff() hcloud.BackoffFunc {
	return hcloud.ExponentialBackoffWithOpts(hcloud.ExponentialBackoffOpts{
		Base:       pollBase,
		Multiplier: pollMultiplier,
		Cap:        pollCap,
	})
}

// rateLimitTransport records the hcloud rate-limit headers of every response as
// gauges. The SDK parses them into Response.Meta.Ratelimit, but most call sites
// (every AllWithOpts, the action waiter) never hand the Response back, so reading
// them at the transport is the only place that sees every request, including the
// action poll that dominates the budget.
type rateLimitTransport struct {
	next http.RoundTripper
}

func (t rateLimitTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(req)
	if err == nil && resp != nil {
		recordRateLimit(resp.Header)
	}
	return resp, err
}

// recordRateLimit publishes RateLimit-Limit, RateLimit-Remaining and
// RateLimit-Reset (a unix timestamp) when present. Absent or malformed headers are
// ignored rather than zeroing the gauge, so a response without them (an error page
// from a proxy, say) cannot fake an exhausted budget.
func recordRateLimit(h http.Header) {
	parse := func(name string) (float64, bool) {
		v, err := strconv.ParseInt(h.Get(name), 10, 64)
		if err != nil {
			return 0, false
		}
		return float64(v), true
	}
	limit, okL := parse("RateLimit-Limit")
	remaining, okR := parse("RateLimit-Remaining")
	reset, okS := parse("RateLimit-Reset")
	if okL && okR {
		metrics.RecordRateLimit(limit, remaining)
	}
	if okS {
		metrics.RecordRateLimitReset(reset)
	}
}

// newHTTPClient returns the HTTP client the hcloud SDK is driven with.
//
// hcloud.NewClient defaults to a bare &http.Client{}, which has no Timeout and
// therefore no upper bound on a single request. A blackholed connection to
// api.hetzner.cloud (dropped SYN/ACK, a middlebox that accepts and never
// answers, a TLS handshake that stalls) parks the calling goroutine forever.
// Every call site here is a controller-runtime worker, and controller-runtime
// imposes no per-reconcile deadline, so a hung request permanently consumes a
// worker rather than failing and requeueing. Enough of them and the operator
// stops reconciling entirely while still reporting healthy.
//
// The timeout is per REQUEST, not per operation, which is what makes it safe to
// set aggressively: the SDK's action waiter polls /actions in a loop of separate
// requests, so a server create that legitimately takes two minutes is a long
// sequence of short requests, not one long one.
//
// A timeout also turns a hang into a retryable error rather than a silent stall:
// http.Client surfaces it as a net.Error with Timeout() == true, which the SDK's
// retry policy (5 attempts, exponential backoff capped at a minute) already
// treats as retryable, and which it abandons as soon as the caller's context is
// done.
//
// The transport bounds the phases individually as well. Client.Timeout alone
// covers the whole request, so without these a stalled TLS handshake is
// indistinguishable from a slow response and burns the entire budget before
// anything is retried.
func newHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = (&net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}).DialContext
	transport.TLSHandshakeTimeout = 10 * time.Second
	transport.ResponseHeaderTimeout = timeout
	// The operator talks to exactly one host, and the SDK's action waiter polls it
	// once every few seconds per in-flight create (see pollBackoff). The default of 2 idle connections per
	// host makes concurrent launches tear down and redial TLS constantly.
	transport.MaxIdleConnsPerHost = 20
	return &http.Client{Timeout: timeout, Transport: rateLimitTransport{next: transport}}
}

// hcloudOptions returns the SDK options used in production. Split out from
// NewHCloudClient so tests can point the same configuration at a test server.
func hcloudOptions(token string, timeout time.Duration) []hcloud.ClientOption {
	return []hcloud.ClientOption{
		hcloud.WithToken(token),
		hcloud.WithHTTPClient(newHTTPClient(timeout)),
		hcloud.WithApplication("karpenter-provider-hetzner", ""),
		hcloud.WithPollOpts(hcloud.PollOpts{BackoffFunc: pollBackoff()}),
	}
}

// NewHCloudClient creates a new Hetzner Cloud API client using the HCLOUD_TOKEN
// environment variable and the API timeout from cfg.
func NewHCloudClient(cfg *Config) (*hcloud.Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("config is required")
	}
	token := os.Getenv("HCLOUD_TOKEN")
	if token == "" {
		return nil, fmt.Errorf("HCLOUD_TOKEN environment variable is required")
	}
	return hcloud.NewClient(hcloudOptions(token, cfg.APITimeout)...), nil
}
