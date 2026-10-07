package cloudprovider

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/awslabs/operatorpkg/status"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"
	"sigs.k8s.io/karpenter/pkg/scheduling"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/metrics"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/imagefamily"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instance"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instancetype"
)

const providerName = "hetzner"

// Drift reasons for HCloud-specific drift detection.
const (
	DriftImage      karpcp.DriftReason = "ImageDrift"
	DriftNetwork    karpcp.DriftReason = "NetworkDrift"
	DriftFirewall   karpcp.DriftReason = "FirewallDrift"
	DriftServerType karpcp.DriftReason = "ServerTypeDrift"
	DriftLocation   karpcp.DriftReason = "LocationDrift"
	DriftLabels     karpcp.DriftReason = "LabelsDrift"
)

// CloudProvider implements the Karpenter CloudProvider interface for Hetzner Cloud.
type CloudProvider struct {
	kubeClient       client.Client
	instanceProvider *instance.Provider
	typeProvider     *instancetype.Provider
	imageProvider    *imagefamily.Provider
}

// NewCloudProvider creates a new CloudProvider.
func NewCloudProvider(
	kubeClient client.Client,
	instanceProvider *instance.Provider,
	typeProvider *instancetype.Provider,
	imageProvider *imagefamily.Provider,
) *CloudProvider {
	return &CloudProvider{
		kubeClient:       kubeClient,
		instanceProvider: instanceProvider,
		typeProvider:     typeProvider,
		imageProvider:    imageProvider,
	}
}

// Name returns the cloud provider name.
func (cp *CloudProvider) Name() string {
	return providerName
}

// GetSupportedNodeClasses returns the supported node class types.
func (cp *CloudProvider) GetSupportedNodeClasses() []status.Object {
	return []status.Object{&apiv1.HCloudNodeClass{}}
}

// RepairPolicies returns the repair policies for unhealthy nodes.
func (cp *CloudProvider) RepairPolicies() []karpcp.RepairPolicy {
	return []karpcp.RepairPolicy{
		{
			ConditionType:      corev1.NodeReady,
			ConditionStatus:    corev1.ConditionFalse,
			TolerationDuration: 5 * time.Minute,
		},
		{
			ConditionType:      corev1.NodeReady,
			ConditionStatus:    corev1.ConditionUnknown,
			TolerationDuration: 5 * time.Minute,
		},
	}
}

// hcloudArchFor maps an instance type's kubernetes.io/arch value onto the hcloud
// architecture used for image lookups.
func hcloudArchFor(it *karpcp.InstanceType) hcloud.Architecture {
	if it.Requirements.Get(corev1.LabelArchStable).Any() == "arm64" {
		return hcloud.ArchitectureARM
	}
	return hcloud.ArchitectureX86
}

// resolveImage returns the image to launch for arch, preferring the ID the nodeclass
// controller published in status for the CURRENT spec generation. It falls back to a
// live lookup only when status has no such entry -- the not-yet-reconciled case, which
// the selection gate also lets through, and which includes a spec edit that status has
// not caught up with (see CurrentResolvedImages).
//
// A definitive miss from that fallback is returned as NodeClassNotReadyError so core
// deletes the NodeClaim. A bare error would land in launch.go's default branch, parking
// the claim at Launched=Unknown and requeuing it indefinitely. Errors that merely mean
// the catalogue was unreadable stay untyped and retryable.
func (cp *CloudProvider) resolveImage(ctx context.Context, nodeClass *apiv1.HCloudNodeClass, arch hcloud.Architecture) (*hcloud.Image, error) {
	for _, ri := range nodeClass.CurrentResolvedImages() {
		if ri.Architecture == string(arch) {
			return &hcloud.Image{ID: ri.ImageID, Architecture: arch}, nil
		}
	}
	image, err := cp.imageProvider.Resolve(ctx, nodeClass.Spec.ImageSelector, arch)
	if err != nil {
		if imagefamily.IsPermanent(err) {
			return nil, karpcp.NewNodeClassNotReadyError(
				fmt.Errorf("HCloudNodeClass %q has no image for architecture %s: %w", nodeClass.Name, arch, err))
		}
		return nil, fmt.Errorf("resolving image: %w", err)
	}
	// Never provision a server whose image architecture diverges from the architecture the
	// NodeClaim requires: such a node boots and then fails every workload with "exec
	// format error". This is the only place the check can bite -- entries read from status
	// above carry the architecture they were filed under, and the nodeclass controller
	// verifies that against the image before recording it (see resolveImages).
	if image.Architecture != arch {
		return nil, karpcp.NewNodeClassNotReadyError(fmt.Errorf(
			"HCloudNodeClass %q resolved image %d with architecture %q for architecture %s",
			nodeClass.Name, image.ID, image.Architecture, arch))
	}
	return image, nil
}

