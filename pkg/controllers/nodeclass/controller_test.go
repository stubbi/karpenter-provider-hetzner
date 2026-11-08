package nodeclass

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/deadline"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/imagefamily"
)

type fakeNetworks struct{ net *hcloud.Network }

func (f fakeNetworks) GetByID(_ context.Context, _ int64) (*hcloud.Network, *hcloud.Response, error) {
	return f.net, nil, nil
}

type fakeLocations struct {
	locations []*hcloud.Location
	err       error
}

func (f fakeLocations) All(_ context.Context) ([]*hcloud.Location, error) {
	return f.locations, f.err
}

func validLocations() fakeLocations {
	return fakeLocations{locations: []*hcloud.Location{{Name: "nbg1", NetworkZone: hcloud.NetworkZoneEUCentral}}}
}

func validNetwork() *hcloud.Network {
	return &hcloud.Network{
		ID:      1,
		Subnets: []hcloud.NetworkSubnet{{NetworkZone: hcloud.NetworkZoneEUCentral}},
	}
}

type fakeFirewalls struct{ fw *hcloud.Firewall }

func (f fakeFirewalls) GetByID(_ context.Context, _ int64) (*hcloud.Firewall, *hcloud.Response, error) {
	return f.fw, nil, nil
}

type fakeSSHKeys struct{ key *hcloud.SSHKey }

func (f fakeSSHKeys) GetByID(_ context.Context, _ int64) (*hcloud.SSHKey, *hcloud.Response, error) {
	return f.key, nil, nil
}

// fakeImages answers every architecture, echoing the requested one back on the image
// exactly as hcloud does (it filters server-side and always populates Architecture).
type fakeImages struct{ img *hcloud.Image }

func (f fakeImages) AllWithOpts(_ context.Context, opts hcloud.ImageListOpts) ([]*hcloud.Image, error) {
	img := *f.img
	if len(opts.Architecture) > 0 {
		img.Architecture = opts.Architecture[0]
	}
	return []*hcloud.Image{&img}, nil
}

type emptyImages struct{}

func (emptyImages) AllWithOpts(_ context.Context, _ hcloud.ImageListOpts) ([]*hcloud.Image, error) {
	return nil, nil
}

// amd64OnlyImages returns an image for amd64/x86 requests and nothing for arm64,
// mimicking a cluster that only has an x86 OS image (the common case).
type amd64OnlyImages struct{ img *hcloud.Image }

func (f amd64OnlyImages) AllWithOpts(_ context.Context, opts hcloud.ImageListOpts) ([]*hcloud.Image, error) {
	for _, a := range opts.Architecture {
		if a == hcloud.ArchitectureX86 {
			img := *f.img
			img.Architecture = hcloud.ArchitectureX86
			return []*hcloud.Image{&img}, nil
		}
	}
	return nil, nil
}

func newNodeClass() *apiv1.HCloudNodeClass {
	return &apiv1.HCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec: apiv1.HCloudNodeClassSpec{
			Locations:     []string{"nbg1"},
			NetworkID:     1,
			ImageSelector: apiv1.ImageSelector{Family: "ubuntu"},
		},
	}
}

