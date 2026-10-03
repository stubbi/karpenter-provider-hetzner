package cloudprovider_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/cloudprovider"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/imagefamily"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instance"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instancetype"
)

func newTestCloudProvider() *cloudprovider.CloudProvider {
	return cloudprovider.NewCloudProvider(nil, nil, nil, nil)
}

func TestName(t *testing.T) {
	cp := newTestCloudProvider()
	if got := cp.Name(); got != "hetzner" {
		t.Errorf("Name() = %q, want %q", got, "hetzner")
	}
}

func TestGetSupportedNodeClasses(t *testing.T) {
	cp := newTestCloudProvider()
	classes := cp.GetSupportedNodeClasses()
	if len(classes) != 1 {
		t.Fatalf("GetSupportedNodeClasses() returned %d classes, want 1", len(classes))
	}
	if _, ok := classes[0].(*apiv1.HCloudNodeClass); !ok {
		t.Errorf("GetSupportedNodeClasses()[0] is not *apiv1.HCloudNodeClass")
	}
}

func TestRepairPolicies(t *testing.T) {
	cp := newTestCloudProvider()
	policies := cp.RepairPolicies()
	if len(policies) != 2 {
		t.Fatalf("RepairPolicies() returned %d policies, want 2", len(policies))
	}
}

// ---------------------------------------------------------------------------
// Fake clients
// ---------------------------------------------------------------------------

type fakeServerClient struct {
	servers   map[int64]*hcloud.Server
	createErr error
	nextID    int64
	lastOpts  hcloud.ServerCreateOpts
}

func (f *fakeServerClient) lastUserData() string {
	return f.lastOpts.UserData
}

func (f *fakeServerClient) Create(_ context.Context, opts hcloud.ServerCreateOpts) (hcloud.ServerCreateResult, *hcloud.Response, error) {
	f.lastOpts = opts
	if f.createErr != nil {
		return hcloud.ServerCreateResult{}, nil, f.createErr
	}
	if f.servers == nil {
		f.servers = map[int64]*hcloud.Server{}
	}
	if f.nextID == 0 {
		f.nextID = 100
	}
	id := f.nextID
	f.nextID++
	s := &hcloud.Server{ID: id, Name: opts.Name, Labels: opts.Labels, ServerType: opts.ServerType, Location: opts.Location}
	f.servers[id] = s
	return hcloud.ServerCreateResult{Server: s}, nil, nil
}

func (f *fakeServerClient) DeleteWithResult(_ context.Context, server *hcloud.Server) (*hcloud.ServerDeleteResult, *hcloud.Response, error) {
	delete(f.servers, server.ID)
	return &hcloud.ServerDeleteResult{}, nil, nil
}

func (f *fakeServerClient) GetByID(_ context.Context, id int64) (*hcloud.Server, *hcloud.Response, error) {
	return f.servers[id], nil, nil
}

func (f *fakeServerClient) AllWithOpts(_ context.Context, _ hcloud.ServerListOpts) ([]*hcloud.Server, error) {
	out := make([]*hcloud.Server, 0, len(f.servers))
	for _, s := range f.servers {
		out = append(out, s)
	}
	return out, nil
}

type fakeServerTypeClient struct{ types []*hcloud.ServerType }

func (f *fakeServerTypeClient) All(_ context.Context) ([]*hcloud.ServerType, error) {
	return f.types, nil
}

type fakeImageClient struct{ images []*hcloud.Image }

func (f *fakeImageClient) AllWithOpts(_ context.Context, opts hcloud.ImageListOpts) ([]*hcloud.Image, error) {
	if len(opts.Architecture) == 0 {
		return f.images, nil
	}
	// hcloud filters images by architecture server-side and the provider never
	// re-filters client-side, so a fake that ignores this hands back wrong-arch images
	// and hides every arch-related failure.
	var out []*hcloud.Image
	for _, img := range f.images {
		if slices.Contains(opts.Architecture, img.Architecture) {
			out = append(out, img)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// buildCP builds a CloudProvider whose NodeClass is nc and whose single backing
// server is server, plus a NodeClaim whose desired state matches the server
// (BASELINE has no drift; each test perturbs one thing).
func buildCP(t *testing.T, nc *apiv1.HCloudNodeClass, server *hcloud.Server) (*cloudprovider.CloudProvider, *karpv1.NodeClaim) {
	t.Helper()
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	if nc.Name == "" {
		nc.Name = "default"
	}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(nc).Build()

	fsc := &fakeServerClient{servers: map[int64]*hcloud.Server{server.ID: server}}
	stc := &fakeServerTypeClient{}
	imgc := &fakeImageClient{images: []*hcloud.Image{{ID: 42, Description: "Ubuntu 24.04", Architecture: hcloud.ArchitectureX86}}}

	cp := cloudprovider.NewCloudProvider(kube,
		instance.NewProvider(fsc, "test-cluster"),
		instancetype.NewProvider(stc),
		imagefamily.NewProvider(imgc))

	nodeClaim := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim"}}
	nodeClaim.Labels = map[string]string{}
	if server.ServerType != nil {
		nodeClaim.Labels[corev1.LabelInstanceTypeStable] = server.ServerType.Name
	}
	nodeClaim.Spec.NodeClassRef = &karpv1.NodeClassReference{Name: nc.Name, Group: apiv1.Group, Kind: "HCloudNodeClass"}
	nodeClaim.Status.ProviderID = instance.FormatProviderID(server.ID)
	if server.Image != nil {
		nodeClaim.Status.ImageID = strconv.FormatInt(server.Image.ID, 10)
	}
	return cp, nodeClaim
}

// baselineServer returns a server with no drift relative to a baseline NodeClass:
// type cx22, image 42, attached to network 1, no firewalls.
func baselineServer() *hcloud.Server {
	return &hcloud.Server{
		ID:         50,
		ServerType: &hcloud.ServerType{Name: "cx22"},
		Image:      &hcloud.Image{ID: 42},
		PrivateNet: []hcloud.ServerPrivateNet{{Network: &hcloud.Network{ID: 1}}},
	}
}

// baselineNodeClass returns a NodeClass matching baselineServer (network 1, no firewalls).
func baselineNodeClass() *apiv1.HCloudNodeClass {
	return &apiv1.HCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: apiv1.HCloudNodeClassSpec{
			Locations:     []string{"nbg1"},
			NetworkID:     1,
			ImageSelector: apiv1.ImageSelector{Family: "ubuntu"},
		},
	}
}

// cx22Type returns a representative shared-vCPU x86 server type with a single
// nbg1 offering, used by the Create/List/Get/GetInstanceTypes tests.
func cx22Type() *hcloud.ServerType {
	return &hcloud.ServerType{
		Name:         "cx22",
		Cores:        2,
		Memory:       4,
		Disk:         40,
		Architecture: hcloud.ArchitectureX86,
		CPUType:      hcloud.CPUTypeShared,
		Pricings: []hcloud.ServerTypeLocationPricing{
			{
				Location: &hcloud.Location{Name: "nbg1"},
				Hourly:   hcloud.Price{Net: "0.0070"},
				Monthly:  hcloud.Price{Net: "4.5100"},
			},
		},
	}
}

// buildCPWithTypes wires a CloudProvider with the given NodeClass and server types,
// and returns the fakes so tests can inject errors / inspect state. Unlike buildCP
// it does NOT pre-seed a backing server.
func buildCPWithTypes(t *testing.T, nc *apiv1.HCloudNodeClass, types []*hcloud.ServerType) (
	*cloudprovider.CloudProvider, *fakeServerClient, *instancetype.Provider) {
	t.Helper()
	return buildCPWithImages(t, nc, types,
		[]*hcloud.Image{{ID: 42, Description: "Ubuntu 24.04", Architecture: hcloud.ArchitectureX86}})
}

// buildCPWithImages is buildCPWithTypes with an explicit image catalogue, for tests that
// need the live image lookup to fail or to return something other than image 42.
func buildCPWithImages(t *testing.T, nc *apiv1.HCloudNodeClass, types []*hcloud.ServerType,
	images []*hcloud.Image) (*cloudprovider.CloudProvider, *fakeServerClient, *instancetype.Provider) {
	t.Helper()
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	if nc.Name == "" {
		nc.Name = "default"
	}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(nc).Build()
	fsc := &fakeServerClient{servers: map[int64]*hcloud.Server{}}
	stc := &fakeServerTypeClient{types: types}
	imgc := &fakeImageClient{images: images}
	typeProvider := instancetype.NewProvider(stc)
	cp := cloudprovider.NewCloudProvider(kube,
		instance.NewProvider(fsc, "test-cluster"),
		typeProvider,
		imagefamily.NewProvider(imgc))
	return cp, fsc, typeProvider
}

// withResolvedImages stamps images onto nc the way a successful reconcile does: entries
// plus an ImagesReady condition carrying the current metadata.generation.
//
// Setting Status.ResolvedImages alone is not equivalent. Consumers gate on the recorded
// generation matching metadata.generation, and a bare NodeClass has generation 0, so
// entries with no condition match by 0 == 0 -- a state no real cluster can be in, since
// generation is >= 1 from creation onward. A test built that way keeps passing even if
// the gate is broken for every generation an apiserver would actually assign.
func withResolvedImages(nc *apiv1.HCloudNodeClass, images ...apiv1.ResolvedImage) *apiv1.HCloudNodeClass {
	nc.Generation = 1
	nc.Status.ResolvedImages = images
	nc.StatusConditions().SetTrue(apiv1.ConditionTypeImagesReady)
	return nc
}

// createNodeClaim returns a NodeClaim with empty requirements (compatible with any type).
func createNodeClaim() *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "claim"}}
	nc.Spec.NodeClassRef = &karpv1.NodeClassReference{Name: "default", Group: apiv1.Group, Kind: "HCloudNodeClass"}
	return nc
}