// hasResolvedImage reports whether the NodeClass resolved an image for arch under the
// current spec generation. An empty list means the nodeclass controller has not reported
// on this spec yet, so every architecture stays eligible: filtering on absent status
// would block provisioning entirely.
func hasResolvedImage(nodeClass *apiv1.HCloudNodeClass, arch hcloud.Architecture) bool {
	resolved := nodeClass.CurrentResolvedImages()
	if len(resolved) == 0 {
		return true
	}
	for _, ri := range resolved {
		if ri.Architecture == string(arch) {
			return true
		}
	}
	return false
}

// Create provisions a new Hetzner server for the given NodeClaim.
func (cp *CloudProvider) Create(ctx context.Context, nodeClaim *karpv1.NodeClaim) (*karpv1.NodeClaim, error) {
	log := logf.FromContext(ctx)
	nodeClass, err := cp.resolveNodeClass(ctx, nodeClaim.Spec.NodeClassRef)
	if err != nil {
		return nil, fmt.Errorf("resolving node class: %w", err)
	}

	// Get instance types for the node class locations.
	//
	// NOTE: this reads the provider's own catalogue. When the NodeOverlay feature gate is
	// enabled, cmd/controller wraps this provider in karpenter's overlay.Decorate, which
	// overrides GetInstanceTypes but inherits Create unchanged -- so core ranks types on
	// overlaid prices while the selection below ranks on raw hcloud prices, and the two
	// can disagree. The gate defaults off. This is closable here rather than upstream:
	// Create needs the nodeoverlay store, not the decorator, and the store is public and
	// already held in main.go. Left to a follow-up to keep this change reviewable.
	instanceTypes, err := cp.typeProvider.List(ctx, nodeClass)
	if err != nil {
		return nil, fmt.Errorf("listing instance types: %w", err)
	}

	// Filter instance types by NodeClaim requirements.
	reqs := scheduling.NewNodeSelectorRequirementsWithMinValues(nodeClaim.Spec.Requirements...)
	// offering is the one selected was ranked on, and therefore the one it is launched
	// into: deriving both from a single expression keeps the price we chose on and the
	// price we pay identical.
	var selected *karpcp.InstanceType
	var offering *karpcp.Offering
	// Distinguish the two ways selection can come up empty. A missing image is a
	// NodeClass configuration problem that no retry fixes; no available offering is
	// transient capacity. They need different errors, and core retries them differently.
	archsMissingImages := sets.New[string]()
	capacityBlocked := false
	// Explicit min-scan rather than core's OrderByPrice: that helper sorts with the
	// unstable sort.Slice and defines no tiebreak, so types tied on price come out in an
	// order that shifts when an unrelated type is added or an offering's availability
	// flips -- two NodeClaims from one NodePool could land on different shapes. Ties break
	// on name here, so selection is reproducible from the same catalogue. Scanning also
	// avoids re-deriving every type's price inside a sort comparator.
	for _, it := range instanceTypes {
		if !reqs.IsCompatible(it.Requirements, scheduling.AllowUndefinedWellKnownLabels) {
			continue
		}
		// Skip architectures the NodeClass has no image for. The image is resolved once,
		// after this loop has picked a winner, so a miss there is terminal: nothing
		// demotes the type or marks it unavailable, and core requeues the same candidate
		// forever. Cheapest-first
		// makes that reachable whenever a pool permits both architectures and the ARM
		// candidate prices lower, while a single-arch cluster is explicitly supported
		// (see resolveImages).
		if arch := hcloudArchFor(it); !hasResolvedImage(nodeClass, arch) {
			archsMissingImages.Insert(string(arch))
			log.V(1).Info("skipping instance type: node class has no resolved image",
				"instanceType", it.Name, "arch", string(arch), "nodeClass", nodeClass.Name)
			continue
		}
		cheapest := it.Offerings.Available().Compatible(reqs).Cheapest()
		if cheapest == nil {
			capacityBlocked = true
			continue
		}
		if selected == nil || cheapest.Price < offering.Price ||
			(cheapest.Price == offering.Price && it.Name < selected.Name) {
			selected, offering = it, cheapest
		}
	}
	// Counted once per architecture per launch, not once per skipped type: incrementing
	// inside the loop above would scale the counter with the size of the hcloud catalogue
	// rather than with the number of launches that had to route around a missing image,
	// which is the thing worth alerting on. Usually non-terminal -- a cheaper candidate is
	// dropped and a pricier one launches, so the error below never fires and nothing else
	// would show that a mislabelled image is quietly costing money.
	for _, arch := range sets.List(archsMissingImages) {
		metrics.RecordSelectionSkipped(arch, "no_resolved_image")
	}
	if selected == nil {
		// Only claim capacity when a candidate cleared the image gate and still had no
		// offering. Otherwise the image is the whole story, and dressing it as capacity
		// sends operators to their Hetzner quotas for a problem in the NodeClass.
		if archsMissingImages.Len() > 0 && !capacityBlocked {
			// NodeClassNotReady, not a bare error: core switches on the type in launch.go.
			// Both this and InsufficientCapacity delete the NodeClaim so the scheduler can
			// try another shape; anything else parks it in Launched=Unknown and requeues
			// forever. This branch also reports reason=nodeclass_not_ready rather than
			// blaming capacity.
			return nil, karpcp.NewNodeClassNotReadyError(fmt.Errorf(
				"no instance type satisfies requirements for nodeclaim %s: HCloudNodeClass %q has no resolved image for architecture %s",
				nodeClaim.Name, nodeClass.Name, strings.Join(sets.List(archsMissingImages), ", ")))
		}
		return nil, karpcp.NewInsufficientCapacityError(fmt.Errorf("no instance type satisfies requirements for nodeclaim %s", nodeClaim.Name))
	}

	// Determine architecture from the selected instance type.
	arch := selected.Requirements.Get(corev1.LabelArchStable).Any()
	hcloudArch := hcloudArchFor(selected)
	log.Info("selected instance type", "instanceType", selected.Name, "arch", arch)

	// Resolve OS image. Prefer the ID the nodeclass controller already published: the
	// selection gate above admitted this architecture on the strength of that entry, so
	// reading it here keeps gate and launch on one source. A second, live lookup can
	// disagree with the gate -- or fail after a candidate has already been chosen, which
	// is unrecoverable because selection is deterministic and re-picks the same type.
	//
	// The wrong-arch guard lives inside resolveImage and in the nodeclass controller,
	// which are the two places an image and an architecture are actually paired up.
	// Re-checking it here would compare the architecture against itself: on the status
	// path resolveImage reports the architecture the entry was filed under, not one read
	// back from hcloud.
	image, err := cp.resolveImage(ctx, nodeClass, hcloudArch)
	if err != nil {
		return nil, err
	}

	// Launch in the offering's location: the type was ranked by that offering's price, so
	// launching anywhere else can bill a multiple of the price it was chosen on.
	location := offering.Zone()

	// Collect node pool name from NodeClaim labels (may be empty).
	nodePoolName := nodeClaim.Labels[karpv1.NodePoolLabelKey]

	// Resolve userData, preferring the Secret reference over inline userData.
	userData, err := cp.resolveUserData(ctx, nodeClass)
	if err != nil {
		return nil, fmt.Errorf("resolving userData: %w", err)
	}

	// Create the server.
	server, err := cp.instanceProvider.Create(ctx, instance.CreateOpts{
		Name:                   nodeClaim.Name,
		ServerType:             selected.Name,
		Location:               location,
		Image:                  image,
		NetworkID:              nodeClass.Spec.NetworkID,
		FirewallIDs:            nodeClass.Spec.FirewallIDs,
		SSHKeyIDs:              nodeClass.Spec.SSHKeyIDs,
		Labels:                 nodeClass.Spec.Labels,
		UserData:               userData,
		NodeClaim:              nodeClaim.Name,
		NodePool:               nodePoolName,
		PlacementGroupStrategy: nodeClass.Spec.PlacementGroupStrategy,
		EnablePublicIPv4:       nodeClass.Spec.PublicIPv4Enabled(),
		EnablePublicIPv6:       nodeClass.Spec.PublicIPv6Enabled(),
	})
	if err != nil {
		if karpcp.IsInsufficientCapacityError(err) {
			cp.typeProvider.MarkUnavailable(selected.Name, location)
		}
		return nil, fmt.Errorf("creating server: %w", err)
	}

	// Build labels from instance type requirements.
	labels := map[string]string{}
	for key, req := range selected.Requirements {
		if req.Operator() == corev1.NodeSelectorOpIn {
			labels[key] = req.Values()[0]
		}
	}
	// Overlay offering-specific labels (zone, capacity-type) from the offering the server
	// was launched into, so the zone label always matches where the server actually is:
	// karpenter core resolves a node's offering by that label to price it for consolidation.
	for _, req := range offering.Requirements {
		labels[req.Key] = req.Any()
	}
	// Merge existing NodeClaim labels.
	for k, v := range nodeClaim.Labels {
		labels[k] = v
	}

	// Build the hydrated NodeClaim.
	created := nodeClaim.DeepCopy()
	created.Labels = labels
	created.Status.ProviderID = instance.FormatProviderID(server.ID)
	created.Status.Capacity = selected.Capacity
	created.Status.Allocatable = selected.Allocatable()
	if server.Image != nil {
		created.Status.ImageID = strconv.FormatInt(server.Image.ID, 10)
	}

	return created, nil
}

