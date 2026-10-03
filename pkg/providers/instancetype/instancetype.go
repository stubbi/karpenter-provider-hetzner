package instancetype

import (
	"context"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/metrics"
)

const cacheTTL = 6 * time.Hour

// ServerTypeClient is the narrow interface for the hcloud server types API.
type ServerTypeClient interface {
	All(ctx context.Context) ([]*hcloud.ServerType, error)
}

// Provider resolves Hetzner server types to Karpenter InstanceTypes.
type Provider struct {
	client ServerTypeClient

	// mu guards the cache fields only. It is never held across an hcloud call:
	// the catalogue is read on every provisioning decision, and a single slow or
	// hung API request must not be able to stall callers whose cache is warm.
	mu          sync.RWMutex
	cachedTypes []*cloudprovider.InstanceType
	cachedAt    time.Time
	cacheExpiry time.Time

	// refreshMu serializes catalogue refreshes so a burst of concurrent misses
	// makes one API call rather than N. It is deliberately a second mutex: it IS
	// held across the API call, and readers must never contend on it.
	refreshMu sync.Mutex

	unavailable *unavailableCache

	nowFn func() time.Time
}

// NewProvider creates a new instance type provider.
func NewProvider(client ServerTypeClient) *Provider {
	return &Provider{
		client: client,
		unavailable: newUnavailableCache(
			// 5m: long enough to route around a saturated location, short enough to
			// retry it soon. Matches observed recovery -- a resource_unavailable on
			// (cx43, hel1) cleared within minutes, an identical create succeeding ~70s
			// later. A long quarantine would keep falling through to a ~4x-priced type
			// well after capacity returned, so if this is ever tuned, prefer a short base
			// with backoff on repeat failures over a longer flat TTL.
			// TODO: make configurable via operator config if needed.
			5 * time.Minute,
		),
		nowFn: time.Now,
	}
}

// List returns all available InstanceTypes, filtered to those with offerings in the given locations.
// Results are cached for 6 hours.
//
// The hcloud call happens with no reader-visible lock held. Holding the cache
// lock across it made every caller wait on the slowest possible hcloud request,
// including the overwhelming majority whose cache was warm and who needed no
// network at all: one hung request stalled all provisioning, and the failure
// looked like Karpenter having stopped rather than like an API problem.
//
// Refreshes are still serialized, by a separate mutex that only refreshers take,
// so a burst of concurrent misses makes one API call rather than one per caller.
func (p *Provider) List(ctx context.Context, locations []string) ([]*cloudprovider.InstanceType, error) {
	if types, ok := p.freshCache(); ok {
		metrics.RecordCacheHit()
		return p.applyAvailability(filterByLocations(types, locations)), nil
	}

	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()

	// Another goroutine may have refreshed while this one waited for refreshMu.
	if types, ok := p.freshCache(); ok {
		metrics.RecordCacheHit()
		return p.applyAvailability(filterByLocations(types, locations)), nil
	}

	metrics.RecordCacheMiss()

	serverTypes, err := p.client.All(ctx)
	if err != nil {
		// Serve the expired catalogue rather than failing. Hetzner's server-type
		// catalogue changes on the order of years -- a six-hour-old copy is not
		// meaningfully less correct than a fresh one -- while returning an error
		// here fails Create and GetInstanceTypes, which stops the cluster
		// provisioning at all. A transient 5xx should not be able to do that.
		//
		// The expiry is deliberately NOT extended, so the next call retries the API
		// instead of settling into the stale copy, and the staleness is counted so
		// "serving stale" is alertable rather than silent. Availability is unaffected
		// either way: it is computed live from the unavailable cache on every call
		// (see applyAvailability), never baked into the catalogue.
		if stale, age, ok := p.staleCache(); ok {
			metrics.RecordCacheStale()
			logf.FromContext(ctx).Error(err, "hcloud server-type catalogue unreadable; serving the last one fetched",
				"age", age.Round(time.Second).String())
			return p.applyAvailability(filterByLocations(stale, locations)), nil
		}
		// Nothing was ever fetched, so there is nothing to fall back to.
		return nil, err
	}

	types := make([]*cloudprovider.InstanceType, 0, len(serverTypes))
	for _, st := range serverTypes {
		types = append(types, toInstanceType(st))
	}
	p.store(types)

	return p.applyAvailability(filterByLocations(types, locations)), nil
}

