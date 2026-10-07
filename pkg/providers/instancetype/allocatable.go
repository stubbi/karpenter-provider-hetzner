package instancetype

import (
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
)

// Kubelet's own defaults for the signals that move allocatable, applied to any
// signal a declared kubelet block does not list. The kubelet always holds
// something back even when nothing is configured, so modelling zero would
// overstate what a pod can use.
const (
	defaultMemoryEvictionThreshold = "100Mi"
	defaultNodefsEvictionThreshold = "10%"
)

// overheadFor converts a node class's declared kubelet reservations into the
// overhead Karpenter subtracts from capacity. Karpenter's own formula is
// allocatable = capacity - (kubeReserved + systemReserved + evictionThreshold),
// so every one of those three has to be filled in for allocatable to match what
// the node will actually report.
//
// capacity is required because eviction thresholds may be expressed as a
// percentage of it.
func overheadFor(nodeClass *apiv1.HCloudNodeClass, capacity corev1.ResourceList) *cloudprovider.InstanceTypeOverhead {
	var kubelet *apiv1.KubeletConfiguration
	if nodeClass != nil {
		kubelet = nodeClass.Spec.Kubelet
	}
	if kubelet == nil || len(kubelet.SystemReserved)+len(kubelet.KubeReserved)+len(kubelet.EvictionHard) == 0 {
		// A node class that declares nothing (an empty block included) has said
		// nothing about its bootstrap, which is not the same as saying it reserves
		// nothing. Before this package read reservations from the node class it
		// subtracted a flat 100m/100Mi from every type; keeping exactly that as the
		// undeclared default means upgrading changes no existing node's advertised
		// allocatable.
		return &cloudprovider.InstanceTypeOverhead{KubeReserved: legacyDefaultKubeReserved()}
	}
	return &cloudprovider.InstanceTypeOverhead{
		KubeReserved:      parseResourceList(kubelet.KubeReserved),
		SystemReserved:    parseResourceList(kubelet.SystemReserved),
		EvictionThreshold: evictionThreshold(kubelet.EvictionHard, capacity),
	}
}

// legacyDefaultKubeReserved is what this provider reserved on every server type
// before reservations became declarable. It applies only when a node class
// declares no reservations; one that declares any is taken at its word.
func legacyDefaultKubeReserved() corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse("100m"),
		corev1.ResourceMemory: resource.MustParse("100Mi"),
	}
}

// evictionThreshold models the memory and disk the kubelet holds back to keep
// itself above its hard eviction signals. That headroom is unavailable to pods,
// so it reduces allocatable exactly as a reservation does.
//
// Only signals that move a resource Karpenter schedules on are translated:
// memory.available and nodefs.available. The others are accepted by the API but
// have no allocatable equivalent.
//
// A signal the node class does not list is assumed at the kubelet default. A
// stock kubelet zeroes it once any signal is set, but one running with
// mergeDefaultEvictionSettings keeps the default, and assuming zero there would
// place pods the kubelet then evicts. Assuming the default only undersells the
// node; a node class says the kubelet enforces nothing for a signal by declaring
// "0%", the kubelet's own spelling for that.
func evictionThreshold(hard map[string]apiv1.EvictionThreshold, capacity corev1.ResourceList) corev1.ResourceList {
	signal := func(name, fallback string, of resource.Quantity) resource.Quantity {
		if raw, ok := hard[name]; ok {
			if q, err := resolveThreshold(string(raw), of); err == nil {
				return q
			}
		}
		q, _ := resolveThreshold(fallback, of)
		return q
	}
	return corev1.ResourceList{
		corev1.ResourceMemory:           signal("memory.available", defaultMemoryEvictionThreshold, capacity[corev1.ResourceMemory]),
		corev1.ResourceEphemeralStorage: signal("nodefs.available", defaultNodefsEvictionThreshold, capacity[corev1.ResourceEphemeralStorage]),
	}
}

// resolveThreshold reads an eviction threshold, which the kubelet accepts either
// as a quantity ("400Mi") or as a percentage of the resource's capacity ("10%").
// Like the kubelet, "0%" and "100%" mean no threshold at all.
func resolveThreshold(raw string, capacity resource.Quantity) (resource.Quantity, error) {
	pct, isPct := strings.CutSuffix(raw, "%")
	if !isPct {
		return resource.ParseQuantity(raw)
	}
	if raw == "0%" || raw == "100%" {
		return resource.Quantity{}, nil
	}
	// The kubelet's own arithmetic (eviction.parsePercentage and
	// GetThresholdQuantity), float32 included. It is applied to this provider's
	// capacity, which is Hetzner's advertised size rather than what the kubelet
	// measures, so the result is exact only once capacity is.
	f, err := strconv.ParseFloat(pct, 32)
	if err != nil {
		return resource.Quantity{}, err
	}
	fraction := float32(f) / 100
	return *resource.NewQuantity(int64(float64(capacity.Value())*float64(fraction)), resource.BinarySI), nil
}

// parseResourceList converts the node class's string-keyed reservations into a
// ResourceList. The CRD admits only values ParseQuantity accepts (pinned by
// TestCRDPatterns), so a parse error is unreachable for a stored object.
func parseResourceList(in map[string]apiv1.ReservedQuantity) corev1.ResourceList {
	out := corev1.ResourceList{}
	for k, v := range in {
		if q, err := resource.ParseQuantity(string(v)); err == nil {
			out[corev1.ResourceName(k)] = q
		}
	}
	return out
}