func TestReconcile_SetsReadyWhenValid(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(fakeImages{img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04"}})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)

	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	if !got.StatusConditions().Get(apiv1.ConditionTypeNetworkReady).IsTrue() {
		t.Error("NetworkReady should be true")
	}
	if len(got.Status.ResolvedImages) == 0 {
		t.Error("expected resolved images")
	}
	for _, ri := range got.Status.ResolvedImages {
		if ri.Architecture == "" {
			t.Error("resolved image missing architecture")
		}
	}
	if !got.StatusConditions().Root().IsTrue() {
		t.Error("Ready should be true when all dependents are true")
	}
}

func TestReconcile_SingleArchImageIsReady(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	// Cluster only has an amd64 image (no arm64) — the NodeClass must still be Ready.
	img := imagefamily.NewProvider(amd64OnlyImages{img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04"}})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)

	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	if !got.StatusConditions().Get(apiv1.ConditionTypeImagesReady).IsTrue() {
		t.Error("ImagesReady should be true when at least one arch resolves")
	}
	if !got.StatusConditions().Root().IsTrue() {
		t.Error("Ready should be true with a single-arch image")
	}
	if len(got.Status.ResolvedImages) != 1 || got.Status.ResolvedImages[0].Architecture != "x86" {
		t.Errorf("expected exactly one x86 resolved image, got %+v", got.Status.ResolvedImages)
	}
}

func TestReconcile_ImageResolutionFails(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(emptyImages{})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)

	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	if got.StatusConditions().Get(apiv1.ConditionTypeImagesReady).IsTrue() {
		t.Error("ImagesReady should be false when image resolution fails")
	}
	if got.StatusConditions().Root().IsTrue() {
		t.Error("Ready should not be true when images fail to resolve")
	}
	// Status must not keep advertising images that no longer resolve: instance-type
	// selection reads ResolvedImages to decide which architectures are launchable, so a
	// stale entry routes it at an architecture that cannot boot.
	if len(got.Status.ResolvedImages) != 0 {
		t.Errorf("expected ResolvedImages cleared when no architecture resolves, got %+v", got.Status.ResolvedImages)
	}
}

// TestReconcile_ClearsStaleResolvedImages verifies that images resolved on an earlier
// pass are removed once resolution stops succeeding, rather than lingering in status.
func TestReconcile_ClearsStaleResolvedImages(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	nc.Status.ResolvedImages = []apiv1.ResolvedImage{
		{Architecture: "x86", ImageID: 42},
		{Architecture: "arm", ImageID: 43},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(emptyImages{})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)

	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.ResolvedImages) != 0 {
		t.Errorf("stale resolved images survived a failed resolution: %+v", got.Status.ResolvedImages)
	}
}

func TestReconcile_NetworkNotFound(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(fakeImages{img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04"}})
	c := NewController(kube, fakeNetworks{net: nil}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img) // network missing

	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	if got.StatusConditions().Get(apiv1.ConditionTypeNetworkReady).IsTrue() {
		t.Error("NetworkReady should be false when network is missing")
	}
	if got.StatusConditions().Root().IsTrue() {
		t.Error("Ready should not be true when network is missing")
	}
}

func TestReconcile_LocationNotFound(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	nc.Spec.Locations = []string{"nbg1", "nonsense"}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(fakeImages{img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04"}})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)

	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	cond := got.StatusConditions().Get(apiv1.ConditionTypeLocationsReady)
	if !cond.IsFalse() || cond.Reason != "LocationNotFound" || !strings.Contains(cond.Message, "nonsense") {
		t.Errorf("unexpected LocationsReady condition: %+v", cond)
	}
	if got.StatusConditions().Root().IsTrue() {
		t.Error("Ready should not be true when a location does not exist")
	}
}

func TestReconcile_NetworkZoneNotCovered(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	nc.Spec.Locations = []string{"nbg1", "ash-dc1"}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	locations := fakeLocations{locations: []*hcloud.Location{
		{Name: "nbg1", NetworkZone: hcloud.NetworkZoneEUCentral},
		{Name: "ash-dc1", NetworkZone: hcloud.NetworkZoneUSEast},
	}}
	img := imagefamily.NewProvider(fakeImages{img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04"}})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, locations, fakeFirewalls{}, fakeSSHKeys{}, img)

	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	cond := got.StatusConditions().Get(apiv1.ConditionTypeLocationsReady)
	if !cond.IsFalse() || cond.Reason != "NetworkZoneNotCovered" || !strings.Contains(cond.Message, "ash-dc1 (us-east)") {
		t.Errorf("unexpected LocationsReady condition: %+v", cond)
	}
	if got.StatusConditions().Root().IsTrue() {
		t.Error("Ready should not be true when a location's network zone has no subnet")
	}
}

func TestReconcile_LocationAPIFailureIsUnknown(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(fakeImages{img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04"}})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, fakeLocations{err: errors.New("rate limited (429)")}, fakeFirewalls{}, fakeSSHKeys{}, img)

	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	cond := got.StatusConditions().Get(apiv1.ConditionTypeLocationsReady)
	if cond.IsTrue() || cond.IsFalse() || cond.Reason != "LocationCheckFailed" {
		t.Errorf("location API failure should be Unknown, got %+v", cond)
	}
}

func TestReconcile_FirewallNotFound(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	nc.Spec.FirewallIDs = []int64{7}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()
	img := imagefamily.NewProvider(fakeImages{img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04"}})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{fw: nil}, fakeSSHKeys{}, img) // firewall missing

	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	if got.StatusConditions().Get(apiv1.ConditionTypeResourcesReady).IsTrue() {
		t.Error("ResourcesReady should be false when a firewall is missing")
	}
	if got.StatusConditions().Root().IsTrue() {
		t.Error("Ready should not be true when a referenced firewall is missing")
	}
}

func TestReconcile_UserDataSecretValid(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	nc.Spec.UserDataSecretRef = &apiv1.UserDataSecretReference{
		Namespace: "kube-system",
		Name:      "talos",
		Key:       "userData",
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "talos", Namespace: "kube-system"},
		Data:       map[string][]byte{"userData": []byte("machine: {}\n")},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc, secret).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(fakeImages{img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04"}})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)

	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	if !got.StatusConditions().Get(apiv1.ConditionTypeUserDataReady).IsTrue() {
		t.Error("UserDataReady should be true for a valid secret ref")
	}
	if !got.StatusConditions().Root().IsTrue() {
		t.Error("Ready should be true when all conditions including UserDataReady are satisfied")
	}
}

// TestReconcile_UserDataKeyMissing verifies that Reconcile sets UserDataReady=False
// (reason UserDataKeyMissing) when the Secret exists but does not contain the
// referenced key. This is the "secret present, key absent" branch that differs from
// the secret-missing case.
func TestReconcile_UserDataKeyMissing(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	nc.Spec.UserDataSecretRef = &apiv1.UserDataSecretReference{
		Namespace: "kube-system",
		Name:      "talos",
		Key:       "userData",
	}
	// Secret exists but has a different key — "userData" is absent.
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "talos", Namespace: "kube-system"},
		Data:       map[string][]byte{"wrong": []byte("x")},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc, secret).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(fakeImages{img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04"}})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)

	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	if !got.StatusConditions().Get(apiv1.ConditionTypeUserDataReady).IsFalse() {
		t.Error("UserDataReady should be false when secret exists but referenced key is absent")
	}
	if got.StatusConditions().Root().IsTrue() {
		t.Error("Ready should not be true when UserDataReady is false")
	}
}

func TestReconcile_UserDataSecretMissing(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	nc.Spec.UserDataSecretRef = &apiv1.UserDataSecretReference{
		Namespace: "kube-system",
		Name:      "does-not-exist",
		Key:       "userData",
	}
	// Secret is NOT added to the fake client.
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(fakeImages{img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04"}})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)

	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	if !got.StatusConditions().Get(apiv1.ConditionTypeUserDataReady).IsFalse() {
		t.Error("UserDataReady should be false when the secret is missing")
	}
	if got.StatusConditions().Root().IsTrue() {
		t.Error("Ready should not be true when UserDataReady is false")
	}
}

// unreadableImages fails every list call, standing in for a 429/5xx from hcloud.
type unreadableImages struct{}

func (unreadableImages) AllWithOpts(_ context.Context, _ hcloud.ImageListOpts) ([]*hcloud.Image, error) {
	return nil, errors.New("rate limited (429)")
}

// x86OKArmErrors resolves x86 and fails the arm lookup with an API error, the shape of
// a partial outage: one architecture is genuinely known, the other is merely unreadable.
type x86OKArmErrors struct{ img *hcloud.Image }

func (f x86OKArmErrors) AllWithOpts(_ context.Context, opts hcloud.ImageListOpts) ([]*hcloud.Image, error) {
	for _, a := range opts.Architecture {
		if a == hcloud.ArchitectureX86 {
			img := *f.img
			img.Architecture = hcloud.ArchitectureX86
			return []*hcloud.Image{&img}, nil
		}
	}
	return nil, errors.New("server error (503)")
}

// TestReconcile_KeepsResolvedImagesOnTransientError verifies that an unreadable image
// catalogue does not clear status or declare the images gone. Instance-type selection
// reads ResolvedImages to decide which architectures are launchable, so clearing on an
// API blip makes Create reject every candidate and Karpenter delete NodeClaims over a
// transient error. Every other condition in Reconcile already sets Unknown and keeps
// state on API errors; images now match.
func TestReconcile_KeepsResolvedImagesOnTransientError(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	nc.Status.ResolvedImages = []apiv1.ResolvedImage{{Architecture: "x86", ImageID: 42}}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(unreadableImages{})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)
	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	if len(got.Status.ResolvedImages) != 1 || got.Status.ResolvedImages[0].ImageID != 42 {
		t.Errorf("last-known-good images discarded on a transient error: %+v", got.Status.ResolvedImages)
	}
	if cond := got.StatusConditions().Get(apiv1.ConditionTypeImagesReady); cond.IsFalse() {
		t.Errorf("transient error reported as definitive (False); want Unknown: %+v", cond)
	}
}

// TestReconcile_CarriesForwardArchOnTransientError verifies that when one architecture
// resolves and the other's lookup fails with an API error, the failing architecture's
// previous entry is carried forward rather than dropped. Dropping it reads as "this
// architecture has no image", which makes selection refuse it and Karpenter delete the
// NodeClaims of an arch-pinned NodePool over an unrelated blip.
func TestReconcile_CarriesForwardArchOnTransientError(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	nc.Status.ResolvedImages = []apiv1.ResolvedImage{
		{Architecture: "x86", ImageID: 42},
		{Architecture: "arm", ImageID: 43},
	}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(x86OKArmErrors{img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04"}})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)
	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	var haveArm bool
	for _, ri := range got.Status.ResolvedImages {
		if ri.Architecture == "arm" && ri.ImageID == 43 {
			haveArm = true
		}
	}
	if !haveArm {
		t.Errorf("arm entry dropped on an API error instead of carried forward: %+v", got.Status.ResolvedImages)
	}
}

// wrongArchImages returns an x86 image regardless of the architecture requested,
// simulating the catalogue answering with something the server-side filter should have
// excluded. The provider does not re-filter client-side, so nothing else catches it.
type wrongArchImages struct{}

func (wrongArchImages) AllWithOpts(_ context.Context, _ hcloud.ImageListOpts) ([]*hcloud.Image, error) {
	return []*hcloud.Image{{ID: 77, Description: "Ubuntu 24.04", Architecture: hcloud.ArchitectureX86}}, nil
}

// TestReconcile_RejectsWrongArchImage verifies that an image whose architecture does not
// match the one requested is never recorded in status. Create launches the ID from
// status and synthesises the architecture from the request, so an unverified entry here
// is launched unchecked and boots a node that fails workloads with exec format error.
func TestReconcile_RejectsWrongArchImage(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(wrongArchImages{})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)
	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	for _, ri := range got.Status.ResolvedImages {
		if ri.Architecture == string(hcloud.ArchitectureARM) {
			t.Errorf("recorded an x86 image under arm: %+v", got.Status.ResolvedImages)
		}
	}
}

// TestReconcile_DoesNotCarryForwardAcrossSpecChange verifies that image IDs resolved
// under an older spec are not carried forward after the spec changes. Carry-forward
// exists to survive an API blip, and is only sound while the question is unchanged: if
// the operator repins imageSelector and the new lookup fails with anything that is not a
// definitive miss, reusing the pre-edit IDs launches the OLD image while reporting the
// NodeClass green -- and a permanent failure (a 403 from a rotated token, a selector the
// API rejects) makes that state permanent rather than blip-bounded.
func TestReconcile_DoesNotCarryForwardAcrossSpecChange(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	// Resolve under generation 1, exactly as a successful pass would: SetTrue stamps the
	// ImagesReady condition's observedGeneration, which is where the generation of the
	// recorded IDs is read back from.
	nc.Generation = 1
	nc.StatusConditions().SetTrue(apiv1.ConditionTypeImagesReady)
	nc.Status.ResolvedImages = []apiv1.ResolvedImage{{Architecture: "x86", ImageID: 42}}
	nc.Generation = 2 // ...then the operator edits the spec, so those IDs answer the old one
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(unreadableImages{})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)
	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	for _, ri := range got.Status.ResolvedImages {
		if ri.ImageID == 42 {
			t.Errorf("carried a pre-edit image ID forward across a spec change: %+v", got.Status.ResolvedImages)
		}
	}
	if got.StatusConditions().Get(apiv1.ConditionTypeImagesReady).IsTrue() {
		t.Error("ImagesReady is green while no image was resolved for the current spec")
	}
}

// definitiveX86TransientArm reports x86 as definitively absent (an empty but readable
// catalogue) while the arm lookup fails with an API error -- the shape of an image
// deleted from the project during an unrelated hcloud blip.
type definitiveX86TransientArm struct{}

func (definitiveX86TransientArm) AllWithOpts(_ context.Context, opts hcloud.ImageListOpts) ([]*hcloud.Image, error) {
	for _, a := range opts.Architecture {
		if a == hcloud.ArchitectureX86 {
			return nil, nil // readable, and holds nothing: definitive
		}
	}
	return nil, errors.New("server error (503)")
}

// TestReconcile_ReportsDefinitiveErrorAlongsideTransient verifies that an architecture
// proven absent is both reported and dropped from status, even when a different
// architecture failed transiently in the same pass. The transient branch keeps state on
// purpose, but anything left in status at that point can only belong to a DEFINITIVELY
// failed architecture -- a transient one is carried forward into the resolved set -- so
// leaving it there advertises an image the catalogue says is gone, and reporting only
// the 503 hides the one error the operator can actually act on.
func TestReconcile_ReportsDefinitiveErrorAlongsideTransient(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	nc.Status.ResolvedImages = []apiv1.ResolvedImage{{Architecture: "x86", ImageID: 42}}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()

	img := imagefamily.NewProvider(definitiveX86TransientArm{})
	c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)
	if _, err := c.Reconcile(context.Background(), nc.DeepCopy()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	for _, ri := range got.Status.ResolvedImages {
		if ri.Architecture == "x86" {
			t.Errorf("kept an x86 entry the catalogue definitively lacks: %+v", got.Status.ResolvedImages)
		}
	}
	cond := got.StatusConditions().Get(apiv1.ConditionTypeImagesReady)
	if cond == nil {
		t.Fatal("ImagesReady condition not set")
	}
	if !strings.Contains(cond.Message, "no ubuntu image found") {
		t.Errorf("definitive x86 failure not reported; message only carries the blip: %q", cond.Message)
	}
	if !strings.Contains(cond.Message, "503") {
		t.Errorf("transient arm failure dropped from the message: %q", cond.Message)
	}
}

// TestResolvedImagesGeneration_AbsentConditionFailsSafe pins the read path for the
// carry-forward gate. operatorpkg's ConditionSet constructor initializes any absent
// dependent condition to Unknown stamped with the CURRENT generation, so reading the
// generation through nc.StatusConditions() would report a NodeClass that never resolved
// an image as up to date -- disabling the gate -- and would mutate the object from what
// reads like a getter. Absent must read as zero so the caller clears rather than carries
// forward.
func TestResolvedImagesGeneration_AbsentConditionFailsSafe(t *testing.T) {
	nc := newNodeClass()
	nc.Generation = 7
	nc.Status.Conditions = nil

	if got := resolvedImagesGeneration(nc); got != 0 {
		t.Errorf("absent ImagesReady read as generation %d, want 0 (gate must clear, not carry forward)", got)
	}
	if len(nc.Status.Conditions) != 0 {
		t.Errorf("reading the generation fabricated %d conditions; it must not mutate the object",
			len(nc.Status.Conditions))
	}

	// A condition that IS present reports the generation it was stamped at.
	nc.Generation = 1
	nc.StatusConditions().SetTrue(apiv1.ConditionTypeImagesReady)
	nc.Generation = 2
	if got := resolvedImagesGeneration(nc); got != 1 {
		t.Errorf("resolvedImagesGeneration = %d, want 1 (the generation the images were resolved under)", got)
	}
}

// hungNetworks blocks until the reconcile's context is done, like a request that
// never completes, and records whether a deadline was set.
type hungNetworks struct{ sawDeadline *bool }

func (h hungNetworks) GetByID(ctx context.Context, _ int64) (*hcloud.Network, *hcloud.Response, error) {
	_, *h.sawDeadline = ctx.Deadline()
	<-ctx.Done()
	return nil, nil, ctx.Err()
}

func TestReconcile_HungAPICallEndsOnTheReconcileBudget(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := newNodeClass()
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(nc).WithStatusSubresource(nc).Build()
	img := imagefamily.NewProvider(fakeImages{img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04"}})
	var sawDeadline bool
	c := NewController(kube, hungNetworks{sawDeadline: &sawDeadline}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img)
	if c.timeout != deadline.Read {
		t.Fatalf("default reconcile budget = %s, want deadline.Read (%s)", c.timeout, deadline.Read)
	}
	c.timeout = 50 * time.Millisecond

	done := make(chan error, 1)
	go func() {
		_, err := c.Reconcile(context.Background(), nc.DeepCopy())
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("reconcile never returned: the hung network call has no deadline")
	}
	if !sawDeadline {
		t.Error("network call ran without a deadline")
	}
	// The timed-out call must still be reported, so the status write cannot share
	// the exhausted budget.
	got := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), client.ObjectKeyFromObject(nc), got); err != nil {
		t.Fatal(err)
	}
	if cond := got.StatusConditions().Get(apiv1.ConditionTypeNetworkReady); cond == nil || !cond.IsUnknown() {
		t.Errorf("NetworkReady = %v, want Unknown after the call timed out", cond)
	}
}