// freshCache returns the cached catalogue when it is present and unexpired.
func (p *Provider) freshCache() ([]*cloudprovider.InstanceType, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.cachedTypes != nil && p.nowFn().Before(p.cacheExpiry) {
		return p.cachedTypes, true
	}
	return nil, false
}

// staleCache returns the cached catalogue regardless of expiry, with its age.
func (p *Provider) staleCache() ([]*cloudprovider.InstanceType, time.Duration, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.cachedTypes == nil {
		return nil, 0, false
	}
	return p.cachedTypes, p.nowFn().Sub(p.cachedAt), true
}

// store replaces the cached catalogue and restarts its TTL.
func (p *Provider) store(types []*cloudprovider.InstanceType) {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := p.nowFn()
	p.cachedTypes = types
	p.cachedAt = now
	p.cacheExpiry = now.Add(cacheTTL)
}

// MarkUnavailable records that a (serverType, location) offering failed with a
// capacity error so it is reported unavailable for a TTL. The mark takes effect
// on the next call to List, which Karpenter invokes at the start of each
// provisioning cycle (not within the cycle that failed).
func (p *Provider) MarkUnavailable(serverType, location string) {
	p.unavailable.markUnavailable(serverType, location)
}

// applyAvailability returns copies of the given instance types with each
// offering's Available flag computed live from the unavailable cache, so the
// 6h type-catalog cache never bakes in (and thus never staleness-traps)
// availability.
//
// The returned InstanceType and Offering structs are fresh value-copies, so
// setting Available never mutates the cached entries. Note that nested
// reference fields (Requirements, Capacity, Overhead) are intentionally shared
// with the cache, not deep-copied: callers must treat returned types as
// read-only and must not mutate those maps.
func (p *Provider) applyAvailability(types []*cloudprovider.InstanceType) []*cloudprovider.InstanceType {
	out := make([]*cloudprovider.InstanceType, len(types))
	for i, it := range types {
		offerings := make(cloudprovider.Offerings, len(it.Offerings))
		for j, o := range it.Offerings {
			zone := o.Requirements.Get(corev1.LabelTopologyZone).Any()
			cp := *o
			// AND, never overwrite: o.Available already encodes catalogue facts (unpriced
			// or withdrawn by hcloud) that the capacity cache knows nothing about.
			cp.Available = o.Available && !p.unavailable.isUnavailable(it.Name, zone)
			offerings[j] = &cp
		}
		// Construct a fresh InstanceType (rather than copying *it) to avoid
		// copying the embedded sync.Once (govet copylocks); Requirements/Capacity/
		// Overhead are intentionally shared read-only with the cached entry.
		out[i] = &cloudprovider.InstanceType{
			Name:         it.Name,
			Offerings:    offerings,
			Requirements: it.Requirements,
			Capacity:     it.Capacity,
			Overhead:     it.Overhead,
		}
	}
	return out
}

