package nodeclass

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/deadline"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/metrics"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/bootstrap"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/imagefamily"
)

const (
	resyncInterval = 5 * time.Minute

	// statusWriteTimeout bounds the status update at the end of a reconcile. It
	// is an API-server write, independent of the hcloud budget.
	statusWriteTimeout = 30 * time.Second
)

// NetworkGetter is the narrow hcloud networks API the controller needs.
type NetworkGetter interface {
	GetByID(ctx context.Context, id int64) (*hcloud.Network, *hcloud.Response, error)
}

// LocationLister is the narrow hcloud locations API the controller needs.
type LocationLister interface {
	All(ctx context.Context) ([]*hcloud.Location, error)
}

// FirewallGetter is the narrow hcloud firewalls API the controller needs.
type FirewallGetter interface {
	GetByID(ctx context.Context, id int64) (*hcloud.Firewall, *hcloud.Response, error)
}

// SSHKeyGetter is the narrow hcloud SSH keys API the controller needs.
type SSHKeyGetter interface {
	GetByID(ctx context.Context, id int64) (*hcloud.SSHKey, *hcloud.Response, error)
}

// Controller reconciles HCloudNodeClass status.
type Controller struct {
	kubeClient client.Client
	networks   NetworkGetter
	locations  LocationLister
	firewalls  FirewallGetter
	sshKeys    SSHKeyGetter
	images     *imagefamily.Provider
	recorder   events.EventRecorder
	// timeout bounds one reconcile (see deadline.Read). controller-runtime
	// imposes none, so without it a slow API call holds the worker indefinitely.
	timeout time.Duration
	// namespace is the provider's own namespace, where spec.bootstrap token
	// Secrets live.
	namespace string
}

// WithControllerNamespace sets the namespace spec.bootstrap token Secrets are read
// from.
func (c *Controller) WithControllerNamespace(namespace string) *Controller {
	c.namespace = namespace
	return c
}

func NewController(kubeClient client.Client, networks NetworkGetter, locations LocationLister, firewalls FirewallGetter, sshKeys SSHKeyGetter, images *imagefamily.Provider) *Controller {
	return &Controller{kubeClient: kubeClient, networks: networks, locations: locations, firewalls: firewalls, sshKeys: sshKeys, images: images, timeout: deadline.Read}
}

// warnf emits a Warning event on the HCloudNodeClass when a recorder is
// available, and is a no-op otherwise (the controller still sets conditions).
func (c *Controller) warnf(nc *apiv1.HCloudNodeClass, reason, action, format string, args ...interface{}) {
	if c.recorder == nil {
		return
	}
	c.recorder.Eventf(nc, nil, corev1.EventTypeWarning, reason, action, format, args...)
}

func (c *Controller) Name() string { return "nodeclass.status" }

