package instancetype

import (
	"context"
	"os"
	"regexp"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
)

// listOne lists a single server type through the provider for the given node class.
func listOne(t *testing.T, st *hcloud.ServerType, nc *apiv1.HCloudNodeClass) *cloudprovider.InstanceType {
	t.Helper()
	p := NewProvider(&mockServerTypeClient{types: []*hcloud.ServerType{st}})
	types, err := p.List(context.Background(), nc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(types) != 1 {
		t.Fatalf("expected 1 instance type, got %d", len(types))
	}
	return types[0]
}

func kubeletNodeClass(k *apiv1.KubeletConfiguration) *apiv1.HCloudNodeClass {
	return &apiv1.HCloudNodeClass{Spec: apiv1.HCloudNodeClassSpec{Kubelet: k}}
}

// reserved returns capacity minus allocatable for a resource: the total
// overhead Karpenter subtracts.
func reserved(it *cloudprovider.InstanceType, name corev1.ResourceName) int64 {
	c, a := it.Capacity[name], it.Allocatable()[name]
	if name == corev1.ResourceCPU {
		return c.MilliValue() - a.MilliValue()
	}
	return c.Value() - a.Value()
}

// tenPercentOf40Gi is the kubelet's nodefs.available=10% reservation on a 40Gi
// disk: float32(0.1) is slightly above 0.1, so it is 64 bytes more than 4Gi.
const tenPercentOf40Gi = 4294967360

// TestAllocatable_ReservationMatchesRegisteredNode checks the declared
// reservations against what real servers report once registered, captured from
// running clusters whose bootstrap sets system-reserved and kube-reserved at
// 512Mi/200m each plus a 400Mi memory.available hard eviction threshold. The
// kubelet's capacity minus allocatable on those nodes is the reservation it
// applied; declaring the same values must subtract exactly that.
//
// It checks the reservation only. Capacity is still the advertised size, which
// the guest never fully sees (about 280Mi short on a cpx22), so absolute
// allocatable remains overstated until capacity itself is corrected; that is a
// separate change.
//
// The table declares nodefs.available too, because those nodes set it: an
// unlisted signal would be assumed at the kubelet default.
func TestAllocatable_ReservationMatchesRegisteredNode(t *testing.T) {
	nc := kubeletNodeClass(&apiv1.KubeletConfiguration{
		SystemReserved: map[string]apiv1.ReservedQuantity{"cpu": "200m", "memory": "512Mi"},
		KubeReserved:   map[string]apiv1.ReservedQuantity{"cpu": "200m", "memory": "512Mi"},
		EvictionHard:   map[string]apiv1.EvictionThreshold{"memory.available": "400Mi", "nodefs.available": "10%"},
	})
	for _, tc := range []struct {
		name              string
		cores             int
		memGB             float32
		realCapacityKi    int64
		realAllocatableKi int64
	}{
		{"cpx22", 2, 4, 3905948, 2447772},
		{"cx33", 4, 8, 7937224, 6479048},
		{"cx43", 8, 16, 15988560, 14530384},
		{"cx53", 16, 32, 32089152, 30630976},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it := listOne(t, makeServerType(tc.name, hcloud.ArchitectureX86, hcloud.CPUTypeShared, tc.cores, tc.memGB, 80, testPricings), nc)

			if got, want := reserved(it, corev1.ResourceMemory), (tc.realCapacityKi-tc.realAllocatableKi)*1024; got != want {
				t.Errorf("memory reserved %dMi, node reserves %dMi", got>>20, want>>20)
			}
			if got := reserved(it, corev1.ResourceCPU); got != 400 {
				t.Errorf("cpu reserved %dm, node reserves 400m", got)
			}
		})
	}
}