// Delete terminates the server backing the given NodeClaim.
func (cp *CloudProvider) Delete(ctx context.Context, nodeClaim *karpv1.NodeClaim) error {
	return cp.instanceProvider.Delete(ctx, nodeClaim.Status.ProviderID)
}

// Get retrieves the NodeClaim corresponding to the given provider ID.
func (cp *CloudProvider) Get(ctx context.Context, providerID string) (*karpv1.NodeClaim, error) {
	server, err := cp.instanceProvider.Get(ctx, providerID)
	if err != nil {
		return nil, err
	}
	if server == nil {
		return nil, karpcp.NewNodeClaimNotFoundError(fmt.Errorf("server with provider ID %q not found", providerID))
	}
	return serverToNodeClaim(server), nil
}

// List retrieves all NodeClaims managed by this provider.
func (cp *CloudProvider) List(ctx context.Context) ([]*karpv1.NodeClaim, error) {
	servers, err := cp.instanceProvider.List(ctx)
	if err != nil {
		return nil, err
	}
	nodeClaims := make([]*karpv1.NodeClaim, 0, len(servers))
	for _, s := range servers {
		nodeClaims = append(nodeClaims, serverToNodeClaim(s))
	}
	return nodeClaims, nil
}

// GetInstanceTypes returns the available instance types for the given NodePool.
func (cp *CloudProvider) GetInstanceTypes(ctx context.Context, nodePool *karpv1.NodePool) ([]*karpcp.InstanceType, error) {
	if nodePool == nil {
		return cp.typeProvider.List(ctx, nil)
	}

	nodeClass, err := cp.resolveNodeClass(ctx, nodePool.Spec.Template.Spec.NodeClassRef)
	if err != nil {
		return nil, fmt.Errorf("resolving node class for node pool %s: %w", nodePool.Name, err)
	}

	its, err := cp.typeProvider.List(ctx, nodeClass)
	if err != nil {
		return nil, err
	}
	return markImagelessArchesUnavailable(its, nodeClass), nil
}

