package instance

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/metrics"
)

// ServerClient is the narrow interface for the hcloud servers API needed by this provider.
type ServerClient interface {
	Create(ctx context.Context, opts hcloud.ServerCreateOpts) (hcloud.ServerCreateResult, *hcloud.Response, error)
	DeleteWithResult(ctx context.Context, server *hcloud.Server) (*hcloud.ServerDeleteResult, *hcloud.Response, error)
	GetByID(ctx context.Context, id int64) (*hcloud.Server, *hcloud.Response, error)
	AllWithOpts(ctx context.Context, opts hcloud.ServerListOpts) ([]*hcloud.Server, error)
}

// PlacementGroupClient is the narrow interface for the hcloud placement groups API.
type PlacementGroupClient interface {
	AllWithOpts(ctx context.Context, opts hcloud.PlacementGroupListOpts) ([]*hcloud.PlacementGroup, error)
	Create(ctx context.Context, opts hcloud.PlacementGroupCreateOpts) (hcloud.PlacementGroupCreateResult, *hcloud.Response, error)
}

// ActionWaiter waits for hcloud actions to complete. *hcloud.ActionClient satisfies it.
type ActionWaiter interface {
	WaitFor(ctx context.Context, actions ...*hcloud.Action) error
}

// Provider wraps hcloud server CRUD operations for Karpenter.
type Provider struct {
	client      ServerClient
	pgClient    PlacementGroupClient
	waiter      ActionWaiter
	clusterName string
}

// NewProvider returns a Provider that does NOT wait for hcloud actions to
// complete after server creation and has no placement-group support. Intended
// for tests; production uses NewProviderWithPlacementGroups.
func NewProvider(client ServerClient, clusterName string) *Provider {
	return &Provider{client: client, clusterName: clusterName}
}

// NewProviderWithWaiter returns a Provider that blocks after server creation
// until all hcloud create actions complete, but has no placement-group support.
// Intended for tests; production uses NewProviderWithPlacementGroups.
func NewProviderWithWaiter(client ServerClient, clusterName string, waiter ActionWaiter) *Provider {
	return &Provider{client: client, waiter: waiter, clusterName: clusterName}
}

// NewProviderWithPlacementGroups returns a Provider that supports placement
// groups and waits for hcloud create actions to complete. This is the
// production constructor.
func NewProviderWithPlacementGroups(client ServerClient, pgClient PlacementGroupClient, clusterName string, waiter ActionWaiter) *Provider {
	return &Provider{client: client, pgClient: pgClient, waiter: waiter, clusterName: clusterName}
}

// CreateOpts contains all parameters needed to create a Hetzner server node.
type CreateOpts struct {
	Name                   string
	ServerType             string
	Location               string
	Image                  *hcloud.Image
	NetworkID              int64
	FirewallIDs            []int64
	SSHKeyIDs              []int64
	Labels                 map[string]string
	UserData               string
	NodeClaim              string
	NodePool               string
	PlacementGroupStrategy string
	EnablePublicIPv4       bool
	EnablePublicIPv6       bool
}

// placementGroupName returns the deterministic placement group name for a node
// pool, scoped to the cluster so two clusters sharing a Hetzner project (and a
// node-pool name) do not collide on the same placement group. When nodePool is
// empty it falls back to the cluster-scoped default.
func placementGroupName(clusterName, nodePool string) string {
	base := "karpenter-" + clusterName
	if nodePool == "" {
		return base
	}
	return base + "-" + nodePool
}

// getOrCreatePlacementGroup finds an existing placement group of type spread
// with the given name, creating it if it does not exist yet. It returns the
// group's ID, or 0 and an error on failure.
func (p *Provider) getOrCreatePlacementGroup(ctx context.Context, name string) (int64, error) {
	// AllWithOpts filters server-side by Name + Type, so a non-empty result is
	// already the group we want.
	existing, err := p.pgClient.AllWithOpts(ctx, hcloud.PlacementGroupListOpts{
		Name: name,
		Type: hcloud.PlacementGroupTypeSpread,
	})
	if err != nil {
		return 0, fmt.Errorf("listing placement groups: %w", err)
	}
	if len(existing) > 0 {
		return existing[0].ID, nil
	}

	result, _, err := p.pgClient.Create(ctx, hcloud.PlacementGroupCreateOpts{
		Name: name,
		Type: hcloud.PlacementGroupTypeSpread,
	})
	if err != nil {
		return 0, fmt.Errorf("creating placement group %q: %w", name, err)
	}
	return result.PlacementGroup.ID, nil
}