func (c *Controller) Reconcile(ctx context.Context, nc *apiv1.HCloudNodeClass) (reconcile.Result, error) {
	// A handful of bounded GETs plus image resolution and a status write. The
	// transient-error paths below already treat a timed-out call like any other
	// unreadable API (Unknown, previous images preserved), so a deadline here can
	// only shorten a stall, never discard good state.
	parent := ctx
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	stored := nc.DeepCopy()

	// Network validation.
	net, _, err := c.networks.GetByID(ctx, nc.Spec.NetworkID)
	switch {
	case err != nil:
		nc.StatusConditions().SetUnknownWithReason(apiv1.ConditionTypeNetworkReady, "NetworkCheckFailed", err.Error())
		c.warnf(nc, "NetworkCheckFailed", "ValidateNetwork", "network check failed: %v", err)
	case net == nil:
		nc.StatusConditions().SetFalse(apiv1.ConditionTypeNetworkReady, "NetworkNotFound", "configured networkID does not exist")
		c.warnf(nc, "NetworkNotFound", "ValidateNetwork", "networkID %d does not exist", nc.Spec.NetworkID)
	default:
		nc.StatusConditions().SetTrue(apiv1.ConditionTypeNetworkReady)
	}

	// Validate declared locations and their network-zone coverage.
	if reason, msg, unknown, ok := c.validateLocations(ctx, nc, net); ok {
		nc.StatusConditions().SetTrue(apiv1.ConditionTypeLocationsReady)
	} else if unknown {
		nc.StatusConditions().SetUnknownWithReason(apiv1.ConditionTypeLocationsReady, reason, msg)
		c.warnf(nc, reason, "ValidateLocations", "%s", msg)
	} else {
		nc.StatusConditions().SetFalse(apiv1.ConditionTypeLocationsReady, reason, msg)
		c.warnf(nc, reason, "ValidateLocations", "%s", msg)
	}

	// Validate referenced firewalls and SSH keys exist.
	if reason, msg, unknown, ok := c.validateResources(ctx, nc); ok {
		nc.StatusConditions().SetTrue(apiv1.ConditionTypeResourcesReady)
	} else if unknown {
		nc.StatusConditions().SetUnknownWithReason(apiv1.ConditionTypeResourcesReady, reason, msg)
		c.warnf(nc, reason, "ValidateResources", "%s", msg)
	} else {
		nc.StatusConditions().SetFalse(apiv1.ConditionTypeResourcesReady, reason, msg)
		c.warnf(nc, reason, "ValidateResources", "%s", msg)
	}

	// Validate the userData secret ref (if set).
	if reason, msg, unknown, ok := c.validateUserData(ctx, nc); ok {
		nc.StatusConditions().SetTrue(apiv1.ConditionTypeUserDataReady)
	} else if unknown {
		nc.StatusConditions().SetUnknownWithReason(apiv1.ConditionTypeUserDataReady, reason, msg)
		c.warnf(nc, reason, "ValidateUserData", "%s", msg)
	} else {
		nc.StatusConditions().SetFalse(apiv1.ConditionTypeUserDataReady, reason, msg)
		c.warnf(nc, reason, "ValidateUserData", "%s", msg)
	}

	// Validate the provider-rendered bootstrap (if set): everything that can make a
	// launch fail before a server exists, surfaced here rather than per NodeClaim.
	if reason, msg, unknown, ok := c.validateBootstrap(ctx, nc); ok {
		nc.StatusConditions().SetTrue(apiv1.ConditionTypeBootstrapReady)
	} else if unknown {
		nc.StatusConditions().SetUnknownWithReason(apiv1.ConditionTypeBootstrapReady, reason, msg)
		c.warnf(nc, reason, "ValidateBootstrap", "%s", msg)
	} else {
		nc.StatusConditions().SetFalse(apiv1.ConditionTypeBootstrapReady, reason, msg)
		c.warnf(nc, reason, "ValidateBootstrap", "%s", msg)
	}

	// Image resolution for both architectures.
	// Merge this pass with what status already holds. Selection reads ResolvedImages to
	// decide which architectures are launchable, so the two failure modes must not be
	// conflated: a catalogue that says "no such image" is evidence, a catalogue that
	// could not be read is not. Treating the latter as the former clears a resolved
	// architecture and makes Karpenter delete NodeClaims over an API blip.
	//
	// Carry-forward is only valid while the spec is unchanged. Once metadata.generation
	// moves past the generation those IDs were resolved under, they answer a question the
	// spec no longer asks, and reusing them would launch the previous image while
	// reporting the NodeClass green.
	//
	// The generation is read back from the ImagesReady condition rather than a status
	// field of our own: every branch below writes that condition in the same pass that
	// writes ResolvedImages, so its observedGeneration always describes the entries
	// currently in status. See HCloudNodeClassStatus.ResolvedImages for why a dedicated
	// field cannot be used.
	if resolvedImagesGeneration(nc) != nc.Generation {
		// The spec has moved since these IDs were resolved. They answer a question that
		// is no longer being asked, so drop them rather than let a failed lookup below
		// preserve them: an operator who repins imageSelector must never keep launching
		// the previous image. Unlike an API blip, this is a deliberate spec change.
		nc.Status.ResolvedImages = nil
	}
	prev := make(map[string]int64, len(nc.Status.ResolvedImages))
	for _, ri := range nc.Status.ResolvedImages {
		prev[ri.Architecture] = ri.ImageID
	}
	var resolved []apiv1.ResolvedImage
	var transientErrs, definitiveErrs []error
	for _, r := range c.resolveImages(ctx, nc) {
		switch {
		case r.err == nil:
			resolved = append(resolved, apiv1.ResolvedImage{Architecture: string(r.arch), ImageID: r.imageID})
		case r.transient:
			transientErrs = append(transientErrs, r.err)
			// Absence is unknown, so carry the last known good entry rather than
			// reporting this architecture as gone.
			if id, ok := prev[string(r.arch)]; ok {
				resolved = append(resolved, apiv1.ResolvedImage{Architecture: string(r.arch), ImageID: id})
			}
		default:
			definitiveErrs = append(definitiveErrs, r.err)
		}
	}
	// Surface transient failures whatever the outcome. A partially successful pass still
	// sets ImagesReady=True so a healthy architecture keeps provisioning, which means the
	// condition alone would never reveal that the other architecture is running on a
	// preserved ID rather than a fresh answer.
	if len(transientErrs) > 0 {
		ierr := errors.Join(transientErrs...)
		logf.FromContext(ctx).Error(ierr, "image lookup failed; affected architectures keep their previous image",
			"nodeClass", nc.Name)
		metrics.RecordImageResolutionError()
		c.warnf(nc, "ImageResolutionErrored", "ResolveImages", "image catalogue unreadable: %v", ierr)
	}
	switch {
	case len(resolved) > 0:
		nc.Status.ResolvedImages = resolved
		nc.StatusConditions().SetTrue(apiv1.ConditionTypeImagesReady)
	case len(transientErrs) > 0:
		// Nothing resolved and at least one lookup was unreadable: report Unknown, as
		// every other condition here does for an API error.
		//
		// Anything still in status here belongs to an architecture that failed
		// DEFINITIVELY -- a transient one would have been carried into `resolved` above
		// -- so it is dropped rather than left advertising an image the catalogue says
		// is gone. Report those errors alongside the transient ones too: they are the
		// actionable half, and reporting only the blip hides the deleted image behind a
		// 503 the operator can do nothing about.
		nc.Status.ResolvedImages = nil
		nc.StatusConditions().SetUnknownWithReason(apiv1.ConditionTypeImagesReady,
			"ImageResolutionErrored",
			errors.Join(errors.Join(transientErrs...), errors.Join(definitiveErrs...)).Error())
		if len(definitiveErrs) > 0 {
			ierr := errors.Join(definitiveErrs...)
			c.warnf(nc, "ImageResolutionFailed", "ResolveImages", "image resolution failed: %v", ierr)
		}
	default:
		// The catalogue was readable and holds nothing for any architecture.
		ierr := errors.Join(definitiveErrs...)
		nc.Status.ResolvedImages = nil
		nc.StatusConditions().SetFalse(apiv1.ConditionTypeImagesReady, "ImageResolutionFailed", ierr.Error())
		c.warnf(nc, "ImageResolutionFailed", "ResolveImages", "image resolution failed: %v", ierr)
	}

	if !equality.Semantic.DeepEqual(stored, nc) {
		// Written under its own short budget from the caller's context, not the
		// reconcile budget: when the hcloud calls above ran the budget out, the
		// Unknown conditions they produced are exactly what must reach status.
		writeCtx, cancelWrite := context.WithTimeout(parent, statusWriteTimeout)
		defer cancelWrite()
		if err := c.kubeClient.Status().Update(writeCtx, nc); err != nil {
			return reconcile.Result{}, err
		}
	}
	// Requeue periodically so the Ready condition re-reflects reality (e.g. a
	// network deleted out-of-band, or a newer image published).
	return reconcile.Result{RequeueAfter: resyncInterval}, nil
}

