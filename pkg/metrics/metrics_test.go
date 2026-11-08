package metrics_test

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/metrics"
)

// findMetricFamily returns metric families by exact name from crmetrics.Registry.
func findMetricFamily(t *testing.T, name string) (float64, bool) {
	t.Helper()
	mfs, err := crmetrics.Registry.(prometheus.Gatherer).Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, mf := range mfs {
		if mf.GetName() == name {
			return float64(len(mf.GetMetric())), true
		}
	}
	return 0, false
}

// counterValue returns the sum of all counter values for a given metric family
// name whose labels contain the given key=value pair. When multiple series
// match (e.g. the same "operation" label but different "result" labels), their
// values are summed.
func counterValue(t *testing.T, name string, labelKey, labelVal string) float64 {
	t.Helper()
	mfs, err := crmetrics.Registry.(prometheus.Gatherer).Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var total float64
	for _, mf := range mfs {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				if lp.GetName() == labelKey && lp.GetValue() == labelVal {
					total += m.GetCounter().GetValue()
					break
				}
			}
		}
	}
	return total
}

func TestRecordServerCreate_CounterIncremented(t *testing.T) {
	before := counterValue(t, "karpenter_hetzner_server_create_total", "result", "success")
	metrics.RecordServerCreate(metrics.ResultSuccess, 2*time.Second)
	after := counterValue(t, "karpenter_hetzner_server_create_total", "result", "success")
	if after <= before {
		t.Errorf("expected server_create_total{result=success} to increase; before=%v after=%v", before, after)
	}
}

func TestRecordServerCreate_ErrorCounterIncremented(t *testing.T) {
	before := counterValue(t, "karpenter_hetzner_server_create_total", "result", "error")
	metrics.RecordServerCreate(metrics.ResultError, 500*time.Millisecond)
	after := counterValue(t, "karpenter_hetzner_server_create_total", "result", "error")
	if after <= before {
		t.Errorf("expected server_create_total{result=error} to increase; before=%v after=%v", before, after)
	}
}

func TestRecordServerCreate_AlsoRecordsAPICall(t *testing.T) {
	before := counterValue(t, "karpenter_hetzner_hcloud_api_calls_total", "operation", "server_create")
	metrics.RecordServerCreate(metrics.ResultSuccess, 1*time.Second)
	after := counterValue(t, "karpenter_hetzner_hcloud_api_calls_total", "operation", "server_create")
	if after <= before {
		t.Errorf("expected hcloud_api_calls_total{operation=server_create} to increase; before=%v after=%v", before, after)
	}
}

func TestRecordServerDelete_CounterIncremented(t *testing.T) {
	before := counterValue(t, "karpenter_hetzner_server_delete_total", "result", "success")
	metrics.RecordServerDelete(metrics.ResultSuccess)
	after := counterValue(t, "karpenter_hetzner_server_delete_total", "result", "success")
	if after <= before {
		t.Errorf("expected server_delete_total{result=success} to increase; before=%v after=%v", before, after)
	}
}

func TestRecordDrift_IncrementsCounter(t *testing.T) {
	beforeImage := counterValue(t, "karpenter_hetzner_drift_detected_total", "reason", "ImageDrift")
	beforeNet := counterValue(t, "karpenter_hetzner_drift_detected_total", "reason", "NetworkDrift")

	metrics.RecordDrift("ImageDrift")
	metrics.RecordDrift("NetworkDrift")
	metrics.RecordDrift("ImageDrift")

	afterImage := counterValue(t, "karpenter_hetzner_drift_detected_total", "reason", "ImageDrift")
	afterNet := counterValue(t, "karpenter_hetzner_drift_detected_total", "reason", "NetworkDrift")

	if afterImage-beforeImage < 2 {
		t.Errorf("expected ImageDrift to increase by >= 2; delta=%v", afterImage-beforeImage)
	}
	if afterNet-beforeNet < 1 {
		t.Errorf("expected NetworkDrift to increase by >= 1; delta=%v", afterNet-beforeNet)
	}
}

func TestRecordCacheHitMiss_IncrementsCounters(t *testing.T) {
	beforeHit := counterValue(t, "karpenter_hetzner_instance_type_cache_total", "result", "hit")
	beforeMiss := counterValue(t, "karpenter_hetzner_instance_type_cache_total", "result", "miss")

	metrics.RecordCacheHit()
	metrics.RecordCacheHit()
	metrics.RecordCacheMiss()

	afterHit := counterValue(t, "karpenter_hetzner_instance_type_cache_total", "result", "hit")
	afterMiss := counterValue(t, "karpenter_hetzner_instance_type_cache_total", "result", "miss")

	if afterHit-beforeHit < 2 {
		t.Errorf("expected hit to increase by >= 2; delta=%v", afterHit-beforeHit)
	}
	if afterMiss-beforeMiss < 1 {
		t.Errorf("expected miss to increase by >= 1; delta=%v", afterMiss-beforeMiss)
	}
}

func TestHistogramRegistered(t *testing.T) {
	metrics.RecordServerCreate(metrics.ResultSuccess, 5*time.Second)

	_, found := findMetricFamily(t, "karpenter_hetzner_server_create_duration_seconds")
	if !found {
		t.Error("histogram karpenter_hetzner_server_create_duration_seconds not found in registry")
	}
}

func TestRecordServerDeleteError_CountsByCode(t *testing.T) {
	before := counterValue(t, "karpenter_hetzner_server_delete_errors_total", "code", "protected")
	metrics.RecordServerDeleteError("protected")
	after := counterValue(t, "karpenter_hetzner_server_delete_errors_total", "code", "protected")
	if after != before+1 {
		t.Errorf("server_delete_errors_total{code=protected}: before=%v after=%v, want +1", before, after)
	}
}
