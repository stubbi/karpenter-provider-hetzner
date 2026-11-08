package instance

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/deadline"
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
	GetByID(ctx context.Context, id int64) (*hcloud.PlacementGroup, *hcloud.Response, error)
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

	// pgMu guards pgPools and serializes placement-group allocation. Without it a
	// scale-up burst ran one list+create pair per concurrent create on the same
	// name, all saw an empty list, and all created a group (#69); with it the
	// first create resolves the pool's groups and every other one reads the
	// cache, which also drops a list call from every create (#70).
	pgMu    sync.Mutex
	pgPools map[string]*pgPool
	nowFn   func() time.Time

	budgets deadline.Budgets
}

// NewProvider returns a Provider that does NOT wait for hcloud actions to
// complete after server creation and has no placement-group support. Intended
// for tests; production uses NewProviderWithPlacementGroups.
func NewProvider(client ServerClient, clusterName string) *Provider {
	return &Provider{client: client, clusterName: clusterName, budgets: deadline.Defaults()}
}

// NewProviderWithWaiter returns a Provider that blocks after server creation
// until all hcloud create actions complete, but has no placement-group support.
// Intended for tests; production uses NewProviderWithPlacementGroups.
func NewProviderWithWaiter(client ServerClient, clusterName string, waiter ActionWaiter) *Provider {
	return &Provider{client: client, waiter: waiter, clusterName: clusterName, budgets: deadline.Defaults()}
}

// NewProviderWithPlacementGroups returns a Provider that supports placement
// groups and waits for hcloud create actions to complete. This is the
// production constructor.
func NewProviderWithPlacementGroups(client ServerClient, pgClient PlacementGroupClient, clusterName string, waiter ActionWaiter) *Provider {
	return &Provider{client: client, pgClient: pgClient, waiter: waiter, clusterName: clusterName, budgets: deadline.Defaults()}
}

