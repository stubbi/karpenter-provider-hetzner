package imagefamily

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
)

// mockImageClient is a fake ImageClient for testing.
type mockImageClient struct {
	images   []*hcloud.Image
	lastOpts hcloud.ImageListOpts
}

func (m *mockImageClient) AllWithOpts(_ context.Context, opts hcloud.ImageListOpts) ([]*hcloud.Image, error) {
	m.lastOpts = opts
	var result []*hcloud.Image
	for _, img := range m.images {
		// Filter by type if specified.
		if len(opts.Type) > 0 {
			match := false
			for _, t := range opts.Type {
				if img.Type == t {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		// Filter by architecture if specified.
		if len(opts.Architecture) > 0 {
			match := false
			for _, a := range opts.Architecture {
				if img.Architecture == a {
					match = true
					break
				}
			}
			if !match {
				continue
			}
		}
		result = append(result, img)
	}
	return result, nil
}

func makeImage(id int64, imgType hcloud.ImageType, arch hcloud.Architecture, description string, created time.Time) *hcloud.Image {
	return &hcloud.Image{
		ID:           id,
		Type:         imgType,
		Architecture: arch,
		Description:  description,
		Created:      created,
	}
}

var baseTime = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)

func TestResolveUbuntu_PicksFirst(t *testing.T) {
	images := []*hcloud.Image{
		makeImage(1, hcloud.ImageTypeSystem, hcloud.ArchitectureX86, "Ubuntu 22.04", baseTime),
		makeImage(2, hcloud.ImageTypeSystem, hcloud.ArchitectureX86, "Ubuntu 20.04", baseTime),
	}
	p := NewProvider(&mockImageClient{images: images})

	img, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: "ubuntu"}, hcloud.ArchitectureX86)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img.ID != 1 {
		t.Errorf("expected first match (ID=1), got ID=%d", img.ID)
	}
}

func TestResolveUbuntu_VersionPin(t *testing.T) {
	images := []*hcloud.Image{
		makeImage(1, hcloud.ImageTypeSystem, hcloud.ArchitectureX86, "Ubuntu 22.04", baseTime),
		makeImage(2, hcloud.ImageTypeSystem, hcloud.ArchitectureX86, "Ubuntu 20.04", baseTime),
	}
	p := NewProvider(&mockImageClient{images: images})

	img, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: "ubuntu", Version: "20.04"}, hcloud.ArchitectureX86)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img.ID != 2 {
		t.Errorf("expected ID=2 (Ubuntu 20.04), got ID=%d", img.ID)
	}
}

func TestResolveUbuntu_ArchFilter(t *testing.T) {
	images := []*hcloud.Image{
		makeImage(1, hcloud.ImageTypeSystem, hcloud.ArchitectureARM, "Ubuntu 22.04 ARM", baseTime),
		makeImage(2, hcloud.ImageTypeSystem, hcloud.ArchitectureX86, "Ubuntu 22.04", baseTime),
	}
	p := NewProvider(&mockImageClient{images: images})

	img, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: "ubuntu"}, hcloud.ArchitectureARM)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img.ID != 1 {
		t.Errorf("expected ARM image (ID=1), got ID=%d", img.ID)
	}
}

func TestResolveUbuntu_NoMatch(t *testing.T) {
	images := []*hcloud.Image{
		makeImage(1, hcloud.ImageTypeSystem, hcloud.ArchitectureX86, "Ubuntu 22.04", baseTime),
	}
	p := NewProvider(&mockImageClient{images: images})

	_, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: "ubuntu", Version: "24.04"}, hcloud.ArchitectureX86)
	if err == nil {
		t.Fatal("expected error for no matching image, got nil")
	}
}