// ---------------------------------------------------------------------------
// Drift tests
// ---------------------------------------------------------------------------

func TestIsDrifted_NoDrift(t *testing.T) {
	cp, nodeClaim := buildCP(t, baselineNodeClass(), baselineServer())
	reason, err := cp.IsDrifted(context.Background(), nodeClaim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Errorf("expected no drift, got %q", reason)
	}
}

func TestIsDrifted_Firewall(t *testing.T) {
	nc := baselineNodeClass()
	nc.Spec.FirewallIDs = []int64{7}
	server := baselineServer()
	server.PublicNet.Firewalls = []*hcloud.ServerFirewallStatus{{Firewall: hcloud.Firewall{ID: 9}}}
	cp, nodeClaim := buildCP(t, nc, server)
	reason, err := cp.IsDrifted(context.Background(), nodeClaim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != cloudprovider.DriftFirewall {
		t.Errorf("want FirewallDrift, got %q", reason)
	}
}

func TestIsDrifted_FirewallAttached_NoDrift(t *testing.T) {
	nc := baselineNodeClass()
	nc.Spec.FirewallIDs = []int64{7}
	server := baselineServer()
	server.PublicNet.Firewalls = []*hcloud.ServerFirewallStatus{{Firewall: hcloud.Firewall{ID: 7}}}
	cp, nodeClaim := buildCP(t, nc, server)
	reason, err := cp.IsDrifted(context.Background(), nodeClaim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Errorf("expected no drift when firewall attached, got %q", reason)
	}
}

func TestIsDrifted_ServerType(t *testing.T) {
	server := baselineServer()
	server.ServerType = &hcloud.ServerType{Name: "cx32"} // live server is cx32
	cp, nodeClaim := buildCP(t, baselineNodeClass(), server)
	nodeClaim.Labels[corev1.LabelInstanceTypeStable] = "cx22" // desired was cx22
	reason, err := cp.IsDrifted(context.Background(), nodeClaim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != cloudprovider.DriftServerType {
		t.Errorf("want ServerTypeDrift, got %q", reason)
	}
}

// ---------------------------------------------------------------------------
// Create / Delete / Get / List / GetInstanceTypes tests
// ---------------------------------------------------------------------------

func TestCreate_Success(t *testing.T) {
	cp, _, _ := buildCPWithTypes(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type()})
	out, err := cp.Create(context.Background(), createNodeClaim())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if out.Status.ProviderID == "" || !strings.HasPrefix(out.Status.ProviderID, "hcloud://") {
		t.Errorf("expected hcloud provider ID, got %q", out.Status.ProviderID)
	}
	if out.Status.Capacity.Cpu().IsZero() {
		t.Error("expected non-zero CPU capacity")
	}
}

func TestCreate_InsufficientCapacityMarksUnavailable(t *testing.T) {
	cp, fsc, typeProvider := buildCPWithTypes(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type()})
	fsc.createErr = hcloud.Error{Code: hcloud.ErrorCodeResourceUnavailable}

	_, err := cp.Create(context.Background(), createNodeClaim())
	if err == nil {
		t.Fatal("expected error on capacity failure")
	}
	// The offering for (cx22, nbg1) should now be marked unavailable.
	its, lerr := typeProvider.List(context.Background(), []string{"nbg1"})
	if lerr != nil {
		t.Fatal(lerr)
	}
	for _, it := range its {
		if it.Name != "cx22" {
			continue
		}
		for _, o := range it.Offerings {
			if o.Available {
				t.Error("expected cx22/nbg1 offering to be unavailable after capacity error")
			}
		}
	}
}

func TestGet_NotFound(t *testing.T) {
	cp, _, _ := buildCPWithTypes(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type()})
	_, err := cp.Get(context.Background(), instance.FormatProviderID(999))
	if !karpcp.IsNodeClaimNotFoundError(err) {
		t.Errorf("expected NodeClaimNotFoundError, got %v", err)
	}
}

