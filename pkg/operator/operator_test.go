package operator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// hangingServer returns a test server that accepts the connection and never
// answers, which is the shape of the failure a timeout has to catch: a healthy
// TCP handshake followed by silence. It is not a dropped packet or a refused
// connection, both of which fail fast on their own.
func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	released := make(chan struct{})
	t.Cleanup(func() { close(released) })
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-released:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestHCloudClient_TimesOutOnHangingAPI is the regression for the unbounded
// client. hcloud.NewClient defaults to a bare &http.Client{}, so before this the
// call below never returned and the controller-runtime worker that made it was
// consumed for the life of the process.
//
// The context deadline is deliberately much longer than the request timeout: it
// is the test's backstop, not the thing under test. If the request timeout is
// what fires, the call returns in roughly the request timeout; if the client is
// unbounded again, only the context ends it and the elapsed time gives that away.
func TestHCloudClient_TimesOutOnHangingAPI(t *testing.T) {
	srv := hangingServer(t)

	client := hcloud.NewClient(append(
		hcloudOptions("test-token", 200*time.Millisecond),
		hcloud.WithEndpoint(srv.URL),
		// One attempt: the production retry policy correctly treats a timeout as
		// retryable, and its backoff would dominate the test without testing
		// anything this test is about.
		hcloud.WithRetryOpts(hcloud.RetryOpts{MaxRetries: 0}),
	)...)

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	start := time.Now()
	_, err := client.ServerType.All(ctx)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected an error from a hanging API, got nil")
	}
	if ctx.Err() != nil {
		t.Fatalf("the context deadline ended the call after %s, so the HTTP client is still unbounded", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("call took %s with a 200ms request timeout; the timeout is not bounding the request", elapsed)
	}
}

// The transport has to bound the request phases individually, not just the
// request as a whole: Client.Timeout alone cannot distinguish a stalled TLS
// handshake from a slow response, so a stall burns the entire budget before the
// SDK retries anything.
func TestNewHTTPClient_BoundsEveryPhase(t *testing.T) {
	c := newHTTPClient(defaultAPITimeout)

	if c.Timeout != defaultAPITimeout {
		t.Errorf("Timeout = %s, want %s", c.Timeout, defaultAPITimeout)
	}
	rl, ok := c.Transport.(rateLimitTransport)
	if !ok {
		t.Fatalf("Transport is %T, want rateLimitTransport", c.Transport)
	}
	tr, ok := rl.next.(*http.Transport)
	if !ok {
		t.Fatalf("wrapped transport is %T, want *http.Transport", rl.next)
	}
	if tr.TLSHandshakeTimeout <= 0 {
		t.Error("TLSHandshakeTimeout is unset, so a stalled handshake is bounded only by the whole-request timeout")
	}
	if tr.ResponseHeaderTimeout <= 0 {
		t.Error("ResponseHeaderTimeout is unset, so a server that accepts and never answers is bounded only by the whole-request timeout")
	}
	// Cloning http.DefaultTransport rather than building a bare &http.Transport{}
	// is what keeps proxy support and HTTP/2 working; both are silently lost by a
	// zero-value transport, and a cluster behind an egress proxy would then fail
	// to reach the API at all.
	if tr.Proxy == nil {
		t.Error("Proxy is nil: a bare transport ignores HTTPS_PROXY, which breaks clusters with egress proxies")
	}
}

func TestLoadConfig_APITimeoutDefaultsWhenUnset(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "paperclip-prod")
	t.Setenv("HCLOUD_API_TIMEOUT", "")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APITimeout != defaultAPITimeout {
		t.Errorf("APITimeout = %s, want %s", cfg.APITimeout, defaultAPITimeout)
	}
}

func TestLoadConfig_APITimeoutFromEnv(t *testing.T) {
	t.Setenv("CLUSTER_NAME", "paperclip-prod")
	t.Setenv("HCLOUD_API_TIMEOUT", "45s")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.APITimeout != 45*time.Second {
		t.Errorf("APITimeout = %s, want 45s", cfg.APITimeout)
	}
}

// A bad value fails startup rather than falling back. Falling back would leave
// the operator running with a timeout its operator does not believe is in
// effect, and this is the setting someone reaches for when the API is already
// misbehaving.
func TestLoadConfig_RejectsBadAPITimeout(t *testing.T) {
	for _, bad := range []string{"soon", "30", "0s", "-1s", "10m"} {
		t.Setenv("CLUSTER_NAME", "paperclip-prod")
		t.Setenv("HCLOUD_API_TIMEOUT", bad)
		if _, err := LoadConfig(); err == nil {
			t.Errorf("expected an error for HCLOUD_API_TIMEOUT %q", bad)
		}
	}
}

func TestNewHCloudClient_RequiresToken(t *testing.T) {
	t.Setenv("HCLOUD_TOKEN", "")
	if _, err := NewHCloudClient(&Config{ClusterName: "c", APITimeout: defaultAPITimeout}); err == nil {
		t.Fatal("expected an error when HCLOUD_TOKEN is unset")
	}
}
