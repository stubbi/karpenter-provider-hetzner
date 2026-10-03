// Package metrics defines and registers Prometheus metrics for the Hetzner
// Karpenter provider under the "karpenter_hetzner_" namespace.
//
// All metrics are registered once in an init() against controller-runtime's
// shared Registry so they coexist safely with karpenter-core metrics.
// Callers import this package for its side-effects and then invoke the helper
// functions (RecordServerCreate, RecordServerDelete, RecordDrift,
// RecordCacheHit, RecordCacheMiss) to instrument hot paths.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Metric label values for the "result" label.
const (
	ResultSuccess = "success"
	ResultError   = "error"
)

var (
	// serverCreateTotal counts server create attempts by result (success|error).
	serverCreateTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "karpenter_hetzner",
		Name:      "server_create_total",
		Help:      "Total number of Hetzner server create calls by result.",
	}, []string{"result"})

	// serverCreateErrorsTotal counts failed server creates by hcloud error code, so
	// quota exhaustion (resource_limit_exceeded) is visible on its own rather than
	// hidden among capacity errors Karpenter routes around.
	serverCreateErrorsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "karpenter_hetzner",
		Name:      "server_create_errors_total",
		Help:      "Total number of failed Hetzner server creates by hcloud error code.",
	}, []string{"code"})

	// serverCreateDurationSeconds measures how long server creates take (wall
	// time from Create call through action-wait completion).
	serverCreateDurationSeconds = prometheus.NewHistogram(prometheus.HistogramOpts{
		Namespace: "karpenter_hetzner",
		Name:      "server_create_duration_seconds",
		Help:      "Duration of Hetzner server create operations in seconds.",
		Buckets:   []float64{1, 5, 10, 20, 30, 60, 120},
	})

	// serverDeleteTotal counts server delete attempts by result (success|error).
	serverDeleteTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "karpenter_hetzner",
		Name:      "server_delete_total",
		Help:      "Total number of Hetzner server delete calls by result.",
	}, []string{"result"})

	// hcloudAPICallsTotal counts hcloud API calls by operation and result. We
	// scope it to the operations we actually instrument (server_create,
	// server_delete, placement_group, image_list) to keep label cardinality
	// predictable without threading a counter through every internal helper.
	hcloudAPICallsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "karpenter_hetzner",
		Name:      "hcloud_api_calls_total",
		Help:      "Total number of hcloud API calls by operation and result.",
	}, []string{"operation", "result"})

	// driftDetectedTotal counts drift detections by reason.
	driftDetectedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "karpenter_hetzner",
		Name:      "drift_detected_total",
		Help:      "Total number of drift detections by reason.",
	}, []string{"reason"})

	// selectionSkippedTotal counts launches whose instance-type selection had to pass
	// over an architecture, by architecture and reason. Incremented once per architecture
	// per launch rather than once per skipped type, so the rate tracks how often the
	// provider routes around the problem instead of how many types hcloud happens to
	// publish. The common case is non-terminal -- a cheaper candidate is skipped and a
	// pricier one launches -- so nothing else in the system reveals it.
	selectionSkippedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "karpenter_hetzner",
		Name:      "instance_type_selection_skipped_total",
		Help:      "Total number of launches that skipped an architecture during instance-type selection, by architecture and reason.",
	}, []string{"arch", "reason"})

	// imageResolutionErrorsTotal counts image lookups that failed without proving the
	// image absent. The NodeClass stays Ready in that case, so nothing else surfaces it.
	imageResolutionErrorsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Namespace: "karpenter_hetzner",
		Name:      "image_resolution_errors_total",
		Help:      "Total number of image lookups that failed without proving the image absent.",
	})

	// instanceTypeCacheTotal counts instance-type cache lookups by result
	// (hit|miss|stale).
	instanceTypeCacheTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "karpenter_hetzner",
		Name:      "instance_type_cache_total",
		Help:      "Total number of instance-type cache lookups by result.",
	}, []string{"result"})
)

func init() {
	crmetrics.Registry.MustRegister(
		serverCreateTotal,
		serverCreateErrorsTotal,
		serverCreateDurationSeconds,
		serverDeleteTotal,
		hcloudAPICallsTotal,
		driftDetectedTotal,
		instanceTypeCacheTotal,
		selectionSkippedTotal,
		imageResolutionErrorsTotal,
	)
}

// RecordServerCreate records a server create result and its duration.
// Call this once per Create() return, passing "success" or "error" and
// the wall-clock duration measured from the call's entry point.
func RecordServerCreate(result string, dur time.Duration) {
	serverCreateTotal.WithLabelValues(result).Inc()
	serverCreateDurationSeconds.Observe(dur.Seconds())
	hcloudAPICallsTotal.WithLabelValues("server_create", result).Inc()
}

// RecordServerCreateError records a failed server create by hcloud error code
// ("other" when the error carries none).
func RecordServerCreateError(code string) {
	serverCreateErrorsTotal.WithLabelValues(code).Inc()
}

// RecordServerDelete records a server delete result.
func RecordServerDelete(result string) {
	serverDeleteTotal.WithLabelValues(result).Inc()
	hcloudAPICallsTotal.WithLabelValues("server_delete", result).Inc()
}

// RecordDrift increments the drift counter for the given reason string
// (e.g. "ImageDrift", "NetworkDrift").
func RecordDrift(reason string) {
	driftDetectedTotal.WithLabelValues(reason).Inc()
}

// RecordSelectionSkipped records that instance-type selection passed over an
// architecture for a launch, e.g. reason "no_resolved_image". Call once per
// architecture per launch, not once per skipped instance type.
func RecordSelectionSkipped(arch, reason string) {
	selectionSkippedTotal.WithLabelValues(arch, reason).Inc()
}

// RecordImageResolutionError records a failed image lookup that did not prove the image
// absent, so the affected architecture kept its previously resolved ID.
func RecordImageResolutionError() {
	imageResolutionErrorsTotal.Inc()
}

// RecordCacheHit records an instance-type cache hit.
func RecordCacheHit() {
	instanceTypeCacheTotal.WithLabelValues("hit").Inc()
}

// RecordCacheMiss records an instance-type cache miss (triggers a fresh API fetch).
func RecordCacheMiss() {
	instanceTypeCacheTotal.WithLabelValues("miss").Inc()
}

// RecordCacheStale records that a refresh failed and the previously fetched
// catalogue was served instead. Nothing else surfaces this: serving stale keeps
// provisioning working, so the only symptom is a catalogue that quietly stops
// tracking hcloud. Alert on a sustained non-zero rate.
func RecordCacheStale() {
	instanceTypeCacheTotal.WithLabelValues("stale").Inc()
}