// toInstanceType maps a Hetzner ServerType to a Karpenter InstanceType.
func toInstanceType(st *hcloud.ServerType) *cloudprovider.InstanceType {
	arch := "amd64"
	if st.Architecture == hcloud.ArchitectureARM {
		arch = "arm64"
	}

	cpuType := string(st.CPUType) // "shared" or "dedicated"

	// Build offerings: one per pricing location. Every priced location stays in the
	// catalogue even when it cannot be launched -- karpenter core documents that Offerings
	// must list all allowed offerings "even if they're temporarily unavailable", and
	// treats a running node whose offering has disappeared as drifted, replacing healthy
	// nodes. Availability, not membership, is what keeps an offering out of selection.
	//
	// Availability is NOT gated on ServerType.Locations[].Available. That flag is wrong in
	// BOTH directions, so no reading of it is safe:
	//
	//   false negatives -- observed reading false for (cx53, nbg1) 50 minutes after the
	//   API accepted a cx53 create there, for (cx53, fsn1) while a cx53 was created in
	//   fsn1, and across all three eu-central locations while eight cx53 nodes ran.
	//
	//   false positives -- hcloud datacenter describe hel1-dc2 listed cx43 in both
	//   server_types.available and available_for_migration, and the create was still
	//   rejected with resource_unavailable; an identical request minutes later succeeded.
	//
	// It carries no signal about whether a server can be created, so gating on it
	// proactively is unsound whichever way it is interpreted. Excluding those offerings
	// drops them out of ranking entirely, so a 32Gi pod falls through cx53 (EUR 29.49) to
	// cpx62 (EUR 129.99) -- a 4.4x regression from the change meant to prevent exactly
	// that. Withdrawn pairs are handled reactively by the unavailable cache, which marks
	// a pair only after a real create failure and so cannot be fooled in either
	// direction.
	//
	// A location's deprecation is different: once its unavailable_after has passed, hcloud
	// refuses every create there ("unsupported location for server type") while the type
	// stays priced. That is a published retirement date, not a stock reading, so those
	// offerings are unavailable up front instead of each costing a failed create and a
	// 5-minute quarantine per burst (cpx11-cpx51 in fsn1/nbg1/hel1 since 2026-01-01).
	retired := map[string]bool{}
	now := time.Now()
	for _, l := range st.Locations {
		if l.Location != nil && l.IsDeprecated() && now.After(l.UnavailableAfter()) {
			retired[l.Location.Name] = true
		}
	}
	offerings := make(cloudprovider.Offerings, 0, len(st.Pricings))
	for _, p := range st.Pricings {
		if p.Location == nil {
			continue
		}
		// An unpriced offering cannot be ranked, so it is unavailable rather than priced
		// at 0 -- a 0 sorts as the best deal in the cluster and would win every selection.
		price, priced := hourlyNetPrice(p)
		offerings = append(offerings, &cloudprovider.Offering{
			Requirements: scheduling.NewRequirements(
				scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
				scheduling.NewRequirement(corev1.LabelTopologyZone, corev1.NodeSelectorOpIn, p.Location.Name),
			),
			// Price stays 0 when unparseable. Selection is protected by Available, but
			// core reads a RUNNING node's price via Compatible(...).Cheapest() with no
			// Available filter, so a 0 here makes that node look free and blocks its
			// replacement-consolidation until pricing recovers. No representable value
			// avoids core's 0-fallback; the exposure is bounded by the pricing outage.
			Price:     price,
			Available: priced && !retired[p.Location.Name],
		})
	}

	// Memory: ServerType.Memory is float32 in GB.
	memBytes := int64(float64(st.Memory) * 1024 * 1024 * 1024)
	// Disk: ServerType.Disk is int in GB.
	diskBytes := int64(st.Disk) * 1024 * 1024 * 1024

	return &cloudprovider.InstanceType{
		Name:      st.Name,
		Offerings: offerings,
		Requirements: scheduling.NewRequirements(
			scheduling.NewRequirement(corev1.LabelInstanceTypeStable, corev1.NodeSelectorOpIn, st.Name),
			scheduling.NewRequirement(corev1.LabelArchStable, corev1.NodeSelectorOpIn, arch),
			scheduling.NewRequirement(karpv1.CapacityTypeLabelKey, corev1.NodeSelectorOpIn, karpv1.CapacityTypeOnDemand),
			scheduling.NewRequirement(apiv1.LabelCPUType, corev1.NodeSelectorOpIn, cpuType),
			scheduling.NewRequirement(apiv1.LabelServerFamily, corev1.NodeSelectorOpIn, serverFamily(st.Name)),
		),
		Capacity: corev1.ResourceList{
			corev1.ResourceCPU:              *resource.NewMilliQuantity(int64(st.Cores)*1000, resource.DecimalSI),
			corev1.ResourceMemory:           *resource.NewQuantity(memBytes, resource.BinarySI),
			corev1.ResourceEphemeralStorage: *resource.NewQuantity(diskBytes, resource.BinarySI),
			corev1.ResourcePods:             *resource.NewQuantity(110, resource.DecimalSI),
		},
		Overhead: &cloudprovider.InstanceTypeOverhead{
			KubeReserved: corev1.ResourceList{
				corev1.ResourceCPU:    resource.MustParse("100m"),
				corev1.ResourceMemory: resource.MustParse("100Mi"),
			},
		},
	}
}

