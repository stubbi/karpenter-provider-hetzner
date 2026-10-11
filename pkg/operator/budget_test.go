package operator

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// TestPollBackoff_GrowsToCap pins the action-poll schedule. The SDK default polls a
// constant 500ms for the whole boot, which made the poll the dominant consumer of
// the shared hourly budget (#70).
func TestPollBackoff_GrowsToCap(t *testing.T) {
	f := pollBackoff()
	want := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}
	for i, w := range want {
		if got := f(i); got != w {
			t.Errorf("poll %d: backoff %v, want %v", i, got, w)
		}
	}
}

// TestPollBackoff_FewPollsPerBoot checks the property that matters: the number of
// action polls over a typical 20s boot, which was ~40 at a constant 500ms.
func TestPollBackoff_FewPollsPerBoot(t *testing.T) {
	f := pollBackoff()
	polls, elapsed := 0, time.Duration(0)
	for elapsed < 20*time.Second {
		elapsed += f(polls)
		polls++
	}
	if polls > 8 {
		t.Errorf("a 20s boot takes %d polls, want at most 8", polls)
	}
}

// TestHCloudClient_UsesPollBackoff drives a real action wait through the production
// options and counts the polls, so a refactor that drops WithPollOpts fails here.
func TestHCloudClient_UsesPollBackoff(t *testing.T) {
	var polls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := polls.Add(1)
		status := "running"
		if n >= 3 {
			status = "success"
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"actions":[{"id":1,"status":"` + status + `","command":"create_server","progress":0,"started":"2026-01-01T00:00:00Z","finished":null,"resources":[],"error":null}],"meta":{"pagination":{"page":1,"per_page":25,"previous_page":null,"next_page":null,"last_page":1,"total_entries":1}}}`))
	}))
	defer srv.Close()

	client := hcloud.NewClient(append(hcloudOptions("test-token", 5*time.Second), hcloud.WithEndpoint(srv.URL))...)
	start := time.Now()
	if err := client.Action.WaitFor(context.Background(), &hcloud.Action{ID: 1, Status: hcloud.ActionStatusRunning}); err != nil {
		t.Fatalf("WaitFor: %v", err)
	}
	// Three polls with sleeps of 1s and 2s in between. At the SDK's 500ms default
	// the same wait finishes in about 1s.
	if elapsed := time.Since(start); elapsed < 2500*time.Millisecond {
		t.Errorf("wait finished after %v: the action poll is not using pollBackoff", elapsed)
	}
}

func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	mfs, err := crmetrics.Registry.(prometheus.Gatherer).Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return mf.GetMetric()[0].GetGauge().GetValue()
		}
	}
	t.Fatalf("metric %s not found", name)
	return 0
}

// TestHCloudClient_RecordsRateLimitHeaders checks every response's RateLimit-*
// headers reach the gauges, including on calls whose Response the SDK never hands
// back to the caller.
func TestHCloudClient_RecordsRateLimitHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("RateLimit-Limit", "3600")
		w.Header().Set("RateLimit-Remaining", "1234")
		w.Header().Set("RateLimit-Reset", "1900000000")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"server_types":[],"meta":{"pagination":{"page":1,"per_page":50,"previous_page":null,"next_page":null,"last_page":1,"total_entries":0}}}`))
	}))
	defer srv.Close()

	client := hcloud.NewClient(append(hcloudOptions("test-token", 5*time.Second), hcloud.WithEndpoint(srv.URL))...)
	if _, err := client.ServerType.All(context.Background()); err != nil {
		t.Fatalf("All: %v", err)
	}
	for name, want := range map[string]float64{
		"karpenter_hetzner_hcloud_ratelimit_limit":                   3600,
		"karpenter_hetzner_hcloud_ratelimit_remaining":               1234,
		"karpenter_hetzner_hcloud_ratelimit_reset_timestamp_seconds": 1900000000,
	} {
		if got := gaugeValue(t, name); got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
}

// TestRecordRateLimit_IgnoresMissingHeaders guards against a response without the
// headers (an error page from a proxy) zeroing the gauge and faking exhaustion.
func TestRecordRateLimit_IgnoresMissingHeaders(t *testing.T) {
	h := http.Header{}
	h.Set("RateLimit-Limit", "3600")
	h.Set("RateLimit-Remaining", "77")
	recordRateLimit(h)
	recordRateLimit(http.Header{})
	if got := gaugeValue(t, "karpenter_hetzner_hcloud_ratelimit_remaining"); got != 77 {
		t.Errorf("remaining = %v after a header-less response, want 77", got)
	}
}