// validateLocations checks that every declared location exists and has a subnet
// in its network zone. Returns ok=true when all checks pass; unknown=true when
// network-zone coverage cannot be validated.
func (c *Controller) validateLocations(ctx context.Context, nc *apiv1.HCloudNodeClass, network *hcloud.Network) (reason, msg string, unknown, ok bool) {
	locations, err := c.locations.All(ctx)
	if err != nil {
		return "LocationCheckFailed", err.Error(), true, false
	}

	available := make(map[string]*hcloud.Location, len(locations))
	for _, location := range locations {
		available[location.Name] = location
	}

	var missing []string
	for _, name := range nc.Spec.Locations {
		if available[name] == nil {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return "LocationNotFound", fmt.Sprintf("locations do not exist: %s", strings.Join(missing, ", ")), false, false
	}

	if network == nil {
		return "NetworkUnavailable", "cannot validate location network zones", true, false
	}

	coveredZones := make(map[hcloud.NetworkZone]struct{}, len(network.Subnets))
	for _, subnet := range network.Subnets {
		coveredZones[subnet.NetworkZone] = struct{}{}
	}

	var uncovered []string
	for _, name := range nc.Spec.Locations {
		location := available[name]
		if _, ok := coveredZones[location.NetworkZone]; !ok {
			uncovered = append(uncovered, fmt.Sprintf("%s (%s)", name, location.NetworkZone))
		}
	}
	if len(uncovered) > 0 {
		return "NetworkZoneNotCovered", fmt.Sprintf("locations are not covered by network %d subnets: %s", nc.Spec.NetworkID, strings.Join(uncovered, ", ")), false, false
	}

	return "", "", false, true
}

// validateResources checks every referenced firewall and SSH key exists.
// Returns ok=true when all exist; unknown=true on a transient API error.
func (c *Controller) validateResources(ctx context.Context, nc *apiv1.HCloudNodeClass) (reason, msg string, unknown, ok bool) {
	for _, id := range nc.Spec.FirewallIDs {
		fw, _, err := c.firewalls.GetByID(ctx, id)
		if err != nil {
			return "FirewallCheckFailed", err.Error(), true, false
		}
		if fw == nil {
			return "FirewallNotFound", fmt.Sprintf("firewall %d does not exist", id), false, false
		}
	}
	for _, id := range nc.Spec.SSHKeyIDs {
		key, _, err := c.sshKeys.GetByID(ctx, id)
		if err != nil {
			return "SSHKeyCheckFailed", err.Error(), true, false
		}
		if key == nil {
			return "SSHKeyNotFound", fmt.Sprintf("ssh key %d does not exist", id), false, false
		}
	}
	return "", "", false, true
}

// validateUserData checks that the referenced userData Secret and key exist and
// are non-empty. Returns ok=true when there is nothing to validate or it
// resolves successfully; unknown=true on a transient API error.
func (c *Controller) validateUserData(ctx context.Context, nc *apiv1.HCloudNodeClass) (reason, msg string, unknown, ok bool) {
	ref := nc.Spec.UserDataSecretRef
	if ref == nil {
		return "", "", false, true
	}
	secret := &corev1.Secret{}
	if err := c.kubeClient.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "UserDataSecretNotFound", fmt.Sprintf("secret %s/%s not found", ref.Namespace, ref.Name), false, false
		}
		return "UserDataCheckFailed", err.Error(), true, false
	}
	if v, present := secret.Data[ref.Key]; !present || len(v) == 0 {
		return "UserDataKeyMissing", fmt.Sprintf("secret %s/%s has no non-empty key %q", ref.Namespace, ref.Name, ref.Key), false, false
	}
	return "", "", false, true
}

