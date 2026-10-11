// Package orphan reclaims Hetzner servers this cluster created for a NodeClaim
// that no longer exists and that never became a Node.
//
// Core cannot do this. Its garbage collector only removes NodeClaims whose
// instance vanished; the reverse direction -- an instance whose NodeClaim
// vanished before a provider ID was recorded -- has no owner upstream. Create
// adopts a server on retry (see instance.Provider.findForNodeClaim), which
// closes the leak whenever the NodeClaim survives; this controller handles the
// rest, e.g. a NodeClaim deleted by core's registration timeout while its
// server was still booting.
package orphan

import (
	"context"
	"fmt"
	"time"

	"github.com/awslabs/operatorpkg/reconciler"
	"github.com/awslabs/operatorpkg/singleton"
	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/metrics"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instance"
)

// interval is how often managed servers are swept. One paginated list call per
// sweep; orphans are rare and the grace period is long, so there is no value in
// sweeping often.
const interval = 5 * time.Minute

// Instances is the subset of instance.Provider the controller needs. List is
// scoped to this cluster's managed servers; Delete refuses anything else.
type Instances interface {
	List(ctx context.Context) ([]*hcloud.Server, error)
	Delete(ctx context.Context, providerID string) error
}

// Controller deletes orphaned servers.
type Controller struct {
	kubeClient  client.Client
	instances   Instances
	gracePeriod time.Duration
	now         func() time.Time
}

// NewController returns an orphan controller. gracePeriod is how old a server
// must be before it is considered: it has to outlast a slow boot plus core's
// registration timeout, so a server whose NodeClaim is merely being created is
// never touched.
func NewController(kubeClient client.Client, instances Instances, gracePeriod time.Duration, now func() time.Time) *Controller {
	return &Controller{kubeClient: kubeClient, instances: instances, gracePeriod: gracePeriod, now: now}
}

// Name is the controller name.
func (c *Controller) Name() string { return "instance.orphan" }

// Reconcile runs one sweep.
//
// A server is reclaimed only when ALL of these hold:
//   - it carries this cluster's managed-by and cluster labels (List and Delete
//     both enforce that);
//   - it carries a karpenter.sh/nodeclaim label: servers without one were not
//     created for a NodeClaim and are never touched;
//   - it is older than the grace period;
//   - no NodeClaim of that name exists;
//   - no Node references its provider ID.
//
// Any read failure aborts the sweep without deleting anything.
func (c *Controller) Reconcile(ctx context.Context) (reconciler.Result, error) {
	log := logf.FromContext(ctx).WithName(c.Name())

	servers, err := c.instances.List(ctx)
	if err != nil {
		return reconciler.Result{}, fmt.Errorf("listing servers: %w", err)
	}
	now := c.now()
	var candidates []*hcloud.Server
	for _, s := range servers {
		if s.Labels[apiv1.ServerLabelNodeClaim] == "" || s.Status == hcloud.ServerStatusDeleting {
			continue
		}
		if s.Created.IsZero() || now.Sub(s.Created) < c.gracePeriod {
			continue
		}
		candidates = append(candidates, s)
	}
	if len(candidates) == 0 {
		return reconciler.Result{RequeueAfter: interval}, nil
	}

	nodeClaims := &karpv1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, nodeClaims); err != nil {
		return reconciler.Result{}, fmt.Errorf("listing nodeclaims: %w", err)
	}
	claimed := make(map[string]bool, len(nodeClaims.Items))
	for _, nc := range nodeClaims.Items {
		claimed[nc.Name] = true
	}
	nodes := &corev1.NodeList{}
	if err := c.kubeClient.List(ctx, nodes); err != nil {
		return reconciler.Result{}, fmt.Errorf("listing nodes: %w", err)
	}
	registered := make(map[string]bool, len(nodes.Items))
	for _, n := range nodes.Items {
		if n.Spec.ProviderID != "" {
			registered[n.Spec.ProviderID] = true
		}
	}

	for _, s := range candidates {
		nodeClaim := s.Labels[apiv1.ServerLabelNodeClaim]
		providerID := instance.FormatProviderID(s.ID)
		if claimed[nodeClaim] || registered[providerID] {
			continue
		}
		log.Info("reclaiming orphaned server: its nodeclaim no longer exists and it never became a node",
			"serverID", s.ID, "name", s.Name, "nodeClaim", nodeClaim, "age", now.Sub(s.Created).Round(time.Second).String())
		if err := c.instances.Delete(ctx, providerID); err != nil && !karpcp.IsNodeClaimNotFoundError(err) {
			log.Error(err, "reclaiming orphaned server", "serverID", s.ID)
			continue
		}
		metrics.RecordOrphanReclaimed()
	}
	return reconciler.Result{RequeueAfter: interval}, nil
}

// Register wires the controller into the manager as a singleton loop.
func (c *Controller) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		Named(c.Name()).
		WatchesRawSource(singleton.Source()).
		Complete(singleton.AsReconciler(c))
}
