package instancetype

import (
	"context"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
)

// mockServerTypeClient is a fake ServerTypeClient for testing.
type mockServerTypeClient struct {
	types []*hcloud.ServerType
	calls int
}

func (m *mockServerTypeClient) All(_ context.Context) ([]*hcloud.ServerType, error) {
	m.calls++
	return m.types, nil
}

func makeServerType(name string, arch hcloud.Architecture, cpuType hcloud.CPUType, cores int, memGB float32, diskGB int, pricings []hcloud.ServerTypeLocationPricing) *hcloud.ServerType {
	return &hcloud.ServerType{
		ID:           1,
		Name:         name,
		Architecture: arch,
		CPUType:      cpuType,
		Cores:        cores,
		Memory:       memGB,
		Disk:         diskGB,
		Pricings:     pricings,
	}
}

var testPricings = []hcloud.ServerTypeLocationPricing{
	{
		Location: &hcloud.Location{Name: "nbg1"},
		Monthly:  hcloud.Price{Net: "7.3000000000"},
	},
	{
		Location: &hcloud.Location{Name: "fsn1"},
		Monthly:  hcloud.Price{Net: "7.3000000000"},
	},
}

func TestList_NoLocationFilter(t *testing.T) {
	st := makeServerType("cx11", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 1, 2, 20, testPricings)
	client := &mockServerTypeClient{types: []*hcloud.ServerType{st}}
	p := NewProvider(client)

	types, err := p.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(types) != 1 {
		t.Fatalf("expected 1 instance type, got %d", len(types))
	}
	if types[0].Name != "cx11" {
		t.Errorf("expected name cx11, got %s", types[0].Name)
	}
}