// Create provisions a new Hetzner server, merging Karpenter management labels.
func (p *Provider) Create(ctx context.Context, opts CreateOpts) (*hcloud.Server, error) {
	start := time.Now()
	server, err := p.create(ctx, opts)
	result := metrics.ResultSuccess
	if err != nil {
		result = metrics.ResultError
		metrics.RecordServerCreateError(errorCode(err))
	}
	metrics.RecordServerCreate(result, time.Since(start))
	return server, err
}

// errorCode returns the hcloud error code in err's chain, or "other": a bounded
// label value that tells quota (resource_limit_exceeded) from a type being out of
// stock (resource_unavailable).
func errorCode(err error) string {
	var he hcloud.Error
	if errors.As(err, &he) && he.Code != "" {
		return string(he.Code)
	}
	return "other"
}

// create is the internal implementation of Create, instrumented by Create().
func (p *Provider) create(ctx context.Context, opts CreateOpts) (*hcloud.Server, error) {
	log := logf.FromContext(ctx)
	labels := make(map[string]string, len(opts.Labels)+3)
	for k, v := range opts.Labels {
		labels[k] = v
	}
	labels[apiv1.ServerLabelManagedBy] = apiv1.ServerValueManagedBy
	labels[apiv1.ServerLabelCluster] = p.clusterName
	if opts.NodeClaim != "" {
		labels[apiv1.ServerLabelNodeClaim] = opts.NodeClaim
	}
	if opts.NodePool != "" {
		labels[apiv1.ServerLabelNodePool] = opts.NodePool
	}

	// Build networks list.
	var networks []*hcloud.Network
	if opts.NetworkID > 0 {
		networks = []*hcloud.Network{{ID: opts.NetworkID}}
	}

	// Build firewalls list.
	var firewalls []*hcloud.ServerCreateFirewall
	for _, fwID := range opts.FirewallIDs {
		firewalls = append(firewalls, &hcloud.ServerCreateFirewall{
			Firewall: hcloud.Firewall{ID: fwID},
		})
	}

	// Build SSH keys list.
	var sshKeys []*hcloud.SSHKey
	for _, keyID := range opts.SSHKeyIDs {
		sshKeys = append(sshKeys, &hcloud.SSHKey{ID: keyID})
	}

	createOpts := hcloud.ServerCreateOpts{
		Name:       opts.Name,
		ServerType: &hcloud.ServerType{Name: opts.ServerType},
		Image:      opts.Image,
		Location:   &hcloud.Location{Name: opts.Location},
		Networks:   networks,
		Firewalls:  firewalls,
		SSHKeys:    sshKeys,
		Labels:     labels,
		UserData:   opts.UserData,
	}

	createOpts.PublicNet = &hcloud.ServerCreatePublicNet{
		EnableIPv4: opts.EnablePublicIPv4,
		EnableIPv6: opts.EnablePublicIPv6,
	}

	// Apply placement group when strategy is "spread" (or empty, since spread
	// is the kubebuilder default). Strategy "none" intentionally skips this.
	if p.pgClient != nil && opts.PlacementGroupStrategy != "none" {
		pgName := placementGroupName(p.clusterName, opts.NodePool)
		pgID, err := p.getOrCreatePlacementGroup(ctx, pgName)
		if err != nil {
			return nil, fmt.Errorf("resolving placement group: %w", err)
		}
		createOpts.PlacementGroup = &hcloud.PlacementGroup{ID: pgID}
	}

	result, _, err := p.client.Create(ctx, createOpts)
	if err != nil {
		return nil, MapCreateError(err)
	}

	// Wait for the create action and any follow-up actions so we only return a
	// server that is actually being provisioned.
	if p.waiter != nil {
		actions := make([]*hcloud.Action, 0, 1+len(result.NextActions))
		if result.Action != nil {
			actions = append(actions, result.Action)
		}
		actions = append(actions, result.NextActions...)
		if len(actions) > 0 {
			if err := p.waiter.WaitFor(ctx, actions...); err != nil {
				return nil, fmt.Errorf("waiting for server %q create actions: %w", opts.Name, err)
			}
		}
	}
	pgAttached := createOpts.PlacementGroup != nil
	imageID := int64(0)
	if opts.Image != nil {
		imageID = opts.Image.ID
	}
	log.Info("created server",
		"name", opts.Name,
		"serverType", opts.ServerType,
		"location", opts.Location,
		"imageID", imageID,
		"placementGroupAttached", pgAttached,
	)
	return result.Server, nil
}

