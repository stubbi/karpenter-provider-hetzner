package imagefamily

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
)

// labelSelectorString renders an hcloud label selector from a map (sorted for
// determinism). hcloud label keys and values are restricted to [a-zA-Z0-9._-]
// (max 63 chars each), so no quoting or escaping is needed.
func labelSelectorString(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, k+"="+m[k])
	}
	return strings.Join(parts, ",")
}

// PermanentError reports that retrying will not change the answer: the catalogue was
// read and holds no match, or the selector can never match. It is distinct from a
// failure to read the catalogue at all, where absence is simply unknown.
//
// Callers must not treat an unreadable catalogue as proof that no image exists: an
// HCloudNodeClass that clears a resolved architecture on a 429 makes Karpenter delete
// NodeClaims over an API blip. Errors that are not a PermanentError are therefore
// assumed transient, which is the safe direction for an error whose cause is unknown --
// a misclassified transient keeps stale state, a misclassified permanent discards good
// state. Inputs known to be bad must be classified permanent explicitly, since the
// transient path preserves state and keeps the NodeClass green.
type PermanentError struct{ error }

func newPermanentError(format string, args ...any) *PermanentError {
	return &PermanentError{error: fmt.Errorf(format, args...)}
}

// IsPermanent reports whether err will produce the same result on every retry.
func IsPermanent(err error) bool {
	var e *PermanentError
	return errors.As(err, &e)
}

// ImageClient is the narrow interface for the hcloud images API needed by this provider.
type ImageClient interface {
	AllWithOpts(ctx context.Context, opts hcloud.ImageListOpts) ([]*hcloud.Image, error)
}

// Provider resolves OS images from an HCloudNodeClass ImageSelector.
type Provider struct {
	client ImageClient
}

// NewProvider creates a new image family provider.
func NewProvider(client ImageClient) *Provider {
	return &Provider{client: client}
}

// Resolve returns the best matching image for the given selector and architecture.
// Supported families: "ubuntu", "talos".
func (p *Provider) Resolve(ctx context.Context, selector apiv1.ImageSelector, arch hcloud.Architecture) (*hcloud.Image, error) {
	log := logf.FromContext(ctx)
	ls := labelSelectorString(selector.Selector)
	var img *hcloud.Image
	var err error
	switch strings.ToLower(selector.Family) {
	case "ubuntu":
		img, err = p.resolveUbuntu(ctx, selector.Version, arch, ls)
	case "talos":
		img, err = p.resolveTalos(ctx, selector.Version, arch, ls)
	default:
		return nil, newPermanentError("unsupported image family %q: must be one of ubuntu, talos", selector.Family)
	}
	if err != nil {
		return nil, err
	}
	log.V(1).Info("resolved image",
		"family", selector.Family,
		"arch", string(arch),
		"imageID", img.ID,
	)
	return img, nil
}

// resolveUbuntu finds a system image whose description contains "ubuntu" and optionally the given version,
// falling back to the newest labelled snapshot when a label selector is set.
// Returns the first matching image.
func (p *Provider) resolveUbuntu(ctx context.Context, version string, arch hcloud.Architecture, ls string) (*hcloud.Image, error) {
	images, err := p.client.AllWithOpts(ctx, hcloud.ImageListOpts{
		Type:         []hcloud.ImageType{hcloud.ImageTypeSystem},
		Architecture: []hcloud.Architecture{arch},
		ListOpts:     hcloud.ListOpts{LabelSelector: ls},
	})
	if err != nil {
		return nil, fmt.Errorf("listing ubuntu images: %w", err)
	}

	for _, img := range images {
		desc := strings.ToLower(img.Description)
		if !strings.Contains(desc, "ubuntu") {
			continue
		}
		if version != "" && !strings.Contains(desc, version) {
			continue
		}
		return img, nil
	}

	// Hetzner system images carry no labels, so a selector that matched none
	// means a custom snapshot (e.g. a Packer-built node image): newest match.
	if ls != "" {
		return p.newestSnapshot(ctx, "ubuntu", "", version, arch, ls)
	}
	if version != "" {
		return nil, newPermanentError("no ubuntu image found for version %q and arch %q", version, arch)
	}
	return nil, newPermanentError("no ubuntu image found for arch %q", arch)
}

// resolveTalos finds the newest snapshot image whose description contains "talos" and optionally the given version.
func (p *Provider) resolveTalos(ctx context.Context, version string, arch hcloud.Architecture, ls string) (*hcloud.Image, error) {
	return p.newestSnapshot(ctx, "talos", "talos", version, arch, ls)
}

// newestSnapshot returns the newest snapshot matching the label selector whose
// description contains descMatch (if set) and version (if set).
func (p *Provider) newestSnapshot(ctx context.Context, family, descMatch, version string, arch hcloud.Architecture, ls string) (*hcloud.Image, error) {
	images, err := p.client.AllWithOpts(ctx, hcloud.ImageListOpts{
		Type:         []hcloud.ImageType{hcloud.ImageTypeSnapshot},
		Architecture: []hcloud.Architecture{arch},
		ListOpts:     hcloud.ListOpts{LabelSelector: ls},
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s images: %w", family, err)
	}

	var best *hcloud.Image
	for _, img := range images {
		desc := strings.ToLower(img.Description)
		if descMatch != "" && !strings.Contains(desc, descMatch) {
			continue
		}
		if version != "" && !strings.Contains(desc, version) {
			continue
		}
		if best == nil || img.Created.After(best.Created) {
			best = img
		}
	}

	if best == nil {
		if version != "" {
			return nil, newPermanentError("no %s snapshot found for version %q and arch %q", family, version, arch)
		}
		return nil, newPermanentError("no %s snapshot found for arch %q", family, arch)
	}
	return best, nil
}