func TestList_LocationFilter(t *testing.T) {
	st := makeServerType("cx11", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 1, 2, 20, testPricings)
	client := &mockServerTypeClient{types: []*hcloud.ServerType{st}}
	p := NewProvider(client)

	// Only request nbg1; fsn1 offering should be filtered out but type still returned.
	types, err := p.List(context.Background(), []string{"nbg1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(types) != 1 {
		t.Fatalf("expected 1 type, got %d", len(types))
	}
	if len(types[0].Offerings) != 1 {
		t.Errorf("expected 1 offering (nbg1 only), got %d", len(types[0].Offerings))
	}
}

func TestList_LocationFilterExcludesAll(t *testing.T) {
	st := makeServerType("cx11", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 1, 2, 20, testPricings)
	client := &mockServerTypeClient{types: []*hcloud.ServerType{st}}
	p := NewProvider(client)

	types, err := p.List(context.Background(), []string{"hel1"}) // not in pricings
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(types) != 0 {
		t.Errorf("expected 0 types (no matching location), got %d", len(types))
	}
}

func TestInstanceType_Capacity(t *testing.T) {
	st := makeServerType("cx21", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 2, 4, 40, testPricings)
	it := toInstanceType(st)

	cpu := it.Capacity[corev1.ResourceCPU]
	if cpu.Value() != 2 {
		t.Errorf("expected 2 CPUs, got %v", cpu.Value())
	}

	mem := it.Capacity[corev1.ResourceMemory]
	expectedMem := int64(4) * 1024 * 1024 * 1024
	if mem.Value() != expectedMem {
		t.Errorf("expected memory %d bytes, got %d", expectedMem, mem.Value())
	}

	disk := it.Capacity[corev1.ResourceEphemeralStorage]
	expectedDisk := int64(40) * 1024 * 1024 * 1024
	if disk.Value() != expectedDisk {
		t.Errorf("expected disk %d bytes, got %d", expectedDisk, disk.Value())
	}

	pods := it.Capacity[corev1.ResourcePods]
	if pods.Value() != 110 {
		t.Errorf("expected 110 pods, got %d", pods.Value())
	}
}

func TestInstanceType_ArchARM(t *testing.T) {
	st := makeServerType("cax11", hcloud.ArchitectureARM, hcloud.CPUTypeShared, 2, 4, 40, testPricings)
	it := toInstanceType(st)

	archReq := it.Requirements.Get("kubernetes.io/arch")
	if archReq.Any() != "arm64" {
		t.Errorf("expected arm64 arch, got %s", archReq.Any())
	}
}

func TestInstanceType_ArchX86(t *testing.T) {
	st := makeServerType("cx11", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 1, 2, 20, testPricings)
	it := toInstanceType(st)

	archReq := it.Requirements.Get("kubernetes.io/arch")
	if archReq.Any() != "amd64" {
		t.Errorf("expected amd64 arch, got %s", archReq.Any())
	}
}

func TestServerFamily(t *testing.T) {
	cases := []struct {
		name   string
		expect string
	}{
		{"cax11", "cax"},
		{"cax21", "cax"},
		{"cx11", "cx"},
		{"cx21", "cx"},
		{"cpx11", "cpx"},
		{"ccx13", "ccx"},
	}
	for _, tc := range cases {
		got := serverFamily(tc.name)
		if got != tc.expect {
			t.Errorf("serverFamily(%q) = %q, want %q", tc.name, got, tc.expect)
		}
	}
}

func TestHourlyNetPrice(t *testing.T) {
	// Prefer hourly net; fall back to monthly net / 730.
	if got, ok := hourlyNetPrice(hcloud.ServerTypeLocationPricing{
		Hourly:  hcloud.Price{Net: "0.0100"},
		Monthly: hcloud.Price{Net: "7.3000"},
	}); !ok || got != 0.01 {
		t.Errorf("want 0.01 from hourly net, got %v (ok=%v)", got, ok)
	}
	got, ok := hourlyNetPrice(hcloud.ServerTypeLocationPricing{Monthly: hcloud.Price{Net: "7.3000"}})
	if !ok || got < 0.0099 || got > 0.0101 {
		t.Errorf("want ~0.01 from monthly net/730, got %v (ok=%v)", got, ok)
	}
	// Unparseable or non-positive figures must report !ok, never a 0 price: a 0 sorts
	// ahead of every real offering and would win every selection.
	for _, p := range []hcloud.ServerTypeLocationPricing{
		{},
		{Hourly: hcloud.Price{Net: ""}, Monthly: hcloud.Price{Net: ""}},
		{Hourly: hcloud.Price{Net: "0.0000"}, Monthly: hcloud.Price{Net: "0.0000"}},
		{Hourly: hcloud.Price{Net: "n/a"}, Monthly: hcloud.Price{Net: "n/a"}},
	} {
		if got, ok := hourlyNetPrice(p); ok {
			t.Errorf("hourlyNetPrice(%+v) = %v, ok=true; want ok=false", p, got)
		}
	}
}

// offeringFor returns the offering for zone, or nil.
func offeringFor(it *cloudprovider.InstanceType, zone string) *cloudprovider.Offering {
	for _, o := range it.Offerings {
		if o.Zone() == zone {
			return o
		}
	}
	return nil
}

// TestToInstanceType_KeepsUnpricedOfferingUnavailable verifies that a location whose
// price cannot be determined stays in the catalogue but is marked unavailable. Removing
// it entirely would be worse than the zero price it replaced: karpenter core documents
// that Offerings must list every allowed offering even when temporarily unavailable, and
// treats a running node whose offering has vanished as drifted (drift.go
// instanceTypeNotFound), so a transient pricing glitch would voluntarily disrupt and
// replace every healthy node of that type in that location. Availability, not
// membership, is what keeps it out of selection.
func TestToInstanceType_KeepsUnpricedOfferingUnavailable(t *testing.T) {
	st := makeServerType("cx22", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 2, 4, 40,
		[]hcloud.ServerTypeLocationPricing{
			{Location: &hcloud.Location{Name: "nbg1"}, Hourly: hcloud.Price{Net: "0.0070"}},
			{Location: &hcloud.Location{Name: "hel1"}},
		})
	it := toInstanceType(st)
	if len(it.Offerings) != 2 {
		t.Fatalf("expected both offerings to remain in the catalogue, got %d", len(it.Offerings))
	}
	hel1 := offeringFor(it, "hel1")
	if hel1 == nil {
		t.Fatal("hel1 offering was dropped; running nodes there would drift")
	}
	if hel1.Available {
		t.Error("unpriced hel1 offering must be unavailable so it is never selected")
	}
	if nbg1 := offeringFor(it, "nbg1"); nbg1 == nil || !nbg1.Available {
		t.Errorf("priced nbg1 offering should stay available, got %+v", nbg1)
	}
}

// TestToInstanceType_IgnoresLocationAvailableFlag pins a deliberate decision: hcloud's
// ServerType.Locations[].Available is NOT used to gate offering availability, because it
// is wrong in both directions. False negatives: observed reading false for (cx53, nbg1)
// 50 minutes after the API accepted a cx53 create there, and across all three eu-central
// locations while eight cx53 nodes were running. False positives: a datacenter reporting
// cx43 as available and available_for_migration still rejected the create with
// resource_unavailable. Gating on it drops good offerings out of ranking, so a 32Gi pod
// falls through cx53 (EUR 29.49) to cpx62 (EUR 129.99). Withdrawn pairs are handled
// reactively by the unavailable cache, which marks a pair only after a real create
// failure and so cannot be fooled either way.
func TestToInstanceType_IgnoresLocationAvailableFlag(t *testing.T) {
	st := makeServerType("cx53", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 16, 32, 320,
		[]hcloud.ServerTypeLocationPricing{
			{Location: &hcloud.Location{Name: "nbg1"}, Hourly: hcloud.Price{Net: "0.0404"}},
		})
	// What the API actually reports while cx53 nodes are running in nbg1.
	st.Locations = []hcloud.ServerTypeLocation{
		{Location: &hcloud.Location{Name: "nbg1"}, Available: false},
	}
	it := toInstanceType(st)
	o := offeringFor(it, "nbg1")
	if o == nil {
		t.Fatal("nbg1 offering missing")
	}
	if !o.Available {
		t.Error("gated on Locations[].Available: a priced, creatable offering was excluded " +
			"from ranking, which falls through to a far pricier type")
	}
}

// TestToInstanceType_RetiredLocationUnavailable: what hcloud reports for cpx41 -- still
// priced everywhere, deprecated per location. Past unavailable_after every create fails
// with "unsupported location for server type"; before it the type is still orderable.
func TestToInstanceType_RetiredLocationUnavailable(t *testing.T) {
	st := makeServerType("cpx41", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 8, 16, 240,
		[]hcloud.ServerTypeLocationPricing{
			{Location: &hcloud.Location{Name: "fsn1"}, Hourly: hcloud.Price{Net: "0.0400"}},
			{Location: &hcloud.Location{Name: "hel1"}, Hourly: hcloud.Price{Net: "0.0400"}},
			{Location: &hcloud.Location{Name: "ash"}, Hourly: hcloud.Price{Net: "0.0400"}},
		})
	deprecated := func(unavailableAfter time.Time) hcloud.DeprecatableResource {
		return hcloud.DeprecatableResource{Deprecation: &hcloud.DeprecationInfo{UnavailableAfter: unavailableAfter}}
	}
	st.Locations = []hcloud.ServerTypeLocation{
		{Location: &hcloud.Location{Name: "fsn1"}, DeprecatableResource: deprecated(time.Now().Add(-time.Hour))},
		{Location: &hcloud.Location{Name: "hel1"}, DeprecatableResource: deprecated(time.Now().Add(time.Hour))},
		{Location: &hcloud.Location{Name: "ash"}},
	}
	it := toInstanceType(st)
	if o := offeringFor(it, "fsn1"); o == nil || o.Available {
		t.Errorf("fsn1 is past unavailable_after: offering must stay listed but unavailable, got %+v", o)
	}
	for _, zone := range []string{"hel1", "ash"} {
		if o := offeringFor(it, zone); o == nil || !o.Available {
			t.Errorf("%s is still orderable: offering must stay available, got %+v", zone, o)
		}
	}
}

func TestList_CacheHit(t *testing.T) {
	st := makeServerType("cx11", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 1, 2, 20, testPricings)
	client := &mockServerTypeClient{types: []*hcloud.ServerType{st}}
	p := NewProvider(client)

	_, _ = p.List(context.Background(), nil)
	_, _ = p.List(context.Background(), nil)

	if client.calls != 1 {
		t.Errorf("expected 1 API call (cache should be hit on second call), got %d", client.calls)
	}
}

func TestList_ReflectsUnavailable(t *testing.T) {
	st := makeServerType("cx11", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 1, 2, 20, testPricings)
	client := &mockServerTypeClient{types: []*hcloud.ServerType{st}}
	p := NewProvider(client)

	// Before marking: both offerings (nbg1, fsn1) must be available.
	before, err := p.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(before) != 1 {
		t.Fatalf("expected 1 instance type, got %d", len(before))
	}
	for _, o := range before[0].Offerings {
		zone := o.Requirements.Get(corev1.LabelTopologyZone).Any()
		if !o.Available {
			t.Errorf("before mark: offering %s should be available", zone)
		}
	}

	// Mark cx11/nbg1 unavailable.
	p.MarkUnavailable("cx11", "nbg1")

	// After marking: nbg1 offering must be unavailable, fsn1 must remain available.
	after, err := p.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("expected 1 instance type after mark, got %d", len(after))
	}
	for _, o := range after[0].Offerings {
		zone := o.Requirements.Get(corev1.LabelTopologyZone).Any()
		switch zone {
		case "nbg1":
			if o.Available {
				t.Errorf("offering nbg1 should be unavailable after MarkUnavailable")
			}
		case "fsn1":
			if !o.Available {
				t.Errorf("offering fsn1 should still be available (different location)")
			}
		default:
			t.Errorf("unexpected zone %q in offerings", zone)
		}
	}

	// Verify the original cached structs are NOT mutated (defensive copy check).
	for _, o := range before[0].Offerings {
		zone := o.Requirements.Get(corev1.LabelTopologyZone).Any()
		if zone == "nbg1" && !o.Available {
			t.Error("cached struct was mutated: original nbg1 offering Available should still be true")
		}
	}
}