// Delete removes the server identified by providerID. It returns nil once the
// deletion has been triggered (Hetzner deletes asynchronously), and a
// cloudprovider.NodeClaimNotFoundError once the server no longer exists — the
// signal Karpenter's termination controller uses to finalize the NodeClaim.
func (p *Provider) Delete(ctx context.Context, providerID string) error {
	err := p.delete(ctx, providerID)
	// A NodeClaimNotFoundError means the instance is already terminated, which is
	// the expected steady-state result of a completed deletion — not a failure —
	// so it must not be counted as a server-delete error.
	result := metrics.ResultSuccess
	if err != nil && !karpcp.IsNodeClaimNotFoundError(err) {
		result = metrics.ResultError
	}
	metrics.RecordServerDelete(result)
	return err
}

// delete is the internal implementation of Delete, instrumented by Delete().
func (p *Provider) delete(ctx context.Context, providerID string) error {
	log := logf.FromContext(ctx)
	id, err := ParseProviderID(providerID)
	if err != nil {
		return fmt.Errorf("parsing provider ID %q: %w", providerID, err)
	}

	server, _, err := p.client.GetByID(ctx, id)
	if err != nil {
		if hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
			return karpcp.NewNodeClaimNotFoundError(fmt.Errorf("server %d not found", id))
		}
		return fmt.Errorf("getting server %d: %w", id, err)
	}
	if server == nil {
		// Server already gone. Per the CloudProvider.Delete contract, return a
		// NodeClaimNotFoundError (not nil) once the instance is terminated — it is
		// the signal Karpenter's termination controller uses to remove the NodeClaim
		// finalizer. Returning nil makes it requeue indefinitely and leaks the NodeClaim.
		return karpcp.NewNodeClaimNotFoundError(fmt.Errorf("server %d not found", id))
	}
	// The API token can delete every server in the project, which may hold other
	// clusters and non-Karpenter servers. Only delete what List would return. A plain
	// error keeps the NodeClaim terminating and loud instead of forgetting it.
	if server.Labels[apiv1.ServerLabelManagedBy] != apiv1.ServerValueManagedBy ||
		server.Labels[apiv1.ServerLabelCluster] != p.clusterName {
		return fmt.Errorf("refusing to delete server %d (%s): not labelled %s=%s,%s=%s",
			id, server.Name, apiv1.ServerLabelManagedBy, apiv1.ServerValueManagedBy,
			apiv1.ServerLabelCluster, p.clusterName)
	}

	log.Info("deleting server", "serverID", id)
	_, _, err = p.client.DeleteWithResult(ctx, server)
	if err != nil {
		if hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
			return karpcp.NewNodeClaimNotFoundError(fmt.Errorf("server %d not found", id))
		}
		return fmt.Errorf("deleting server %d: %w", id, err)
	}
	return nil
}

// Get retrieves the server identified by providerID. Returns nil if not found.
func (p *Provider) Get(ctx context.Context, providerID string) (*hcloud.Server, error) {
	id, err := ParseProviderID(providerID)
	if err != nil {
		return nil, fmt.Errorf("parsing provider ID %q: %w", providerID, err)
	}

	server, _, err := p.client.GetByID(ctx, id)
	if err != nil {
		if hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("getting server %d: %w", id, err)
	}
	return server, nil
}

// List returns all servers managed by this Karpenter instance.
func (p *Provider) List(ctx context.Context) ([]*hcloud.Server, error) {
	opts := hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: fmt.Sprintf("%s=%s,%s=%s",
				apiv1.ServerLabelManagedBy, apiv1.ServerValueManagedBy,
				apiv1.ServerLabelCluster, p.clusterName),
		},
	}
	servers, err := p.client.AllWithOpts(ctx, opts)
	if err != nil {
		return nil, fmt.Errorf("listing karpenter servers: %w", err)
	}
	return servers, nil
}

// ParseProviderID parses a Hetzner provider ID of the form "hcloud://<id>" and returns the integer ID.
func ParseProviderID(providerID string) (int64, error) {
	if !strings.HasPrefix(providerID, apiv1.ProviderIDPrefix) {
		return 0, fmt.Errorf("provider ID %q must start with %q", providerID, apiv1.ProviderIDPrefix)
	}
	idStr := strings.TrimPrefix(providerID, apiv1.ProviderIDPrefix)
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid server ID in provider ID %q: %w", providerID, err)
	}
	return id, nil
}

// FormatProviderID formats a Hetzner server ID as a Karpenter provider ID.
func FormatProviderID(serverID int64) string {
	return apiv1.ProviderIDPrefix + strconv.FormatInt(serverID, 10)
}
