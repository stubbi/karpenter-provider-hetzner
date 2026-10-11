package nodeclass

import (
	"context"
	"errors"
	"fmt"

	controllerruntime "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
)

// HashController keeps the NodeClass hash annotations current and re-stamps
// NodeClaims whose recorded hash predates the current hashing scheme.
//
// Drift compares a NodeClaim's recorded hash with its NodeClass's current spec,
// and deliberately ignores NodeClaims with no hash or a hash of another version
// (see IsDrifted), so upgrading never replaces nodes by itself. This controller
// is what makes the NEXT edit visible for those NodeClaims: it records the
// current hash on them, so an edit after that point compares unequal. The
// annotation on the NodeClass itself is informational, so operators can see
// which hash new nodes are launched with.
type HashController struct {
	kubeClient client.Client
}

func NewHashController(kubeClient client.Client) *HashController {
	return &HashController{kubeClient: kubeClient}
}

func (c *HashController) Name() string { return "nodeclass.hash" }

func (c *HashController) Reconcile(ctx context.Context, nc *apiv1.HCloudNodeClass) (reconcile.Result, error) {
	hash := nc.Hash()
	if nc.Annotations[apiv1.AnnotationNodeClassHash] != hash ||
		nc.Annotations[apiv1.AnnotationNodeClassHashVersion] != apiv1.NodeClassHashVersion {
		patch := client.MergeFrom(nc.DeepCopy())
		if nc.Annotations == nil {
			nc.Annotations = map[string]string{}
		}
		nc.Annotations[apiv1.AnnotationNodeClassHash] = hash
		nc.Annotations[apiv1.AnnotationNodeClassHashVersion] = apiv1.NodeClassHashVersion
		if err := c.kubeClient.Patch(ctx, nc, patch); err != nil {
			return reconcile.Result{}, fmt.Errorf("annotating HCloudNodeClass %q with its hash: %w", nc.Name, err)
		}
	}

	claims := &karpv1.NodeClaimList{}
	if err := c.kubeClient.List(ctx, claims); err != nil {
		return reconcile.Result{}, fmt.Errorf("listing NodeClaims: %w", err)
	}
	var errs []error
	for i := range claims.Items {
		claim := &claims.Items[i]
		ref := claim.Spec.NodeClassRef
		if ref == nil || ref.Group != apiv1.Group || ref.Name != nc.Name {
			continue
		}
		// Only NodeClaims on another hashing scheme (none at all included) are
		// re-stamped. A NodeClaim on the current scheme carries the hash it was
		// launched with, and overwriting it would erase exactly the difference
		// drift exists to detect.
		if claim.Annotations[apiv1.AnnotationNodeClassHashVersion] == apiv1.NodeClassHashVersion {
			continue
		}
		patch := client.MergeFrom(claim.DeepCopy())
		if claim.Annotations == nil {
			claim.Annotations = map[string]string{}
		}
		claim.Annotations[apiv1.AnnotationNodeClassHash] = hash
		claim.Annotations[apiv1.AnnotationNodeClassHashVersion] = apiv1.NodeClassHashVersion
		if err := c.kubeClient.Patch(ctx, claim, patch); client.IgnoreNotFound(err) != nil {
			errs = append(errs, fmt.Errorf("stamping NodeClaim %q with the node class hash: %w", claim.Name, err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return reconcile.Result{}, err
	}
	// Resync picks up NodeClaims launched by an older replica during a rolling
	// upgrade, after this pass already ran.
	return reconcile.Result{RequeueAfter: resyncInterval}, nil
}

// Register wires the controller into the manager.
func (c *HashController) Register(_ context.Context, m manager.Manager) error {
	return controllerruntime.NewControllerManagedBy(m).
		For(&apiv1.HCloudNodeClass{}).
		Named(c.Name()).
		Complete(reconcile.AsReconciler(m.GetClient(), c))
}
