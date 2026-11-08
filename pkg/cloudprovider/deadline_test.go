package cloudprovider_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/cloudprovider"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/deadline"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/imagefamily"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instance"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instancetype"
)

// hungServerTypeClient blocks until the caller's context is done, recording
// whether it carried a deadline: a catalogue request that never completes.
type hungServerTypeClient struct{ sawDeadline bool }

func (h *hungServerTypeClient) All(ctx context.Context) ([]*hcloud.ServerType, error) {
	_, h.sawDeadline = ctx.Deadline()
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestBudgets_HungCatalogueEndsCreateAndGetInstanceTypes(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := baselineNodeClass()
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(nc).Build()
	stc := &hungServerTypeClient{}
	fsc := &fakeServerClient{servers: map[int64]*hcloud.Server{}}
	cp := cloudprovider.NewCloudProvider(kube,
		instance.NewProvider(fsc, "test-cluster"),
		instancetype.NewProvider(stc),
		imagefamily.NewProvider(&fakeImageClient{})).
		WithBudgets(deadline.Budgets{Read: 50 * time.Millisecond, Delete: 50 * time.Millisecond,
			CreateSubmit: 50 * time.Millisecond, BootWait: 50 * time.Millisecond})

	for name, fn := range map[string]func() error{
		"create": func() error { _, err := cp.Create(context.Background(), createNodeClaim()); return err },
		"get instance types": func() error {
			np := &karpv1.NodePool{}
			np.Spec.Template.Spec.NodeClassRef = &karpv1.NodeClassReference{Name: nc.Name, Group: apiv1.Group, Kind: "HCloudNodeClass"}
			_, err := cp.GetInstanceTypes(context.Background(), np)
			return err
		},
	} {
		t.Run(name, func(t *testing.T) {
			stc.sawDeadline = false
			done := make(chan error, 1)
			go func() { done <- fn() }()
			select {
			case err := <-done:
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Errorf("err = %v, want a deadline error", err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("never returned: the hung catalogue call has no deadline")
			}
			if !stc.sawDeadline {
				t.Error("catalogue call ran without a deadline")
			}
			if len(fsc.servers) != 0 {
				t.Error("a server was created although preparation timed out")
			}
		})
	}
}