func TestGet_Found(t *testing.T) {
	cp, fsc, _ := buildCPWithTypes(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type()})
	fsc.servers[55] = &hcloud.Server{ID: 55, ServerType: &hcloud.ServerType{Name: "cx22"}}
	nc, err := cp.Get(context.Background(), instance.FormatProviderID(55))
	if err != nil {
		t.Fatal(err)
	}
	if nc.Status.ProviderID != instance.FormatProviderID(55) {
		t.Errorf("got provider ID %q", nc.Status.ProviderID)
	}
}

func TestList_ReturnsManagedServers(t *testing.T) {
	cp, fsc, _ := buildCPWithTypes(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type()})
	fsc.servers[1] = &hcloud.Server{ID: 1, ServerType: &hcloud.ServerType{Name: "cx22"}}
	fsc.servers[2] = &hcloud.Server{ID: 2, ServerType: &hcloud.ServerType{Name: "cx22"}}
	list, err := cp.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 {
		t.Errorf("expected 2 nodeclaims, got %d", len(list))
	}
}

func TestDelete_RemovesServer(t *testing.T) {
	cp, fsc, _ := buildCPWithTypes(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type()})
	fsc.servers[77] = &hcloud.Server{ID: 77, Labels: map[string]string{
		apiv1.ServerLabelManagedBy: apiv1.ServerValueManagedBy,
		apiv1.ServerLabelCluster:   "test-cluster",
	}}
	nodeClaim := &karpv1.NodeClaim{}
	nodeClaim.Status.ProviderID = instance.FormatProviderID(77)
	if err := cp.Delete(context.Background(), nodeClaim); err != nil {
		t.Fatal(err)
	}
	if _, ok := fsc.servers[77]; ok {
		t.Error("expected server 77 to be deleted")
	}
}

func TestGetInstanceTypes_NilNodePool(t *testing.T) {
	cp, _, _ := buildCPWithTypes(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type()})
	its, err := cp.GetInstanceTypes(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(its) == 0 {
		t.Error("expected at least one instance type")
	}
}

// ---------------------------------------------------------------------------
// Edge-case tests
// ---------------------------------------------------------------------------

// TestDelete_NotFoundReturnsNodeClaimNotFound verifies that deleting a NodeClaim
// whose backing server no longer exists surfaces a NodeClaimNotFoundError — the
// signal Karpenter's termination controller uses to finalize the NodeClaim.
func TestDelete_NotFoundReturnsNodeClaimNotFound(t *testing.T) {
	cp, _, _ := buildCPWithTypes(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type()})
	nodeClaim := &karpv1.NodeClaim{}
	nodeClaim.Status.ProviderID = instance.FormatProviderID(12345) // not seeded
	// Deleting an already-gone server must surface as NodeClaimNotFoundError so
	// Karpenter's termination controller can finalize the NodeClaim instead of
	// requeueing forever (which leaks the NodeClaim).
	if err := cp.Delete(context.Background(), nodeClaim); !karpcp.IsNodeClaimNotFoundError(err) {
		t.Errorf("expected NodeClaimNotFoundError for missing server, got %v", err)
	}
}

// TestGet_NilServerType verifies Get does not panic when the live server has a
// nil ServerType (e.g. mid-provisioning); capacity may be empty in that case.
func TestGet_NilServerType(t *testing.T) {
	cp, fsc, _ := buildCPWithTypes(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type()})
	fsc.servers[88] = &hcloud.Server{ID: 88} // ServerType nil (e.g. mid-provisioning)
	nc, err := cp.Get(context.Background(), instance.FormatProviderID(88))
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if nc.Status.ProviderID != instance.FormatProviderID(88) {
		t.Errorf("expected provider ID set, got %q", nc.Status.ProviderID)
	}
	// Capacity may be empty when ServerType is nil; the call must not panic.
}