// An absent kubelet block has said nothing about the bootstrap, so it keeps
// exactly the flat 100m/100Mi this provider subtracted before reservations were
// declarable: upgrading must not change any existing node's allocatable.
func TestAllocatable_NoKubeletKeepsLegacyReservation(t *testing.T) {
	for name, nc := range map[string]*apiv1.HCloudNodeClass{
		"nil node class":      nil,
		"no kubelet block":    {},
		"empty kubelet block": kubeletNodeClass(&apiv1.KubeletConfiguration{}),
	} {
		t.Run(name, func(t *testing.T) {
			it := listOne(t, makeServerType("cx23", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 2, 4, 40, testPricings), nc)
			if got := reserved(it, corev1.ResourceCPU); got != 100 {
				t.Errorf("cpu reserved %dm, want 100m", got)
			}
			if got := reserved(it, corev1.ResourceMemory); got != 100<<20 {
				t.Errorf("memory reserved %dMi, want 100Mi", got>>20)
			}
			if got := reserved(it, corev1.ResourceEphemeralStorage); got != 0 {
				t.Errorf("ephemeral-storage reserved %d, want 0", got)
			}
		})
	}
}

// A declared block is taken at its word: the legacy default is replaced, not
// added to, and unset eviction signals use the kubelet's own defaults.
func TestAllocatable_DeclaredKubeletReplacesLegacyDefault(t *testing.T) {
	it := listOne(t, makeServerType("cx23", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 2, 4, 40, testPricings),
		kubeletNodeClass(&apiv1.KubeletConfiguration{KubeReserved: map[string]apiv1.ReservedQuantity{"cpu": "250m"}}))

	if got := reserved(it, corev1.ResourceCPU); got != 250 {
		t.Errorf("cpu reserved %dm, want the declared 250m", got)
	}
	if got := reserved(it, corev1.ResourceMemory); got != 100<<20 {
		t.Errorf("memory reserved %dMi, want the kubelet's default 100Mi eviction threshold", got>>20)
	}
	if got := reserved(it, corev1.ResourceEphemeralStorage); got != tenPercentOf40Gi {
		t.Errorf("ephemeral-storage reserved %d, want 10%% of 40Gi", got)
	}
}

// A signal the node class does not list keeps the kubelet default, which errs
// toward underselling the node; declaring "0%" is how a node class says the
// kubelet enforces nothing for it.
func TestAllocatable_PartialEvictionHard(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hard       map[string]apiv1.EvictionThreshold
		wantNodefs int64
	}{
		{"unlisted nodefs keeps the 10% default", map[string]apiv1.EvictionThreshold{"memory.available": "400Mi"}, tenPercentOf40Gi},
		{"0% disables nodefs", map[string]apiv1.EvictionThreshold{"memory.available": "400Mi", "nodefs.available": "0%"}, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it := listOne(t, makeServerType("cx23", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 2, 4, 40, testPricings),
				kubeletNodeClass(&apiv1.KubeletConfiguration{EvictionHard: tc.hard}))
			if got := reserved(it, corev1.ResourceMemory); got != 400<<20 {
				t.Errorf("memory reserved %dMi, want the declared 400Mi", got>>20)
			}
			if got := reserved(it, corev1.ResourceEphemeralStorage); got != tc.wantNodefs {
				t.Errorf("ephemeral-storage reserved %d, want %d", got, tc.wantNodefs)
			}
		})
	}
}

func TestAllocatable_EvictionHard(t *testing.T) {
	for _, tc := range []struct {
		name, value string
		want        int64
	}{
		{"quantity", "1Gi", 1 << 30},
		{"percentage of capacity", "25%", 1 << 30},
		// The kubelet works in float32: 7.5% of 4Gi is 322122560 bytes there, not
		// the 322122547 exact arithmetic gives, and Karpenter must match it.
		{"fractional percentage in the kubelet's float32", "7.5%", 322122560},
		{"leading-dot percentage", ".5%", 21474836},
		// The kubelet treats exactly "0%" and "100%" as no threshold; reserving
		// all of capacity for "100%" would leave nothing schedulable.
		{"0% is no threshold", "0%", 0},
		{"100% is no threshold", "100%", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it := listOne(t, makeServerType("cx23", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 2, 4, 40, testPricings),
				kubeletNodeClass(&apiv1.KubeletConfiguration{EvictionHard: map[string]apiv1.EvictionThreshold{"memory.available": apiv1.EvictionThreshold(tc.value)}}))
			if got := reserved(it, corev1.ResourceMemory); got != tc.want {
				t.Errorf("memory reserved %d, want %d", got, tc.want)
			}
		})
	}
}