// markImagelessArchesUnavailable drops availability -- but not membership -- for every
// instance type whose architecture the NodeClass has no image for.
//
// Availability and membership serve different consumers. The scheduler requires an
// available compatible offering, so this stops core scheduling onto an architecture
// Create would refuse; without it core creates a NodeClaim, Create rejects it, core
// deletes it, and the next cycle recreates the identical claim indefinitely with no
// backoff. Drift checks Offerings.HasCompatible with no Available() filter, so keeping
// the offering listed means running nodes of that type are not drifted and replaced.
//
// The offerings returned by List are fresh value-copies (see applyAvailability), so
// clearing Available here cannot reach the provider's cached catalogue.
func markImagelessArchesUnavailable(its []*karpcp.InstanceType, nodeClass *apiv1.HCloudNodeClass) []*karpcp.InstanceType {
	for _, it := range its {
		if hasResolvedImage(nodeClass, hcloudArchFor(it)) {
			continue
		}
		for _, o := range it.Offerings {
			o.Available = false
		}
	}
	return its
}

// IsDrifted determines whether the given NodeClaim has drifted from its desired state.
func (cp *CloudProvider) IsDrifted(ctx context.Context, nodeClaim *karpv1.NodeClaim) (karpcp.DriftReason, error) {
	log := logf.FromContext(ctx)
	nodeClass, err := cp.resolveNodeClass(ctx, nodeClaim.Spec.NodeClassRef)
	if err != nil {
		return "", fmt.Errorf("resolving node class: %w", err)
	}

	server, err := cp.instanceProvider.Get(ctx, nodeClaim.Status.ProviderID)
	if err != nil {
		return "", fmt.Errorf("getting server: %w", err)
	}
	if server == nil {
		return "", nil
	}

	// logDrift emits a structured INFO log, records a Prometheus counter, and
	// returns the reason so callers can write: return logDrift(reason, id), nil
	logDrift := func(reason karpcp.DriftReason, providerID string) karpcp.DriftReason {
		log.Info("drift detected", "reason", string(reason), "providerID", providerID)
		metrics.RecordDrift(string(reason))
		return reason
	}

	// Check image drift: compare the resolved image ID recorded in NodeClaim status against
	// the current server image.
	if nodeClaim.Status.ImageID != "" && server.Image != nil {
		currentImageID := strconv.FormatInt(server.Image.ID, 10)
		if nodeClaim.Status.ImageID != currentImageID {
			return logDrift(DriftImage, nodeClaim.Status.ProviderID), nil
		}
	}

	// Check network drift: ensure the server is attached to the expected network.
	if nodeClass.Spec.NetworkID > 0 {
		attached := false
		for _, pn := range server.PrivateNet {
			if pn.Network != nil && pn.Network.ID == nodeClass.Spec.NetworkID {
				attached = true
				break
			}
		}
		if !attached {
			return logDrift(DriftNetwork, nodeClaim.Status.ProviderID), nil
		}
	}

	// Firewall drift: every NodeClass firewall must be attached to the server.
	// This is a subset check only; firewalls attached beyond the NodeClass spec
	// (e.g. applied out-of-band) are permitted and do not count as drift.
	if len(nodeClass.Spec.FirewallIDs) > 0 {
		attached := make(map[int64]bool, len(server.PublicNet.Firewalls))
		for _, fw := range server.PublicNet.Firewalls {
			if fw == nil {
				continue
			}
			attached[fw.Firewall.ID] = true
		}
		for _, want := range nodeClass.Spec.FirewallIDs {
			if !attached[want] {
				return logDrift(DriftFirewall, nodeClaim.Status.ProviderID), nil
			}
		}
	}

	// Server-type drift: the running server type must match the type recorded on
	// the NodeClaim's instance-type label. An absent label (e.g. a NodeClaim not
	// created by this provider) intentionally skips the check rather than
	// reporting false drift.
	if want := nodeClaim.Labels[corev1.LabelInstanceTypeStable]; want != "" &&
		server.ServerType != nil && server.ServerType.Name != want {
		return logDrift(DriftServerType, nodeClaim.Status.ProviderID), nil
	}

	// Location drift: the server's location must be in the NodeClass Locations
	// list. Guards a nil Location pointer defensively (e.g. mid-provisioning).
	if len(nodeClass.Spec.Locations) > 0 && server.Location != nil {
		serverLocation := server.Location.Name
		inAllowed := false
		for _, loc := range nodeClass.Spec.Locations {
			if loc == serverLocation {
				inAllowed = true
				break
			}
		}
		if !inAllowed {
			return logDrift(DriftLocation, nodeClaim.Status.ProviderID), nil
		}
	}

	// Label drift: every key/value in NodeClass spec.labels must be present and
	// matching on the server. Extra labels on the server (karpenter management,
	// offering labels, etc.) are permitted and do not count as drift. Empty/nil
	// spec labels means nothing is required — skip the check.
	if len(nodeClass.Spec.Labels) > 0 {
		for k, want := range nodeClass.Spec.Labels {
			if got, ok := server.Labels[k]; !ok || got != want {
				return logDrift(DriftLabels, nodeClaim.Status.ProviderID), nil
			}
		}
	}

	// SSH-key and user-data drift are intentionally not checked: Hetzner does not
	// reliably expose applied SSH keys or user-data after create, so a comparison
	// would produce false positives. They are omitted rather than faked.

	return "", nil
}

