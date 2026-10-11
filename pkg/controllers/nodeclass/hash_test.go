package nodeclass

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpv1 "sigs.k8s.io/karpenter/pkg/apis/v1"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
)

func claimFor(name, nodeClass string, annotations map[string]string) *karpv1.NodeClaim {
	return &karpv1.NodeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: annotations},
		Spec: karpv1.NodeClaimSpec{
			NodeClassRef: &karpv1.NodeClassReference{Group: apiv1.Group, Kind: "HCloudNodeClass", Name: nodeClass},
		},
	}
}

func getClaim(t *testing.T, kube client.Client, name string) *karpv1.NodeClaim {
	t.Helper()
	c := &karpv1.NodeClaim{}
	if err := kube.Get(context.Background(), types.NamespacedName{Name: name}, c); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHashController(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	nc := &apiv1.HCloudNodeClass{
		ObjectMeta: metav1.ObjectMeta{Name: "default"},
		Spec:       apiv1.HCloudNodeClassSpec{Locations: []string{"nbg1"}, NetworkID: 1, UserData: "#cloud-config"},
	}
	current := nc.Hash()
	launchedWith := map[string]string{
		apiv1.AnnotationNodeClassHash:        "launched-with-an-older-spec",
		apiv1.AnnotationNodeClassHashVersion: apiv1.NodeClassHashVersion,
	}
	kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
		nc,
		claimFor("pre-upgrade", "default", nil),
		claimFor("old-scheme", "default", map[string]string{
			apiv1.AnnotationNodeClassHash: "x", apiv1.AnnotationNodeClassHashVersion: "v0"}),
		claimFor("current-scheme", "default", launchedWith),
		claimFor("other-class", "other", nil),
	).Build()

	stored := &apiv1.HCloudNodeClass{}
	if err := kube.Get(context.Background(), types.NamespacedName{Name: "default"}, stored); err != nil {
		t.Fatal(err)
	}
	if _, err := NewHashController(kube).Reconcile(context.Background(), stored); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}

	if err := kube.Get(context.Background(), types.NamespacedName{Name: "default"}, stored); err != nil {
		t.Fatal(err)
	}
	if got := stored.Annotations[apiv1.AnnotationNodeClassHash]; got != current {
		t.Errorf("NodeClass hash annotation %q, want %q", got, current)
	}
	if got := stored.Annotations[apiv1.AnnotationNodeClassHashVersion]; got != apiv1.NodeClassHashVersion {
		t.Errorf("NodeClass hash version %q, want %q", got, apiv1.NodeClassHashVersion)
	}

	// NodeClaims with no hash, or another scheme's, take the current hash so the next
	// edit is detected without the upgrade itself rolling them.
	for _, name := range []string{"pre-upgrade", "old-scheme"} {
		c := getClaim(t, kube, name)
		if c.Annotations[apiv1.AnnotationNodeClassHash] != current ||
			c.Annotations[apiv1.AnnotationNodeClassHashVersion] != apiv1.NodeClassHashVersion {
			t.Errorf("%s: annotations %v, want the current hash and version", name, c.Annotations)
		}
	}
	// A NodeClaim already on the current scheme keeps the hash it was launched with:
	// overwriting it would erase the very edit drift exists to detect.
	if got := getClaim(t, kube, "current-scheme").Annotations[apiv1.AnnotationNodeClassHash]; got != "launched-with-an-older-spec" {
		t.Errorf("current-scheme NodeClaim was re-stamped to %q", got)
	}
	if got := getClaim(t, kube, "other-class").Annotations; len(got) != 0 {
		t.Errorf("another NodeClass's NodeClaim was stamped: %v", got)
	}
}