// TestList_PreservesCatalogueUnavailability verifies that List does not resurrect an
// offering the catalogue marked unavailable. applyAvailability recomputes Available from
// the capacity cache on every call; if it overwrote rather than ANDed, an unpriced or
// hcloud-withdrawn offering would come back available on the very next List and be
// selected as the cheapest -- silently undoing the catalogue-level guard.
func TestList_PreservesCatalogueUnavailability(t *testing.T) {
	// hel1 carries no usable price, so it cannot be ranked and must stay unavailable.
	st := makeServerType("cx22", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 2, 4, 40,
		[]hcloud.ServerTypeLocationPricing{
			{Location: &hcloud.Location{Name: "nbg1"}, Hourly: hcloud.Price{Net: "0.0070"}},
			{Location: &hcloud.Location{Name: "hel1"}},
		})
	p := NewProvider(&mockServerTypeClient{types: []*hcloud.ServerType{st}})

	types, err := p.List(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(types) != 1 {
		t.Fatalf("expected 1 instance type, got %d", len(types))
	}
	hel1 := offeringFor(types[0], "hel1")
	if hel1 == nil {
		t.Fatal("hel1 offering missing from List output")
	}
	if hel1.Available {
		t.Error("List resurrected the unpriced hel1 offering; a 0 price would win cheapest-first")
	}
	if nbg1 := offeringFor(types[0], "nbg1"); nbg1 == nil || !nbg1.Available {
		t.Errorf("nbg1 should remain available through List, got %+v", nbg1)
	}
}
