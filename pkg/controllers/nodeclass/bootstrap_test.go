package nodeclass

import (
	"context"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/imagefamily"
)

func TestReconcile_BootstrapReady(t *testing.T) {
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	token := func(ns string) *corev1.Secret {
		return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: "join"}, Data: map[string][]byte{"token": []byte("t")}}
	}
	withBootstrap := func(userData string) *apiv1.HCloudNodeClass {
		nc := newNodeClass()
		nc.Spec.Bootstrap = &apiv1.BootstrapConfiguration{Distribution: "k3s", Server: "https://10.0.0.2:6443",
			TokenSecretRef: apiv1.BootstrapTokenSecretReference{Name: "join", Key: "token"}}
		nc.Spec.UserData = userData
		return nc
	}
	for name, tc := range map[string]struct {
		nc         *apiv1.HCloudNodeClass
		objects    []client.Object
		wantStatus metav1.ConditionStatus
		wantReason string
	}{
		"no bootstrap":                    {newNodeClass(), nil, metav1.ConditionTrue, ""},
		"renderable":                      {withBootstrap("#cloud-config\nruncmd: [echo hi]\n"), []client.Object{token("karpenter")}, metav1.ConditionTrue, ""},
		"token secret missing":            {withBootstrap(""), nil, metav1.ConditionFalse, "BootstrapTokenUnavailable"},
		"token in another namespace only": {withBootstrap(""), []client.Object{token("default")}, metav1.ConditionFalse, "BootstrapTokenUnavailable"},
		"extension collides":              {withBootstrap("#cloud-config\nruncmd: replaced\n"), []client.Object{token("karpenter")}, metav1.ConditionFalse, "BootstrapInvalid"},
	} {
		t.Run(name, func(t *testing.T) {
			kube := fake.NewClientBuilder().WithScheme(scheme.Scheme).
				WithObjects(append(tc.objects, tc.nc)...).WithStatusSubresource(tc.nc).Build()
			img := imagefamily.NewProvider(fakeImages{img: &hcloud.Image{ID: 42, Description: "Ubuntu 24.04"}})
			c := NewController(kube, fakeNetworks{net: validNetwork()}, validLocations(), fakeFirewalls{}, fakeSSHKeys{}, img).
				WithControllerNamespace("karpenter")
			if _, err := c.Reconcile(context.Background(), tc.nc.DeepCopy()); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			got := &apiv1.HCloudNodeClass{}
			if err := kube.Get(context.Background(), client.ObjectKeyFromObject(tc.nc), got); err != nil {
				t.Fatal(err)
			}
			cond := got.StatusConditions().Get(apiv1.ConditionTypeBootstrapReady)
			if cond == nil || cond.Status != tc.wantStatus || (tc.wantReason != "" && cond.Reason != tc.wantReason) {
				t.Errorf("BootstrapReady = %+v, want %s/%s", cond, tc.wantStatus, tc.wantReason)
			}
			if tc.wantStatus == metav1.ConditionFalse && got.StatusConditions().Root().IsTrue() {
				t.Error("Ready is true while the bootstrap cannot be rendered")
			}
		})
	}
}