// resolveUserData returns the userData for a NodeClass, reading it from the
// referenced Secret when UserDataSecretRef is set (takes precedence over inline UserData).
func (cp *CloudProvider) resolveUserData(ctx context.Context, nc *apiv1.HCloudNodeClass) (string, error) {
	ref := nc.Spec.UserDataSecretRef
	if ref == nil {
		return nc.Spec.UserData, nil
	}
	secret := &corev1.Secret{}
	if err := cp.kubeClient.Get(ctx, types.NamespacedName{Namespace: ref.Namespace, Name: ref.Name}, secret); err != nil {
		return "", fmt.Errorf("reading userData secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	data, ok := secret.Data[ref.Key]
	if !ok || len(data) == 0 {
		return "", fmt.Errorf("userData secret %s/%s has no non-empty key %q", ref.Namespace, ref.Name, ref.Key)
	}
	return string(data), nil
}

// resolveNodeClass fetches the HCloudNodeClass referenced by ref.
func (cp *CloudProvider) resolveNodeClass(ctx context.Context, ref *karpv1.NodeClassReference) (*apiv1.HCloudNodeClass, error) {
	if ref == nil {
		return nil, fmt.Errorf("nodeClassRef is nil")
	}
	nodeClass := &apiv1.HCloudNodeClass{}
	if err := cp.kubeClient.Get(ctx, types.NamespacedName{Name: ref.Name}, nodeClass); err != nil {
		return nil, fmt.Errorf("getting HCloudNodeClass %q: %w", ref.Name, err)
	}
	return nodeClass, nil
}

// serverToNodeClaim maps an hcloud.Server to a Karpenter NodeClaim.
func serverToNodeClaim(server *hcloud.Server) *karpv1.NodeClaim {
	nc := &karpv1.NodeClaim{}
	nc.Status.ProviderID = instance.FormatProviderID(server.ID)

	if server.Image != nil {
		nc.Status.ImageID = strconv.FormatInt(server.Image.ID, 10)
	}

	// Build capacity from ServerType if available.
	if server.ServerType != nil {
		st := server.ServerType
		memBytes := int64(float64(st.Memory) * 1024 * 1024 * 1024)
		diskBytes := int64(st.Disk) * 1024 * 1024 * 1024
		nc.Status.Capacity = corev1.ResourceList{
			corev1.ResourceCPU:              *resource.NewMilliQuantity(int64(st.Cores)*1000, resource.DecimalSI),
			corev1.ResourceMemory:           *resource.NewQuantity(memBytes, resource.BinarySI),
			corev1.ResourceEphemeralStorage: *resource.NewQuantity(diskBytes, resource.BinarySI),
			corev1.ResourcePods:             *resource.NewQuantity(110, resource.DecimalSI),
		}
	}

	// Propagate server labels to NodeClaim.
	if len(server.Labels) > 0 {
		nc.Labels = make(map[string]string, len(server.Labels))
		for k, v := range server.Labels {
			nc.Labels[k] = v
		}
	}

	return nc
}