func TestResolveTalos_PicksNewest(t *testing.T) {
	images := []*hcloud.Image{
		makeImage(1, hcloud.ImageTypeSnapshot, hcloud.ArchitectureX86, "talos v1.6.0", baseTime),
		makeImage(2, hcloud.ImageTypeSnapshot, hcloud.ArchitectureX86, "talos v1.7.0", baseTime.Add(24*time.Hour)),
		makeImage(3, hcloud.ImageTypeSnapshot, hcloud.ArchitectureX86, "talos v1.5.0", baseTime.Add(-24*time.Hour)),
	}
	p := NewProvider(&mockImageClient{images: images})

	img, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: "talos"}, hcloud.ArchitectureX86)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img.ID != 2 {
		t.Errorf("expected newest talos (ID=2), got ID=%d", img.ID)
	}
}

func TestResolveTalos_VersionPin(t *testing.T) {
	images := []*hcloud.Image{
		makeImage(1, hcloud.ImageTypeSnapshot, hcloud.ArchitectureX86, "talos v1.6.0", baseTime),
		makeImage(2, hcloud.ImageTypeSnapshot, hcloud.ArchitectureX86, "talos v1.7.0", baseTime.Add(24*time.Hour)),
	}
	p := NewProvider(&mockImageClient{images: images})

	img, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: "talos", Version: "v1.6.0"}, hcloud.ArchitectureX86)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img.ID != 1 {
		t.Errorf("expected talos v1.6.0 (ID=1), got ID=%d", img.ID)
	}
}

func TestResolveTalos_IgnoresSystemImages(t *testing.T) {
	// talos images must be snapshots; system images called "talos" should be ignored.
	images := []*hcloud.Image{
		makeImage(1, hcloud.ImageTypeSystem, hcloud.ArchitectureX86, "talos system", baseTime),
		makeImage(2, hcloud.ImageTypeSnapshot, hcloud.ArchitectureX86, "talos v1.7.0", baseTime),
	}
	p := NewProvider(&mockImageClient{images: images})

	img, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: "talos"}, hcloud.ArchitectureX86)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Only the snapshot should be found.
	if img.ID != 2 {
		t.Errorf("expected snapshot talos (ID=2), got ID=%d", img.ID)
	}
}

func TestResolveUnsupportedFamily(t *testing.T) {
	p := NewProvider(&mockImageClient{})

	_, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: "fedora"}, hcloud.ArchitectureX86)
	if err == nil {
		t.Fatal("expected error for unsupported family, got nil")
	}
}

func TestResolveTalos_LabelSelectorForwarded(t *testing.T) {
	fc := &mockImageClient{images: []*hcloud.Image{
		makeImage(1, hcloud.ImageTypeSnapshot, hcloud.ArchitectureX86, "talos v1.13.3", baseTime),
	}}
	p := NewProvider(fc)
	sel := apiv1.ImageSelector{Family: "talos", Selector: map[string]string{"caph-image-name": "talos-v1.13.3-gvisor"}}
	if _, err := p.Resolve(context.Background(), sel, hcloud.ArchitectureX86); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := fc.lastOpts.LabelSelector; got != "caph-image-name=talos-v1.13.3-gvisor" {
		t.Fatalf("label selector not forwarded: got %q", got)
	}
}

func TestResolveUbuntu_LabelSelectorForwarded(t *testing.T) {
	fc := &mockImageClient{images: []*hcloud.Image{
		makeImage(2, hcloud.ImageTypeSystem, hcloud.ArchitectureX86, "Ubuntu 24.04", baseTime),
	}}
	p := NewProvider(fc)
	sel := apiv1.ImageSelector{Family: "ubuntu", Selector: map[string]string{"role": "worker"}}
	if _, err := p.Resolve(context.Background(), sel, hcloud.ArchitectureX86); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := fc.lastOpts.LabelSelector; got != "role=worker" {
		t.Fatalf("label selector not forwarded: got %q", got)
	}
}

// erroringImages fails every list call, standing in for a 429/5xx from hcloud.
type erroringImages struct{ err error }