// TestCreate_NoCompatibleType verifies Create returns an InsufficientCapacityError
// when no instance type satisfies the NodeClaim requirements (selected == nil).
func TestCreate_NoCompatibleType(t *testing.T) {
	cp, _, _ := buildCPWithTypes(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type()})
	nodeClaim := createNodeClaim()
	nodeClaim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
		{
			Key:      corev1.LabelInstanceTypeStable,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{"does-not-exist"},
		},
	}
	_, err := cp.Create(context.Background(), nodeClaim)
	if !karpcp.IsInsufficientCapacityError(err) {
		t.Errorf("expected InsufficientCapacityError when no type matches, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// UserData secret resolution tests
// ---------------------------------------------------------------------------

// TestCreate_UserDataFromSecret verifies that when UserDataSecretRef is set, the
// userData passed to the server-create call comes from the referenced Secret, NOT
// from the inline UserData field.
func TestCreate_UserDataFromSecret(t *testing.T) {
	nc := baselineNodeClass()
	nc.Spec.UserData = "inline-should-be-ignored"
	nc.Spec.UserDataSecretRef = &apiv1.UserDataSecretReference{
		Namespace: "kube-system",
		Name:      "talos",
		Key:       "userData",
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "talos", Namespace: "kube-system"},
		Data:       map[string][]byte{"userData": []byte("machine:\n  type: worker\n")},
	}

	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(nc, secret).Build()
	fsc := &fakeServerClient{servers: map[int64]*hcloud.Server{}}
	stc := &fakeServerTypeClient{types: []*hcloud.ServerType{cx22Type()}}
	imgc := &fakeImageClient{images: []*hcloud.Image{{ID: 42, Description: "Ubuntu 24.04", Architecture: hcloud.ArchitectureX86}}}
	cp := cloudprovider.NewCloudProvider(kube,
		instance.NewProvider(fsc, "test-cluster"),
		instancetype.NewProvider(stc),
		imagefamily.NewProvider(imgc))

	if _, err := cp.Create(context.Background(), createNodeClaim()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := fsc.lastUserData(); got != "machine:\n  type: worker\n" {
		t.Errorf("expected userData from secret, got %q", got)
	}
}

// TestCreate_UserDataInlineWhenNoRef verifies that when UserDataSecretRef is nil,
// the inline UserData field is passed through to the server-create call unchanged.
func TestCreate_UserDataInlineWhenNoRef(t *testing.T) {
	nc := baselineNodeClass()
	nc.Spec.UserData = "cloud-init-inline"
	// UserDataSecretRef is intentionally left nil.

	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(nc).Build()
	fsc := &fakeServerClient{servers: map[int64]*hcloud.Server{}}
	stc := &fakeServerTypeClient{types: []*hcloud.ServerType{cx22Type()}}
	imgc := &fakeImageClient{images: []*hcloud.Image{{ID: 42, Description: "Ubuntu 24.04", Architecture: hcloud.ArchitectureX86}}}
	cp := cloudprovider.NewCloudProvider(kube,
		instance.NewProvider(fsc, "test-cluster"),
		instancetype.NewProvider(stc),
		imagefamily.NewProvider(imgc))

	if _, err := cp.Create(context.Background(), createNodeClaim()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := fsc.lastUserData(); got != "cloud-init-inline" {
		t.Errorf("expected inline userData %q, got %q", "cloud-init-inline", got)
	}
}

// TestCreate_UserDataSecretKeyMissing verifies that Create returns an error when
// UserDataSecretRef points to a Secret that exists but does not contain the
// referenced key (key absent == invalid, same as secret missing).
func TestCreate_UserDataSecretKeyMissing(t *testing.T) {
	nc := baselineNodeClass()
	nc.Spec.UserDataSecretRef = &apiv1.UserDataSecretReference{
		Namespace: "kube-system",
		Name:      "talos",
		Key:       "userData",
	}
	// Secret exists but contains a different key — "userData" is absent.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "talos", Namespace: "kube-system"},
		Data:       map[string][]byte{"other": []byte("x")},
	}

	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(nc, secret).Build()
	fsc := &fakeServerClient{servers: map[int64]*hcloud.Server{}}
	stc := &fakeServerTypeClient{types: []*hcloud.ServerType{cx22Type()}}
	imgc := &fakeImageClient{images: []*hcloud.Image{{ID: 42, Description: "Ubuntu 24.04", Architecture: hcloud.ArchitectureX86}}}
	cp := cloudprovider.NewCloudProvider(kube,
		instance.NewProvider(fsc, "test-cluster"),
		instancetype.NewProvider(stc),
		imagefamily.NewProvider(imgc))

	_, err := cp.Create(context.Background(), createNodeClaim())
	if err == nil {
		t.Fatal("expected error when secret exists but referenced key is absent, got nil")
	}
}

// TestCreate_UserDataSecretMissing verifies that Create returns an error when
// UserDataSecretRef points to a Secret that does not exist in the cluster.
func TestCreate_UserDataSecretMissing(t *testing.T) {
	nc := baselineNodeClass()
	nc.Spec.UserDataSecretRef = &apiv1.UserDataSecretReference{
		Namespace: "kube-system",
		Name:      "does-not-exist",
		Key:       "userData",
	}

	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	// Secret is intentionally NOT added to the fake client.
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(nc).Build()
	fsc := &fakeServerClient{servers: map[int64]*hcloud.Server{}}
	stc := &fakeServerTypeClient{types: []*hcloud.ServerType{cx22Type()}}
	imgc := &fakeImageClient{images: []*hcloud.Image{{ID: 42, Description: "Ubuntu 24.04", Architecture: hcloud.ArchitectureX86}}}
	cp := cloudprovider.NewCloudProvider(kube,
		instance.NewProvider(fsc, "test-cluster"),
		instancetype.NewProvider(stc),
		imagefamily.NewProvider(imgc))

	_, err := cp.Create(context.Background(), createNodeClaim())
	if err == nil {
		t.Fatal("expected error when userDataSecretRef points to a missing Secret, got nil")
	}
}

// ---------------------------------------------------------------------------
// DriftNetwork test (the detection code exists but was untested)
// ---------------------------------------------------------------------------

// TestIsDrifted_Network verifies that a server not attached to the NodeClass
// network is flagged as DriftNetwork.
func TestIsDrifted_Network(t *testing.T) {
	nc := baselineNodeClass() // expects NetworkID=1
	server := baselineServer()
	// Replace the attached network with a different ID.
	server.PrivateNet = []hcloud.ServerPrivateNet{{Network: &hcloud.Network{ID: 99}}}
	cp, nodeClaim := buildCP(t, nc, server)
	reason, err := cp.IsDrifted(context.Background(), nodeClaim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != cloudprovider.DriftNetwork {
		t.Errorf("want DriftNetwork, got %q", reason)
	}
}

// TestIsDrifted_NetworkAttached_NoDrift verifies that a server correctly
// attached to the NodeClass network is not flagged as drifted.
func TestIsDrifted_NetworkAttached_NoDrift(t *testing.T) {
	nc := baselineNodeClass() // expects NetworkID=1
	server := baselineServer()
	server.PrivateNet = []hcloud.ServerPrivateNet{{Network: &hcloud.Network{ID: 1}}}
	cp, nodeClaim := buildCP(t, nc, server)
	reason, err := cp.IsDrifted(context.Background(), nodeClaim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Errorf("expected no drift when network correctly attached, got %q", reason)
	}
}

// ---------------------------------------------------------------------------
// Location drift tests
// ---------------------------------------------------------------------------

// TestIsDrifted_Location verifies that a server whose datacenter location is
// not in the NodeClass Locations list is flagged as DriftLocation.
func TestIsDrifted_Location(t *testing.T) {
	nc := baselineNodeClass() // Locations: ["nbg1"]
	server := baselineServer()
	// Server is in "hel1" which is not in the NodeClass locations.
	server.Location = &hcloud.Location{Name: "hel1"}
	cp, nodeClaim := buildCP(t, nc, server)
	reason, err := cp.IsDrifted(context.Background(), nodeClaim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != cloudprovider.DriftLocation {
		t.Errorf("want DriftLocation, got %q", reason)
	}
}

// TestIsDrifted_LocationInList_NoDrift verifies that a server in a location
// that is present in the NodeClass Locations list is not flagged as drifted.
func TestIsDrifted_LocationInList_NoDrift(t *testing.T) {
	nc := baselineNodeClass() // Locations: ["nbg1"]
	server := baselineServer()
	server.Location = &hcloud.Location{Name: "nbg1"}
	cp, nodeClaim := buildCP(t, nc, server)
	reason, err := cp.IsDrifted(context.Background(), nodeClaim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Errorf("expected no drift when location is in list, got %q", reason)
	}
}

// TestIsDrifted_Location_NilLocation_NoDrift verifies that a server with a nil
// Location (e.g. mid-provisioning) does not report location drift.
func TestIsDrifted_Location_NilLocation_NoDrift(t *testing.T) {
	nc := baselineNodeClass()
	server := baselineServer()
	server.Location = nil // nil location -> guard should skip the check
	cp, nodeClaim := buildCP(t, nc, server)
	reason, err := cp.IsDrifted(context.Background(), nodeClaim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Errorf("expected no drift for nil Location, got %q", reason)
	}
}

// ---------------------------------------------------------------------------
// Label drift tests (B3)
// ---------------------------------------------------------------------------

// TestIsDrifted_Labels_Missing verifies that when NodeClass spec.labels contains
// a key that is absent from the server labels, DriftLabels is returned.
func TestIsDrifted_Labels_Missing(t *testing.T) {
	nc := baselineNodeClass()
	nc.Spec.Labels = map[string]string{"env": "prod"}
	server := baselineServer()
	// server has no labels at all → "env" key is missing
	server.Labels = map[string]string{}
	cp, nodeClaim := buildCP(t, nc, server)
	reason, err := cp.IsDrifted(context.Background(), nodeClaim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != cloudprovider.DriftLabels {
		t.Errorf("want DriftLabels, got %q", reason)
	}
}

// TestIsDrifted_Labels_Match verifies that when all NodeClass spec.labels are
// present and matching on the server, no drift is reported.
func TestIsDrifted_Labels_Match(t *testing.T) {
	nc := baselineNodeClass()
	nc.Spec.Labels = map[string]string{"env": "prod"}
	server := baselineServer()
	// server has the required label plus additional management labels (subset rule)
	server.Labels = map[string]string{
		"env":                     "prod",
		"karpenter.sh/managed-by": "karpenter",
	}
	cp, nodeClaim := buildCP(t, nc, server)
	reason, err := cp.IsDrifted(context.Background(), nodeClaim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Errorf("expected no drift when spec labels present+matching, got %q", reason)
	}
}

// TestIsDrifted_Labels_Empty verifies that when NodeClass spec.labels is empty
// (nil), the label check is skipped and no drift is reported.
func TestIsDrifted_Labels_Empty(t *testing.T) {
	nc := baselineNodeClass()
	// Spec.Labels is nil / not set — no requirement
	server := baselineServer()
	server.Labels = map[string]string{} // server has no labels either
	cp, nodeClaim := buildCP(t, nc, server)
	reason, err := cp.IsDrifted(context.Background(), nodeClaim)
	if err != nil {
		t.Fatal(err)
	}
	if reason != "" {
		t.Errorf("expected no drift for empty spec labels, got %q", reason)
	}
}

// ---------------------------------------------------------------------------
// Price-based selection
// ---------------------------------------------------------------------------

// armType builds an arm64 server type in nbg1 at the given net hourly price. Cheap ARM
// candidates recur in these tests because they are what cheapest-first reaches for when a
// NodePool leaves kubernetes.io/arch unconstrained.
func armType(name, hourly string) *hcloud.ServerType {
	return &hcloud.ServerType{
		Name: name, Cores: 8, Memory: 16, Disk: 160,
		Architecture: hcloud.ArchitectureARM, CPUType: hcloud.CPUTypeShared,
		Pricings: []hcloud.ServerTypeLocationPricing{{
			Location: &hcloud.Location{Name: "nbg1"},
			Hourly:   hcloud.Price{Net: hourly},
		}},
	}
}

// hourlyType builds a compatible x86 server type in nbg1 at the given net hourly price.
func hourlyType(name string, cores int, memGB float32, hourly string) *hcloud.ServerType {
	return &hcloud.ServerType{
		Name: name, Cores: cores, Memory: memGB, Disk: 160,
		Architecture: hcloud.ArchitectureX86, CPUType: hcloud.CPUTypeShared,
		Pricings: []hcloud.ServerTypeLocationPricing{{
			Location: &hcloud.Location{Name: "nbg1"},
			Hourly:   hcloud.Price{Net: hourly},
		}},
	}
}

// TestCreate_PicksCheapestCompatibleType verifies that Create launches the cheapest
// compatible type, not the first one the Hetzner API happened to return. Hetzner lists
// server types by ascending id, which puts the expensive CPX family (ids 108-113) ahead
// of CX (114-117), so a provider that takes the first match buys cpx42 (EUR 69.49) where
// cx43 (EUR 15.99) fits identically.
func TestCreate_PicksCheapestCompatibleType(t *testing.T) {
	// Cheapest deliberately in the MIDDLE: a two-element fixture cannot distinguish
	// "sorted by price" from "reversed the input".
	types := []*hcloud.ServerType{
		hourlyType("cpx42", 8, 16, "0.1130"),
		hourlyType("cx43", 8, 16, "0.0260"),
		hourlyType("cpx52", 12, 24, "0.1650"),
	}
	cp, fsc, _ := buildCPWithTypes(t, baselineNodeClass(), types)

	if _, err := cp.Create(context.Background(), createNodeClaim()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fsc.lastOpts.ServerType == nil {
		t.Fatal("no server type recorded on create")
	}
	if got := fsc.lastOpts.ServerType.Name; got != "cx43" {
		t.Errorf("expected cheapest compatible type cx43 (EUR 15.99/mo), got %q (cpx42 is EUR 69.49/mo)", got)
	}
}

// TestCreate_LaunchesInCheapestCompatibleLocation verifies that Create launches in the
// cheapest compatible location and stamps that same location on the NodeClaim's zone
// label. OrderByPrice ranks a type by its minimum-priced offering across locations, so
// selecting the type and then launching at Pricings[0] can bill several times that
// minimum. A zone label taken from a different offering would disagree with where the
// server actually is, which breaks karpenter core's node pricing and consolidation.
// Hetzner prices a given type identically across eu-central today, which keeps this
// latent there, but it is wrong wherever prices differ per location.
func TestCreate_LaunchesInCheapestCompatibleLocation(t *testing.T) {
	st := &hcloud.ServerType{
		Name: "cx43", Cores: 8, Memory: 16, Disk: 160,
		Architecture: hcloud.ArchitectureX86, CPUType: hcloud.CPUTypeShared,
		Pricings: []hcloud.ServerTypeLocationPricing{
			// Expensive location listed first, mirroring hcloud Pricings order.
			{Location: &hcloud.Location{Name: "nbg1"}, Hourly: hcloud.Price{Net: "0.3000"}},
			{Location: &hcloud.Location{Name: "hel1"}, Hourly: hcloud.Price{Net: "0.0500"}},
		},
	}
	nc := baselineNodeClass()
	nc.Spec.Locations = []string{"nbg1", "hel1"}
	cp, fsc, _ := buildCPWithTypes(t, nc, []*hcloud.ServerType{st})

	created, err := cp.Create(context.Background(), createNodeClaim())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fsc.lastOpts.Location == nil {
		t.Fatal("no location recorded on create")
	}
	if got := fsc.lastOpts.Location.Name; got != "hel1" {
		t.Errorf("expected launch in cheapest location hel1 (0.05/h), got %q (nbg1 is 0.30/h)", got)
	}
	if got := created.Labels[corev1.LabelTopologyZone]; got != fsc.lastOpts.Location.Name {
		t.Errorf("zone label %q disagrees with the launch location %q", got, fsc.lastOpts.Location.Name)
	}
}

// TestCreate_SkipsCheaperExcludedType verifies that a cheaper type the NodeClaim does
// not admit is passed over rather than launched, whichever requirement key excludes it.
// Price ordering promotes such a type to the front of selection, so this exercises the
// requirement filter cheapest-first newly leans on. The instance-type case is the one
// core actually sends: it narrows node.kubernetes.io/instance-type to the types
// compatible with the pending pods, which is what keeps architecture core's decision
// rather than the provider's.
func TestCreate_SkipsCheaperExcludedType(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  karpv1.NodeSelectorRequirementWithMinValues
	}{
		{"by arch", karpv1.NodeSelectorRequirementWithMinValues{
			Key: corev1.LabelArchStable, Operator: corev1.NodeSelectorOpIn,
			Values: []string{"amd64"},
		}},
		{"by core's instance-type list", karpv1.NodeSelectorRequirementWithMinValues{
			Key: corev1.LabelInstanceTypeStable, Operator: corev1.NodeSelectorOpIn,
			Values: []string{"cpx42", "cx43"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The expensive admitted type is listed FIRST, so the expected answer is
			// reachable only by price: an unordered first-match loop would buy cpx42.
			types := []*hcloud.ServerType{
				hourlyType("cpx42", 8, 16, "0.1130"),
				armType("cax31", "0.0100"), // cheapest, excluded by both requirements
				hourlyType("cx43", 8, 16, "0.0260"),
			}
			cp, fsc, _ := buildCPWithTypes(t, baselineNodeClass(), types)

			claim := createNodeClaim()
			claim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{tc.req}
			if _, err := cp.Create(context.Background(), claim); err != nil {
				t.Fatalf("Create: %v", err)
			}
			if fsc.lastOpts.ServerType == nil {
				t.Fatal("no server type recorded on create")
			}
			if got := fsc.lastOpts.ServerType.Name; got != "cx43" {
				t.Errorf("expected cheapest ADMITTED type cx43, got %q (cax31 is cheaper but excluded)", got)
			}
		})
	}
}

// TestCreate_HonoursZoneRequirementOverPrice verifies that a zone pinned by the
// NodeClaim wins over a cheaper offering elsewhere. Every other test here drives Create
// with an empty requirement set, which makes the Compatible(reqs) filter on the offering
// a no-op; this is the case that pins it. It matters for PVC-bound pods: commit c9c8096
// aliases csi.hetzner.cloud/location onto topology.kubernetes.io/zone so a volume's
// nodeAffinity reaches this filter, and launching in the cheapest other zone instead
// would strand the volume.
func TestCreate_HonoursZoneRequirementOverPrice(t *testing.T) {
	st := &hcloud.ServerType{
		Name: "cx43", Cores: 8, Memory: 16, Disk: 160,
		Architecture: hcloud.ArchitectureX86, CPUType: hcloud.CPUTypeShared,
		Pricings: []hcloud.ServerTypeLocationPricing{
			{Location: &hcloud.Location{Name: "nbg1"}, Hourly: hcloud.Price{Net: "0.3000"}},
			{Location: &hcloud.Location{Name: "hel1"}, Hourly: hcloud.Price{Net: "0.0500"}},
		},
	}
	nc := baselineNodeClass()
	nc.Spec.Locations = []string{"nbg1", "hel1"}
	cp, fsc, _ := buildCPWithTypes(t, nc, []*hcloud.ServerType{st})

	// Pin the EXPENSIVE zone: only the Compatible(reqs) filter can produce this answer.
	claim := createNodeClaim()
	claim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
		{
			Key:      corev1.LabelTopologyZone,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{"nbg1"},
		},
	}

	created, err := cp.Create(context.Background(), claim)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fsc.lastOpts.Location == nil {
		t.Fatal("no location recorded on create")
	}
	if got := fsc.lastOpts.Location.Name; got != "nbg1" {
		t.Errorf("expected the required zone nbg1, got %q (hel1 is cheaper but not allowed)", got)
	}
	if got := created.Labels[corev1.LabelTopologyZone]; got != "nbg1" {
		t.Errorf("zone label = %q, want nbg1", got)
	}
}

// TestCreate_SkipsArchWithoutResolvedImage verifies that Create walks past a cheaper
// instance type whose architecture the NodeClass has no image for, instead of
// dead-ending on it. The nodeclass controller deliberately marks a NodeClass Ready when
// only ONE architecture resolves ("an all-amd64 cluster has no arm64 Talos snapshot"),
// so an x86-only catalog is a supported configuration -- and cheapest-first selection
// steers into it whenever a pool permits both architectures and the ARM candidate
// prices lower for the requested shape. Create breaks
// out of the selection loop before resolving the image, so without this filter the miss
// is terminal: the type is never demoted, never marked unavailable, and karpenter core
// requeues the same doomed candidate forever while cx43 sits one iteration away.
func TestCreate_SkipsArchWithoutResolvedImage(t *testing.T) {
	cheapArm := armType("cax31", "0.0100")
	nc := baselineNodeClass()
	// Exactly what resolveImages records for an x86-only image catalog.
	withResolvedImages(nc, apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 42})
	types := []*hcloud.ServerType{cheapArm, hourlyType("cx43", 8, 16, "0.0260")}
	cp, fsc, _ := buildCPWithTypes(t, nc, types)

	// No arch requirement: the NodeClaim itself permits arm64, so only the
	// resolved-image filter can keep Create off cax31.
	if _, err := cp.Create(context.Background(), createNodeClaim()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fsc.lastOpts.ServerType == nil {
		t.Fatal("no server type recorded on create")
	}
	if got := fsc.lastOpts.ServerType.Name; got != "cx43" {
		t.Errorf("expected cx43, the cheapest type with a resolved image, got %q", got)
	}
}

// TestCreate_ReportsMissingImageNotCapacity verifies that a NodeClaim blocked purely by
// a missing image reports that, rather than InsufficientCapacityError. The two need
// different responses: capacity is transient and worth a backoff retry, a missing image
// is a configuration problem that no amount of retrying fixes. Reporting it as capacity
// sends operators to their Hetzner quotas for a problem that lives in the NodeClass.
// This is reachable whenever a NodeClass resolves only some architectures -- which the
// nodeclass controller explicitly supports -- and a NodePool pins one of the others.
func TestCreate_ReportsMissingImageNotCapacity(t *testing.T) {
	cheapArm := armType("cax31", "0.0100")
	nc := baselineNodeClass()
	withResolvedImages(nc, apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 42})
	cp, _, _ := buildCPWithTypes(t, nc, []*hcloud.ServerType{cheapArm, hourlyType("cx43", 8, 16, "0.0260")})

	// An arm64 NodePool on a NodeClass that only resolved an amd64 image.
	claim := createNodeClaim()
	claim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
		{
			Key:      corev1.LabelArchStable,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{"arm64"},
		},
	}

	_, err := cp.Create(context.Background(), claim)
	if err == nil {
		t.Fatal("expected an error when no architecture has a resolved image")
	}
	// Assert the TYPE, not just "not capacity": core switches on it in launch.go. A bare
	// error falls to the default branch, which parks the NodeClaim in Launched=Unknown and
	// requeues it forever; NodeClassNotReady deletes the claim so the scheduler can try a
	// different shape. Asserting only "not an ICE" also passes for the pre-fix dead-end
	// error from imageProvider.Resolve, which says "image" and "arm" too.
	if !karpcp.IsNodeClassNotReadyError(err) {
		t.Errorf("want NodeClassNotReadyError so core deletes the claim, got %T: %v", err, err)
	}
	if !strings.Contains(err.Error(), "has no resolved image for architecture") {
		t.Errorf("error should name the missing resolved image, got: %v", err)
	}
	if !strings.Contains(err.Error(), string(hcloud.ArchitectureARM)) {
		t.Errorf("error should name the architecture, got: %v", err)
	}
}

// TestCreate_PrefersCapacityErrorWhenBothBlock verifies that a candidate which cleared
// the image gate and failed only for want of an available offering still yields
// InsufficientCapacityError, even though a different candidate was image-blocked.
// Capacity carries core's retry and MarkUnavailable semantics, so an unrelated missing
// image must not mask a candidate that a retry could actually satisfy.
func TestCreate_PrefersCapacityErrorWhenBothBlock(t *testing.T) {
	cheapArm := armType("cax31", "0.0100")
	nc := baselineNodeClass()
	// arm64 has no image; amd64 does, so cx43 reaches the offering check.
	withResolvedImages(nc, apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 42})
	cp, _, _ := buildCPWithTypes(t, nc, []*hcloud.ServerType{cheapArm, hourlyType("cx43", 8, 16, "0.0260")})

	// Both types are offered only in nbg1, so pinning hel1 leaves cx43 with no
	// compatible offering: image-eligible, capacity-blocked.
	claim := createNodeClaim()
	claim.Spec.Requirements = []karpv1.NodeSelectorRequirementWithMinValues{
		{
			Key:      corev1.LabelTopologyZone,
			Operator: corev1.NodeSelectorOpIn,
			Values:   []string{"hel1"},
		},
	}

	_, err := cp.Create(context.Background(), claim)
	if err == nil {
		t.Fatal("expected an error when no offering is available")
	}
	if !karpcp.IsInsufficientCapacityError(err) {
		t.Errorf("expected InsufficientCapacityError, got %v", err)
	}
}

// TestCreate_TieBreaksDeterministically verifies that two types tied on cheapest price
// resolve to the same one every time. karpenter core's OrderByPrice sorts with the
// unstable sort.Slice and has no tiebreak, so tied types come out in an order that
// shifts when an unrelated type is added or an offering's availability flips -- two
// NodeClaims from one NodePool could land on different CPU shapes or architectures,
// failing a Deployment on some replicas and not others. Ties break on name here.
func TestCreate_TieBreaksDeterministically(t *testing.T) {
	tied := func(name string, arch hcloud.Architecture) *hcloud.ServerType {
		return &hcloud.ServerType{
			Name: name, Cores: 8, Memory: 16, Disk: 160,
			Architecture: arch, CPUType: hcloud.CPUTypeShared,
			Pricings: []hcloud.ServerTypeLocationPricing{{
				Location: &hcloud.Location{Name: "nbg1"},
				Hourly:   hcloud.Price{Net: "0.0260"},
			}},
		}
	}
	// Same two tied types, presented in different catalogue orders and with a differing
	// number of pricier decoys, which is what perturbs an unstable sort's pivot choice.
	orders := [][]*hcloud.ServerType{
		{tied("cpx41", hcloud.ArchitectureX86), tied("ccx13", hcloud.ArchitectureX86)},
		{tied("ccx13", hcloud.ArchitectureX86), tied("cpx41", hcloud.ArchitectureX86)},
		{hourlyType("cpx52", 12, 24, "0.9000"), tied("cpx41", hcloud.ArchitectureX86),
			hourlyType("cpx62", 16, 32, "0.9900"), tied("ccx13", hcloud.ArchitectureX86)},
	}
	for i, types := range orders {
		cp, fsc, _ := buildCPWithTypes(t, baselineNodeClass(), types)
		if _, err := cp.Create(context.Background(), createNodeClaim()); err != nil {
			t.Fatalf("order %d: Create: %v", i, err)
		}
		if got := fsc.lastOpts.ServerType.Name; got != "ccx13" {
			t.Errorf("order %d: tie resolved to %q, want the deterministic winner ccx13", i, got)
		}
	}
}

// TestGetInstanceTypes_MarksImagelessArchUnavailable verifies that architectures the
// NodeClass has no image for are advertised to karpenter core as unavailable, while
// staying in the catalogue.
//
// Availability and membership do different jobs here. The scheduler's fits() gates on
// an available compatible offering, so dropping availability stops core scheduling onto
// an architecture Create will refuse -- without it, core creates a NodeClaim, Create
// returns NodeClassNotReadyError, core deletes the claim, and the next cycle recreates
// the identical claim forever with no backoff. Drift, by contrast, checks
// Offerings.HasCompatible with no Available() filter, so keeping the offering listed
// means running nodes of that type are not drifted and replaced. This is the same
// mechanism already used for unpriced offerings.
func TestGetInstanceTypes_MarksImagelessArchUnavailable(t *testing.T) {
	nc := baselineNodeClass()
	withResolvedImages(nc, apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 42})
	types := []*hcloud.ServerType{armType("cax31", "0.0100"), hourlyType("cx43", 8, 16, "0.0260")}
	cp, _, _ := buildCPWithTypes(t, nc, types)

	nodePool := &karpv1.NodePool{ObjectMeta: metav1.ObjectMeta{Name: "default"}}
	nodePool.Spec.Template.Spec.NodeClassRef = &karpv1.NodeClassReference{
		Name: "default", Group: apiv1.Group, Kind: "HCloudNodeClass",
	}

	its, err := cp.GetInstanceTypes(context.Background(), nodePool)
	if err != nil {
		t.Fatalf("GetInstanceTypes: %v", err)
	}
	byName := map[string]*karpcp.InstanceType{}
	for _, it := range its {
		byName[it.Name] = it
	}

	arm, ok := byName["cax31"]
	if !ok {
		t.Fatal("cax31 was removed from the catalogue; running arm nodes would drift")
	}
	if len(arm.Offerings.Available()) != 0 {
		t.Error("cax31 has no resolved image but is still advertised as available; core will schedule onto it")
	}
	if len(arm.Offerings) == 0 {
		t.Error("cax31 offerings were dropped rather than marked unavailable")
	}

	amd, ok := byName["cx43"]
	if !ok {
		t.Fatal("cx43 missing from catalogue")
	}
	if len(amd.Offerings.Available()) == 0 {
		t.Error("cx43 has a resolved image and must stay available")
	}
}

// TestCreate_LaunchesStatusResolvedImageID verifies that the launch uses the image the
// gate admitted the architecture on, rather than re-deriving one live. Two sources can
// disagree: the gate consults status.resolvedImages while a second live lookup could
// return a different image or fail outright, which is how a candidate could pass
// selection and then die at image resolution. The status ID is deliberately different
// from what the image client would return, so only reading status produces it.
func TestCreate_LaunchesStatusResolvedImageID(t *testing.T) {
	nc := baselineNodeClass()
	withResolvedImages(nc, apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 99})
	cp, fsc, _ := buildCPWithTypes(t, nc, []*hcloud.ServerType{cx22Type()})

	if _, err := cp.Create(context.Background(), createNodeClaim()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fsc.lastOpts.Image == nil {
		t.Fatal("no image recorded on create")
	}
	if got := fsc.lastOpts.Image.ID; got != 99 {
		t.Errorf("launched image %d, want 99 from status.resolvedImages (42 means a second live lookup won)", got)
	}
}

// TestCreate_LiveImageNotFoundIsTerminal verifies that when status carries no entry for
// the architecture -- the un-reconciled NodeClass case, where the gate deliberately
// fails open -- a definitive miss from the live lookup is still classified. A bare error
// lands in launch.go's default branch, which parks the NodeClaim at Launched=Unknown and
// requeues it forever; deterministic selection then re-picks the same doomed type every
// cycle. NodeClassNotReadyError makes core delete the claim instead.
func TestCreate_LiveImageNotFoundIsTerminal(t *testing.T) {
	// Empty catalogue: the live lookup definitively finds nothing.
	cp, _, _ := buildCPWithImages(t, baselineNodeClass(), []*hcloud.ServerType{cx22Type()}, nil)

	_, err := cp.Create(context.Background(), createNodeClaim())
	if err == nil {
		t.Fatal("expected an error when no image can be resolved")
	}
	if !karpcp.IsNodeClassNotReadyError(err) {
		t.Errorf("want NodeClassNotReadyError so core deletes the claim, got %T: %v", err, err)
	}
}

// unfilteredImages ignores the architecture filter and always answers with an x86 image,
// standing in for a catalogue that returns something the server-side filter should have
// excluded. fakeImageClient deliberately honours the filter, so this is the only way to
// reach the mismatch.
type unfilteredImages struct{ img *hcloud.Image }

func (u unfilteredImages) AllWithOpts(_ context.Context, _ hcloud.ImageListOpts) ([]*hcloud.Image, error) {
	return []*hcloud.Image{u.img}, nil
}

// TestCreate_LiveImageWrongArchIsRejected verifies that the live image lookup -- the
// path taken when status carries no entry for the architecture -- refuses an image whose
// architecture does not match the one the NodeClaim requires, instead of launching it.
// Such a node boots and then fails every workload with "exec format error". The check
// has to live on this path: an entry read from status reports the architecture it was
// filed under, so comparing it against that same architecture in Create can never fail
// (the nodeclass controller verifies status entries at write time instead).
func TestCreate_LiveImageWrongArchIsRejected(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := baselineNodeClass() // no status.resolvedImages: forces the live lookup
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(nc).Build()
	fsc := &fakeServerClient{servers: map[int64]*hcloud.Server{}}
	cp := cloudprovider.NewCloudProvider(kube,
		instance.NewProvider(fsc, "test-cluster"),
		instancetype.NewProvider(&fakeServerTypeClient{types: []*hcloud.ServerType{armType("cax31", "0.0100")}}),
		imagefamily.NewProvider(unfilteredImages{
			img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04", Architecture: hcloud.ArchitectureX86},
		}))

	_, err := cp.Create(context.Background(), createNodeClaim())
	if err == nil {
		t.Fatal("launched an arm64 node from an x86 image")
	}
	if fsc.lastOpts.ServerType != nil {
		t.Errorf("a server was created despite the architecture mismatch: %+v", fsc.lastOpts.ServerType)
	}
	if !karpcp.IsNodeClassNotReadyError(err) {
		t.Errorf("want NodeClassNotReadyError so core deletes the claim, got %T: %v", err, err)
	}
}

// selectionSkippedCount reads the current value of
// karpenter_hetzner_instance_type_selection_skipped_total for one arch/reason pair.
// Tests compare deltas, since the registry is process-global and other tests launch too.
func selectionSkippedCount(t *testing.T, arch, reason string) float64 {
	t.Helper()
	families, err := crmetrics.Registry.Gather()
	if err != nil {
		t.Fatalf("gathering metrics: %v", err)
	}
	for _, f := range families {
		if f.GetName() != "karpenter_hetzner_instance_type_selection_skipped_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			labels := map[string]string{}
			for _, l := range m.GetLabel() {
				labels[l.GetName()] = l.GetValue()
			}
			if labels["arch"] == arch && labels["reason"] == reason {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

// TestCreate_CountsSkippedArchOncePerLaunch verifies that the skipped-selection counter
// tracks launches, not catalogue size. Incrementing inside the selection loop made a
// single launch add one per ARM type hcloud publishes, so the rate scaled with the
// catalogue and could not be compared across clusters or read as "how often are we
// routing around a missing image" -- which is what docs/talos-bootstrap.md tells
// operators to alert on.
func TestCreate_CountsSkippedArchOncePerLaunch(t *testing.T) {
	nc := baselineNodeClass()
	withResolvedImages(nc, apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 42})
	// Three ARM types are skipped for the one missing image; the counter must move by 1.
	types := []*hcloud.ServerType{
		armType("cax11", "0.0100"), armType("cax21", "0.0110"), armType("cax31", "0.0120"),
		hourlyType("cx43", 8, 16, "0.0260"),
	}
	cp, fsc, _ := buildCPWithTypes(t, nc, types)

	before := selectionSkippedCount(t, string(hcloud.ArchitectureARM), "no_resolved_image")
	if _, err := cp.Create(context.Background(), createNodeClaim()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got := fsc.lastOpts.ServerType.Name; got != "cx43" {
		t.Fatalf("expected the launch to fall through to cx43, got %q", got)
	}
	if delta := selectionSkippedCount(t, string(hcloud.ArchitectureARM), "no_resolved_image") - before; delta != 1 {
		t.Errorf("counter moved by %v for one launch that skipped 3 arm types, want 1", delta)
	}
}

// TestCreate_IgnoresStaleGenerationResolvedImage verifies that image IDs recorded under
// an earlier spec generation are not launched. The nodeclass controller clears them the
// next time it reconciles, but nothing makes core wait for that: its NodeClass readiness
// gate compares no observedGeneration, so a NodeClass whose imageSelector was just
// repinned still reads Ready=True -- and stays that way indefinitely if the nodeclass
// controller is wedged. Reading status unconditionally would boot the pre-edit image
// while every condition is green; a stale entry must fall back to a live lookup against
// the current selector, which is what Create did before status was consulted at all.
func TestCreate_IgnoresStaleGenerationResolvedImage(t *testing.T) {
	// Resolved under generation 1, exactly as a successful pass would stamp it...
	nc := withResolvedImages(baselineNodeClass(),
		apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 99})
	// ...then the operator edits the spec, so image 99 answers a question no longer asked.
	nc.Generation = 2

	cp, fsc, _ := buildCPWithTypes(t, nc, []*hcloud.ServerType{cx22Type()})

	if _, err := cp.Create(context.Background(), createNodeClaim()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if fsc.lastOpts.Image == nil {
		t.Fatal("no image recorded on create")
	}
	if got := fsc.lastOpts.Image.ID; got != 42 {
		t.Errorf("launched image %d, want 42 from a live lookup (99 means the pre-edit status entry won)", got)
	}
}