// validateBootstrap checks that spec.bootstrap can be rendered: the join token
// Secret and key exist in the provider's namespace, and the userData extension
// merges with the rendered cloud-config. ok=true when bootstrap is unset.
func (c *Controller) validateBootstrap(ctx context.Context, nc *apiv1.HCloudNodeClass) (reason, msg string, unknown, ok bool) {
	b := nc.Spec.Bootstrap
	if b == nil {
		return "", "", false, true
	}
	token, err := bootstrap.ReadToken(ctx, c.kubeClient, c.namespace, b.TokenSecretRef)
	if err != nil {
		if errors.Is(err, bootstrap.ErrTokenUnavailable) {
			return "BootstrapTokenUnavailable", err.Error(), false, false
		}
		return "BootstrapCheckFailed", err.Error(), true, false
	}
	extension := nc.Spec.UserData
	if ref := nc.Spec.UserDataSecretRef; ref != nil {
		secret := &corev1.Secret{}
		if err := c.kubeClient.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, secret); err != nil {
			// UserDataReady reports the Secret itself; the merge cannot be checked
			// without it.
			return "BootstrapCheckFailed", fmt.Sprintf("cannot check the userData extension: %v", err), true, false
		}
		extension = string(secret.Data[ref.Key])
	}
	if _, err := bootstrap.Render(bootstrap.Input{Bootstrap: *b, Kubelet: nc.Spec.Kubelet, Token: token, UserData: extension}); err != nil {
		if bootstrap.IsInvalid(err) {
			return "BootstrapInvalid", err.Error(), false, false
		}
		return "BootstrapCheckFailed", err.Error(), true, false
	}
	return "", "", false, true
}