func (e erroringImages) AllWithOpts(_ context.Context, _ hcloud.ImageListOpts) ([]*hcloud.Image, error) {
	return nil, e.err
}

// TestResolve_DistinguishesNotFoundFromAPIFailure pins the distinction the nodeclass
// controller depends on. An unreadable catalogue is not evidence that an image is
// absent: treating it as such clears a resolved architecture from NodeClass status, and
// instance-type selection then refuses that architecture, so Karpenter deletes
// NodeClaims over an API blip.
func TestResolve_DistinguishesNotFoundFromAPIFailure(t *testing.T) {
	sel := apiv1.ImageSelector{Family: "ubuntu"}

	// Catalogue readable, genuinely empty -> definitive.
	p := NewProvider(&mockImageClient{})
	_, err := p.Resolve(context.Background(), sel, hcloud.ArchitectureX86)
	if err == nil {
		t.Fatal("expected an error for an empty catalogue")
	}
	if !IsPermanent(err) {
		t.Errorf("empty catalogue should be permanent, got %T: %v", err, err)
	}

	// Catalogue unreadable -> must NOT look definitive.
	p = NewProvider(erroringImages{err: errors.New("rate limited (429)")})
	_, err = p.Resolve(context.Background(), sel, hcloud.ArchitectureX86)
	if err == nil {
		t.Fatal("expected an error when the list call fails")
	}
	if IsPermanent(err) {
		t.Errorf("an API failure must not be reported as permanent: %v", err)
	}
}

// TestResolve_UnsupportedFamilyIsPermanent verifies that a family the provider cannot
// handle is classified permanent, not transient. Retrying never changes the answer, and
// the transient class is load-bearing: it makes the nodeclass controller preserve the
// previous image IDs and keep the condition green, so misclassifying a definitively bad
// spec here means launching stale images indefinitely with nothing reported.
func TestResolve_UnsupportedFamilyIsPermanent(t *testing.T) {
	p := NewProvider(&mockImageClient{})
	_, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: "windows"}, hcloud.ArchitectureX86)
	if err == nil {
		t.Fatal("expected an error for an unsupported family")
	}
	if !IsPermanent(err) {
		t.Errorf("unsupported family must be permanent, got %T: %v", err, err)
	}
}

func TestResolveUbuntu_LabelSelectorFallsBackToNewestSnapshot(t *testing.T) {
	fc := &mockImageClient{images: []*hcloud.Image{
		makeImage(1, hcloud.ImageTypeSnapshot, hcloud.ArchitectureX86, "k8s-node-v1.36.4-old", baseTime),
		makeImage(2, hcloud.ImageTypeSnapshot, hcloud.ArchitectureX86, "k8s-node-v1.36.4-new", baseTime.Add(time.Hour)),
	}}
	p := NewProvider(fc)
	sel := apiv1.ImageSelector{Family: "ubuntu", Selector: map[string]string{"role": "k8s-node"}}
	img, err := p.Resolve(context.Background(), sel, hcloud.ArchitectureX86)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if img.ID != 2 {
		t.Errorf("expected newest snapshot (ID=2), got ID=%d", img.ID)
	}
	if got := fc.lastOpts.LabelSelector; got != "role=k8s-node" {
		t.Fatalf("label selector not forwarded to snapshot lookup: got %q", got)
	}
}

func TestResolveUbuntu_NoSelectorIgnoresSnapshots(t *testing.T) {
	p := NewProvider(&mockImageClient{images: []*hcloud.Image{
		makeImage(1, hcloud.ImageTypeSnapshot, hcloud.ArchitectureX86, "ubuntu snapshot", baseTime),
	}})
	if _, err := p.Resolve(context.Background(), apiv1.ImageSelector{Family: "ubuntu"}, hcloud.ArchitectureX86); err == nil {
		t.Fatal("expected no match: snapshots need a label selector")
	}
}
