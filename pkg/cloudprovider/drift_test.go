package cloudprovider_test

import (
	"context"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/cloudprovider"
)

func drifted(t *testing.T, nc *apiv1.HCloudNodeClass, mutate func(*karpv1.NodeClaim)) karpcp.DriftReason {
	t.Helper()
	cp, claim := buildCP(t, nc, baselineServer())
	claim.Labels[corev1.LabelArchStable] = "amd64"
	if mutate != nil {
		mutate(claim)
	}
	reason, err := cp.IsDrifted(context.Background(), claim)
	if err != nil {
		t.Fatal(err)
	}
	return reason
}

// The NodeClaim was launched from image 42 (baselineServer). A pinned selector that
// now resolves to image 43 must roll it.
func TestIsDrifted_ImagePinnedResolvesNewImage(t *testing.T) {
	for name, sel := range map[string]apiv1.ImageSelector{
		"version":  {Family: "ubuntu", Version: "24.04"},
		"selector": {Family: "talos", Selector: map[string]string{"caph-image-name": "talos-v1.13"}},
	} {
		t.Run(name, func(t *testing.T) {
			nc := baselineNodeClass()
			nc.Spec.ImageSelector = sel
			withResolvedImages(nc, apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 43})
			if got := drifted(t, nc, nil); got != cloudprovider.DriftImage {
				t.Errorf("want ImageDrift, got %q", got)
			}
		})
	}
}

func TestIsDrifted_ImageNoDrift(t *testing.T) {
	pinned := func() *apiv1.HCloudNodeClass {
		nc := baselineNodeClass()
		nc.Spec.ImageSelector.Version = "24.04"
		return nc
	}
	for name, tc := range map[string]struct {
		nc     *apiv1.HCloudNodeClass
		mutate func(*karpv1.NodeClaim)
	}{
		// A bare selector means "newest": chasing it would roll the fleet on any
		// matching snapshot upload.
		"unpinned selector resolving a newer image": {
			withResolvedImages(baselineNodeClass(), apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 43}), nil},
		"pinned and still resolving the launch image": {
			withResolvedImages(pinned(), apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 42}), nil},
		// The new image is for the other architecture only.
		"pinned, other architecture changed": {
			withResolvedImages(pinned(),
				apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 42},
				apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureARM), ImageID: 99}), nil},
		"pinned, no resolution for this architecture": {
			withResolvedImages(pinned(), apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureARM), ImageID: 99}), nil},
		// Status answers a superseded spec: never compare a repin against the previous
		// selector's image.
		"pinned, status from an older generation": {
			func() *apiv1.HCloudNodeClass {
				nc := withResolvedImages(pinned(), apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 43})
				nc.Generation = 2
				return nc
			}(), nil},
		"pinned, NodeClaim records no image": {
			withResolvedImages(pinned(), apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 43}),
			func(c *karpv1.NodeClaim) { c.Status.ImageID = "" }},
		"pinned, NodeClaim has no architecture label": {
			withResolvedImages(pinned(), apiv1.ResolvedImage{Architecture: string(hcloud.ArchitectureX86), ImageID: 43}),
			func(c *karpv1.NodeClaim) { delete(c.Labels, corev1.LabelArchStable) }},
	} {
		t.Run(name, func(t *testing.T) {
			if got := drifted(t, tc.nc, tc.mutate); got != "" {
				t.Errorf("want no drift, got %q", got)
			}
		})
	}
}

func stampHash(hash, version string) func(*karpv1.NodeClaim) {
	return func(c *karpv1.NodeClaim) {
		c.Annotations = map[string]string{
			apiv1.AnnotationNodeClassHash:        hash,
			apiv1.AnnotationNodeClassHashVersion: version,
		}
	}
}

func TestIsDrifted_NodeClassHash(t *testing.T) {
	launched := baselineNodeClass()
	launchedHash := launched.Hash()

	edited := baselineNodeClass()
	edited.Spec.UserData = "#cloud-config\nruncmd: [echo new]"

	for name, tc := range map[string]struct {
		nc     *apiv1.HCloudNodeClass
		mutate func(*karpv1.NodeClaim)
		want   karpcp.DriftReason
	}{
		"unchanged spec":                     {baselineNodeClass(), stampHash(launchedHash, apiv1.NodeClassHashVersion), ""},
		"edited userData":                    {edited, stampHash(launchedHash, apiv1.NodeClassHashVersion), cloudprovider.DriftNodeClass},
		"NodeClaim launched before the hash": {edited, nil, ""},
		"NodeClaim on an older hash version": {edited, stampHash(launchedHash, "v0"), ""},
		"hash without a version": {edited, func(c *karpv1.NodeClaim) {
			c.Annotations = map[string]string{apiv1.AnnotationNodeClassHash: launchedHash}
		}, ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := drifted(t, tc.nc, tc.mutate); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Create records the hash of the spec it launched from, which is what drift compares
// against later; core copies these annotations onto the persisted NodeClaim.
func TestCreate_StampsNodeClassHash(t *testing.T) {
	nc := baselineNodeClass()
	nc.Spec.SSHKeyIDs = []int64{3, 1}
	cp, _, _ := buildCPWithTypes(t, nc, []*hcloud.ServerType{cx22Type()})
	out, err := cp.Create(context.Background(), createNodeClaim())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if got, want := out.Annotations[apiv1.AnnotationNodeClassHash], nc.Hash(); got != want || got == "" {
		t.Errorf("hash annotation %q, want %q", got, want)
	}
	if got := out.Annotations[apiv1.AnnotationNodeClassHashVersion]; got != apiv1.NodeClassHashVersion {
		t.Errorf("hash version annotation %q, want %q", got, apiv1.NodeClassHashVersion)
	}
}