// WithBudgets replaces the per-class context budgets (see package deadline).
// Production keeps the defaults; tests shrink them.
func (p *Provider) WithBudgets(b deadline.Budgets) *Provider {
	p.budgets = b
	return p
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
	// PlacementGroupIDs, when set, are the operator-managed spread groups to place
	// servers in instead of the provider-managed ones.
	PlacementGroupIDs []int64
	EnablePublicIPv4  bool
	EnablePublicIPv6  bool
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

// findForNodeClaim returns the server this cluster already created for a
// NodeClaim, or nil. Every server carries the karpenter.sh/nodeclaim label, so a
// create that is retried after hcloud accepted it -- the action wait failed, the
// context was cancelled by a rolling update or a leader handover, the process
// died -- finds the first attempt's server here instead of creating a second one
// and orphaning the first (#61).
func (p *Provider) findForNodeClaim(ctx context.Context, nodeClaim string) (*hcloud.Server, error) {
	servers, err := p.client.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{
			LabelSelector: fmt.Sprintf("%s=%s,%s=%s,%s=%s",
				apiv1.ServerLabelManagedBy, apiv1.ServerValueManagedBy,
				apiv1.ServerLabelCluster, p.clusterName,
				apiv1.ServerLabelNodeClaim, nodeClaim),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("looking up existing server for nodeclaim %q: %w", nodeClaim, err)
	}
	for _, s := range servers {
		// A server already being deleted cannot be adopted; creating a fresh one
		// is right, and its name is free again once the delete completes.
		if s.Status == hcloud.ServerStatusDeleting {
			continue
		}
		return s, nil
	}
	return nil, nil
}

// create is the internal implementation of Create, instrumented by Create().
func (p *Provider) create(ctx context.Context, opts CreateOpts) (*hcloud.Server, error) {
	log := logf.FromContext(ctx)
	// Everything up to hcloud accepting the request (the adoption lookup,
	// placement-group resolution and servers.Create) runs under the submission
	// budget. The boot wait below gets its own budget derived from ctx, not from
	// submitCtx: once the server exists, the submission deadline must not be able
	// to cancel the wait and orphan it (#61), while a cancellation of ctx itself
	// still ends the wait.
	submitCtx, cancelSubmit := context.WithTimeout(ctx, p.budgets.CreateSubmit)
	defer cancelSubmit()
	if opts.NodeClaim != "" {
		existing, err := p.findForNodeClaim(submitCtx, opts.NodeClaim)
		if err != nil {
			return nil, err
		}
		if existing != nil {
			log.Info("adopting server already created for this nodeclaim",
				"name", opts.Name, "serverID", existing.ID, "nodeClaim", opts.NodeClaim)
			return existing, nil
		}
	}
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

	// Apply a placement group when strategy is "spread" (or empty, since spread
	// is the kubebuilder default), or when the NodeClass names its own groups.
	// Strategy "none" without explicit groups intentionally skips this.
	usePG := p.pgClient != nil && (len(opts.PlacementGroupIDs) > 0 || opts.PlacementGroupStrategy != "none")
	excluded := map[int64]bool{}
	var result hcloud.ServerCreateResult
	for attempt := 0; ; attempt++ {
		var pgID int64
		var pgKey string
		if usePG {
			var err error
			pgID, pgKey, err = p.acquirePlacementGroup(submitCtx, opts, excluded)
			if err != nil {
				return nil, fmt.Errorf("resolving placement group: %w", err)
			}
			createOpts.PlacementGroup = &hcloud.PlacementGroup{ID: pgID}
		}

		var err error
		result, _, err = p.client.Create(submitCtx, createOpts)
		if err == nil {
			break
		}
		if usePG {
			switch {
			case hcloud.IsError(err, hcloud.ErrorCodePlacementError):
				// The group is full (or cannot take this server). Counts are
				// checked before every create, but concurrent creates race past
				// that check, so overflow has to be handled here too: mark the
				// group full and try once more with another one.
				p.markPlacementGroupFull(pgKey, pgID)
				if attempt == 0 {
					excluded[pgID] = true
					log.Info("placement group refused the server; retrying with another group",
						"name", opts.Name, "placementGroupID", pgID)
					continue
				}
			case hcloud.IsError(err, hcloud.ErrorCodeNotFound):
				// Most likely a cached group was deleted out of band; re-read the
				// pool on the next attempt.
				p.forgetPlacementGroups(pgKey)
			default:
				p.releasePlacementGroup(pgKey, pgID)
			}
		}
		// A duplicate name means an earlier attempt for this NodeClaim got as far
		// as hcloud, e.g. its create raced this one. Adopt that server rather than
		// failing the launch and leaving it behind.
		if opts.NodeClaim != "" && hcloud.IsError(err, hcloud.ErrorCodeUniquenessError) {
			existing, lookupErr := p.findForNodeClaim(submitCtx, opts.NodeClaim)
			if lookupErr == nil && existing != nil {
				log.Info("adopting server after a duplicate-name create",
					"name", opts.Name, "serverID", existing.ID, "nodeClaim", opts.NodeClaim)
				return existing, nil
			}
		}
		return nil, MapCreateError(err, opts.ServerType, opts.Location)
	}
	cancelSubmit()

	// Wait for the create action and any follow-up actions so we only return a
	// server that is actually being provisioned.
	if p.waiter != nil {
		actions := make([]*hcloud.Action, 0, 1+len(result.NextActions))
		if result.Action != nil {
			actions = append(actions, result.Action)
		}
		actions = append(actions, result.NextActions...)
		if len(actions) > 0 {
			waitCtx, cancelWait := context.WithTimeout(ctx, p.budgets.BootWait)
			defer cancelWait()
			if err := p.waiter.WaitFor(waitCtx, actions...); err != nil {
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
		metrics.RecordServerDeleteError(errorCode(err))
	}
	metrics.RecordServerDelete(result)
	return err
}

// delete is the internal implementation of Delete, instrumented by Delete().
func (p *Provider) delete(ctx context.Context, providerID string) error {
	ctx, cancel := context.WithTimeout(ctx, p.budgets.Delete)
	defer cancel()
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
		if hcloud.IsError(err, hcloud.ErrorCodeProtected) {
			// Delete protection was enabled out of band. Retrying cannot clear it,
			// so say exactly what to do: the NodeClaim stays terminating (and its
			// finalizer in place) until someone removes the protection.
			return fmt.Errorf("deleting server %d (%s): delete protection is enabled on the server; "+
				"disable it in the Hetzner console or with `hcloud server disable-protection %d delete` "+
				"so the NodeClaim can finalize: %w", id, server.Name, id, err)
		}
		return fmt.Errorf("deleting server %d: %w", id, err)
	}
	return nil
}

// Get retrieves the server identified by providerID. Returns nil if not found.
func (p *Provider) Get(ctx context.Context, providerID string) (*hcloud.Server, error) {
	ctx, cancel := context.WithTimeout(ctx, p.budgets.Read)
	defer cancel()
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
	ctx, cancel := context.WithTimeout(ctx, p.budgets.Read)
	defer cancel()
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