// TestCRDPatterns pins what admission accepts. The API server checks these
// OpenAPI patterns with Go's regexp, so compiling the generated CRD's patterns
// here is the same check without a control plane.
func TestCRDPatterns(t *testing.T) {
	raw, err := os.ReadFile("../../../charts/karpenter-provider-hetzner/crds/karpenter.hetzner.cloud_hcloudnodeclasses.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	kubelet := crd.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"].Properties["kubelet"].Properties
	pattern := func(field string) *regexp.Regexp {
		return regexp.MustCompile(kubelet[field].AdditionalProperties.Schema.Pattern)
	}
	for field, cases := range map[string]map[string]bool{
		"kubeReserved": {"512Mi": true, "200m": true, "1": true, "0.5": true, "0": true, "1e3": true, "1e-3": true,
			"512MB": false, "-1": false, "10%": false, "": false, " 1Gi": false, "1e1.5": false, "1e.5": false, "1e999": true, "1e99999999999999999999": false, "+512Mi": true, "+": false},
		"systemReserved": {"512Mi": true, "200m": true, "1": true, "0.5": true, "0": true, "1e3": true, "1e-3": true,
			"512MB": false, "-1": false, "10%": false, "": false, " 1Gi": false, "1e1.5": false, "1e.5": false, "1e999": true, "1e99999999999999999999": false, "+512Mi": true, "+": false},
		"evictionHard": {"400Mi": true, "+400Mi": true, "+10%": false, "1e3": true, "1e1.5": false, "10%": true, "7.5%": true, ".5%": true, "5.%": true, "0%": true, "100%": true,
			"<400Mi": false, "101%": false, "-1Gi": false, "10 %": false, "400MB": false, "": false},
	} {
		re := pattern(field)
		for v, want := range cases {
			if got := re.MatchString(v); got != want {
				t.Errorf("%s %q: admitted=%v, want %v", field, v, got, want)
			}
			// Anything admitted must also parse, or the provider would silently
			// drop or replace it.
			if want {
				if _, err := resolveThreshold(v, resource.MustParse("4Gi")); err != nil {
					t.Errorf("%s %q is admitted but does not parse: %v", field, v, err)
				}
			}
		}
	}
}

// Overhead is computed per node class on top of a catalogue cached for all of
// them, so one class's reservations must never show up in another's types.
func TestAllocatable_PerNodeClassOnSharedCatalogue(t *testing.T) {
	p := NewProvider(&mockServerTypeClient{types: []*hcloud.ServerType{
		makeServerType("cx23", hcloud.ArchitectureX86, hcloud.CPUTypeShared, 2, 4, 40, testPricings),
	}})
	declared := kubeletNodeClass(&apiv1.KubeletConfiguration{KubeReserved: map[string]apiv1.ReservedQuantity{"cpu": "500m"}})
	for i, tc := range []struct {
		nc      *apiv1.HCloudNodeClass
		wantCPU int64
	}{{declared, 500}, {&apiv1.HCloudNodeClass{}, 100}, {declared, 500}} {
		types, err := p.List(context.Background(), tc.nc)
		if err != nil {
			t.Fatal(err)
		}
		if got := reserved(types[0], corev1.ResourceCPU); got != tc.wantCPU {
			t.Errorf("call %d: cpu reserved %dm, want %dm", i, got, tc.wantCPU)
		}
	}
}
