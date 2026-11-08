package orphan

import (
	"context"
	"errors"
	"sort"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instance"
)

var now = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)

const grace = 30 * time.Minute

type fakeInstances struct {
	servers []*hcloud.Server
	listErr error
	deleted []string
}

func (f *fakeInstances) List(context.Context) ([]*hcloud.Server, error) { return f.servers, f.listErr }
func (f *fakeInstances) Delete(_ context.Context, providerID string) error {
	f.deleted = append(f.deleted, providerID)
	return nil
}

func server(id int64, nodeClaim string, age time.Duration) *hcloud.Server {
	labels := map[string]string{apiv1.ServerLabelManagedBy: apiv1.ServerValueManagedBy, apiv1.ServerLabelCluster: "c"}
	if nodeClaim != "" {
		labels[apiv1.ServerLabelNodeClaim] = nodeClaim
	}
	return &hcloud.Server{ID: id, Name: nodeClaim, Labels: labels, Created: now.Add(-age), Status: hcloud.ServerStatusRunning}
}

func newController(t *testing.T, inst *fakeInstances, objs ...client.Object) *Controller {
	t.Helper()
	// Importing karpv1 registers NodeClaim into client-go's scheme.
	kube := fake.NewClientBuilder().WithScheme(clientgoscheme.Scheme).WithObjects(objs...).Build()
	return NewController(kube, inst, grace, func() time.Time { return now })
}

// TestReconcile_ReclaimsOnlyOrphans covers every branch of the ownership test in
// one sweep: only a server that is old enough, labelled for a NodeClaim, whose
// NodeClaim is gone and which no Node references, is deleted.
func TestReconcile_ReclaimsOnlyOrphans(t *testing.T) {
	inst := &fakeInstances{servers: []*hcloud.Server{
		server(1, "orphan", 2*time.Hour),     // deleted
		server(2, "young", 5*time.Minute),    // kept: within the grace period
		server(3, "", 2*time.Hour),           // kept: no nodeclaim label
		server(4, "live-claim", 2*time.Hour), // kept: NodeClaim exists
		server(5, "registered", 2*time.Hour), // kept: a Node references it
		server(6, "orphan-at-grace", grace),  // deleted: exactly at the grace period
	}}
	deleting := server(7, "being-deleted", 2*time.Hour)
	deleting.Status = hcloud.ServerStatusDeleting
	inst.servers = append(inst.servers, deleting) // kept: already going away

	c := newController(t, inst,
		&karpv1.NodeClaim{ObjectMeta: metav1.ObjectMeta{Name: "live-claim"}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "n5"}, Spec: corev1.NodeSpec{ProviderID: instance.FormatProviderID(5)}},
	)
	res, err := c.Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(inst.deleted)
	want := []string{instance.FormatProviderID(1), instance.FormatProviderID(6)}
	if len(inst.deleted) != len(want) || inst.deleted[0] != want[0] || inst.deleted[1] != want[1] {
		t.Errorf("deleted %v, want %v", inst.deleted, want)
	}
	if res.RequeueAfter != interval {
		t.Errorf("requeue after %v, want %v", res.RequeueAfter, interval)
	}
}

// TestReconcile_ListFailureDeletesNothing makes sure an unreadable server list is
// an error, not an empty fleet.
func TestReconcile_ListFailureDeletesNothing(t *testing.T) {
	inst := &fakeInstances{listErr: errors.New("rate limited")}
	c := newController(t, inst)
	if _, err := c.Reconcile(context.Background()); err == nil {
		t.Fatal("expected the list error")
	}
	if len(inst.deleted) != 0 {
		t.Errorf("deleted %v after a failed list", inst.deleted)
	}
}

// TestReconcile_NothingOldEnoughSkipsKubeReads keeps the common case cheap: with
// no candidate, NodeClaims and Nodes are not listed at all.
func TestReconcile_NothingOldEnoughSkipsKubeReads(t *testing.T) {
	inst := &fakeInstances{servers: []*hcloud.Server{server(1, "young", time.Minute)}}
	// A client without the karpenter types registered fails any NodeClaim list,
	// so this passes only if the sweep never lists them.
	scheme := runtime.NewScheme()
	kube := fake.NewClientBuilder().WithScheme(scheme).Build()
	c := NewController(kube, inst, grace, func() time.Time { return now })
	if _, err := c.Reconcile(context.Background()); err != nil {
		t.Fatalf("sweep with no candidates touched the API server: %v", err)
	}
	if len(inst.deleted) != 0 {
		t.Errorf("deleted %v", inst.deleted)
	}
}