// serverFamily extracts the server type family prefix (e.g. "cax", "cx", "cpx", "ccx").
func serverFamily(name string) string {
	for _, prefix := range []string{"cax", "cpx", "ccx", "cx"} {
		if strings.HasPrefix(name, prefix) {
			return prefix
		}
	}
	// Fall back to any leading alpha characters.
	i := 0
	for i < len(name) && (name[i] < '0' || name[i] > '9') {
		i++
	}
	if i > 0 {
		return name[:i]
	}
	return name
}

// Pricing here is the server-type base net price and intentionally excludes the
// primary-IPv4 surcharge: the catalog is NodeClass-agnostic. Cost-sensitive
// clusters drop the IPv4 charge with HCloudNodeClass.spec.enablePublicIPv4=false.
//
// hourlyNetPrice returns the net hourly price for a server-type pricing entry,
// preferring the explicit hourly figure and falling back to monthly/730. ok is false
// when neither figure yields a usable price; callers must drop the offering rather
// than substitute a default, because a zero price ranks as the best deal in the
// cluster and would win every selection.
func hourlyNetPrice(p hcloud.ServerTypeLocationPricing) (price float64, ok bool) {
	// ParseFloat accepts "inf"/"NaN", and an infinite price would sort last forever
	// rather than being recognised as unusable, so require a finite positive figure.
	if v, err := strconv.ParseFloat(strings.TrimSpace(p.Hourly.Net), 64); err == nil && v > 0 && !math.IsInf(v, 0) {
		return v, true
	}
	if v, err := strconv.ParseFloat(strings.TrimSpace(p.Monthly.Net), 64); err == nil && v > 0 && !math.IsInf(v, 0) {
		return v / 730, true
	}
	return 0, false
}

// filterByLocations returns only the instance types that have at least one offering in the requested locations.
// If locations is empty, all instance types are returned unchanged.
func filterByLocations(types []*cloudprovider.InstanceType, locations []string) []*cloudprovider.InstanceType {
	if len(locations) == 0 {
		return types
	}
	locSet := make(map[string]struct{}, len(locations))
	for _, l := range locations {
		locSet[l] = struct{}{}
	}

	result := make([]*cloudprovider.InstanceType, 0, len(types))
	for _, it := range types {
		filtered := make(cloudprovider.Offerings, 0, len(it.Offerings))
		for _, o := range it.Offerings {
			zone := o.Requirements.Get(corev1.LabelTopologyZone).Any()
			if _, ok := locSet[zone]; ok {
				filtered = append(filtered, o)
			}
		}
		if len(filtered) > 0 {
			result = append(result, &cloudprovider.InstanceType{
				Name:         it.Name,
				Offerings:    filtered,
				Requirements: it.Requirements,
				Capacity:     it.Capacity,
				Overhead:     it.Overhead,
			})
		}
	}
	return result
}