// resolvedImagesGeneration returns the metadata.generation that the entries currently
// in status.resolvedImages were resolved under. The rule is shared with the cloud
// provider, which applies it on the read side so a launch never uses an image that
// answers a superseded spec; see HCloudNodeClass.ResolvedImagesGeneration for why it is
// read back from the ImagesReady condition and why absent must read as zero.
func resolvedImagesGeneration(nc *apiv1.HCloudNodeClass) int64 {
	return nc.ResolvedImagesGeneration()
}

// imageResolution is the outcome of resolving one architecture. transient marks an
// error that says nothing about whether the image exists -- the catalogue could not be
// read -- as opposed to the catalogue reporting no match.
type imageResolution struct {
	arch      hcloud.Architecture
	imageID   int64
	err       error
	transient bool
}

func (c *Controller) resolveImages(ctx context.Context, nc *apiv1.HCloudNodeClass) []imageResolution {
	// hcloud image IDs are global (not per-location), so resolve one image per
	// architecture. Resolve each architecture independently: many clusters only
	// have an image for a single arch (e.g. an all-amd64 cluster has no arm64
	// Talos snapshot), and a NodeClass is usable as long as at least one arch
	// resolves.
	archs := []hcloud.Architecture{hcloud.ArchitectureX86, hcloud.ArchitectureARM}
	out := make([]imageResolution, 0, len(archs))
	for _, arch := range archs {
		img, err := c.images.Resolve(ctx, nc.Spec.ImageSelector, arch)
		switch {
		case err != nil:
			out = append(out, imageResolution{
				arch:      arch,
				err:       fmt.Errorf("%s: %w", arch, err),
				transient: !imagefamily.IsPermanent(err),
			})
		case img.Architecture != arch:
			// hcloud filters by architecture server-side and the image provider does
			// not re-check. Create launches the ID recorded here and derives the
			// architecture from the NodeClaim, so nothing downstream can catch a
			// mismatch on an entry that reached status: an unverified one boots a node
			// that fails every workload with "exec format error". Create's live-lookup
			// fallback applies the same check to the images it resolves itself.
			out = append(out, imageResolution{
				arch: arch,
				err: fmt.Errorf("%s: catalogue returned image %d with architecture %q",
					arch, img.ID, img.Architecture),
			})
		default:
			out = append(out, imageResolution{arch: arch, imageID: img.ID})
		}
	}
	return out
}

// Register wires the controller into the manager.
func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	c.recorder = m.GetEventRecorder(c.Name())
	return controllerruntime.NewControllerManagedBy(m).
		For(&apiv1.HCloudNodeClass{}).
		Named(c.Name()).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
