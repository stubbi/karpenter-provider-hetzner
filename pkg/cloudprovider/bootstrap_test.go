package cloudprovider_test

import (
	"context"
	"strings"
	"testing"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	karpcp "sigs.k8s.io/karpenter/pkg/cloudprovider"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/cloudprovider"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/imagefamily"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instance"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instancetype"
)

func bootstrapNodeClass() *apiv1.HCloudNodeClass {
	nc := baselineNodeClass()
	nc.Spec.Bootstrap = &apiv1.BootstrapConfiguration{
		Distribution:   "k3s",
		Server:         "https://10.0.0.2:6443",
		TokenSecretRef: apiv1.BootstrapTokenSecretReference{Name: "join", Key: "token"},
	}
	nc.Spec.UserData = "#cloud-config\nruncmd: [echo user]\n"
	return nc
}

// buildBootstrapCP wires a CloudProvider running in namespace "karpenter" with the
// given objects (the NodeClass plus any Secrets).
func buildBootstrapCP(t *testing.T, nc *apiv1.HCloudNodeClass, secrets ...*corev1.Secret) (*cloudprovider.CloudProvider, *fakeServerClient) {
	t.Helper()
	_ = apiv1.SchemeBuilder.AddToScheme(scheme.Scheme)
	b := fake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(nc)
	for _, s := range secrets {
		b = b.WithObjects(s)
	}
	fsc := &fakeServerClient{servers: map[int64]*hcloud.Server{}}
	cp := cloudprovider.NewCloudProvider(b.Build(),
		instance.NewProvider(fsc, "test-cluster"),
		instancetype.NewProvider(&fakeServerTypeClient{types: []*hcloud.ServerType{cx22Type()}}),
		imagefamily.NewProvider(&fakeImageClient{images: []*hcloud.Image{{ID: 42, Description: "Ubuntu 24.04", Architecture: hcloud.ArchitectureX86}}}),
	).WithControllerNamespace("karpenter")
	return cp, fsc
}

func tokenSecret(namespace string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: "join"},
		Data:       map[string][]byte{"token": []byte("K10agent::secret")},
	}
}

func TestCreate_BootstrapRendersJoinConfig(t *testing.T) {
	cp, fsc := buildBootstrapCP(t, bootstrapNodeClass(), tokenSecret("karpenter"))
	if _, err := cp.Create(context.Background(), createNodeClaim()); err != nil {
		t.Fatalf("Create: %v", err)
	}
	ud := fsc.lastUserData()
	for _, want := range []string{
		"#cloud-config",
		"server: https://10.0.0.2:6443",
		"token: K10agent::secret",
		"karpenter.sh/unregistered=:NoExecute",
		"get.k3s.io",
		"echo user",
	} {
		if !strings.Contains(ud, want) {
			t.Errorf("rendered userData lacks %q:\n%s", want, ud)
		}
	}
	if strings.Index(ud, "get.k3s.io") > strings.Index(ud, "echo user") {
		t.Error("the user's runcmd runs before the provider's install command")
	}
}

// The token is read from the provider's own namespace only: a Secret with the same
// name elsewhere does not count. A missing token is a NodeClass problem no retry
// fixes, so it must not park the NodeClaim in an endless retry.
func TestCreate_BootstrapTokenOnlyFromOwnNamespace(t *testing.T) {
	cp, fsc := buildBootstrapCP(t, bootstrapNodeClass(), tokenSecret("default"))
	_, err := cp.Create(context.Background(), createNodeClaim())
	if !karpcp.IsNodeClassNotReadyError(err) {
		t.Fatalf("want NodeClassNotReadyError, got %v", err)
	}
	if len(fsc.servers) != 0 {
		t.Error("a server was created without a join token")
	}
}

func TestCreate_BootstrapInvalidExtensionIsTerminal(t *testing.T) {
	nc := bootstrapNodeClass()
	nc.Spec.UserData = "#cloud-config\nwrite_files:\n- {path: /etc/rancher/k3s/config.yaml, content: x}\n"
	cp, fsc := buildBootstrapCP(t, nc, tokenSecret("karpenter"))
	_, err := cp.Create(context.Background(), createNodeClaim())
	if !karpcp.IsNodeClassNotReadyError(err) {
		t.Fatalf("want NodeClassNotReadyError, got %v", err)
	}
	if len(fsc.servers) != 0 {
		t.Error("a server was created from an unrenderable bootstrap")
	}
}

// Without spec.bootstrap userData is passed through byte for byte, as before.
func TestCreate_NoBootstrapPassesUserDataThrough(t *testing.T) {
	nc := baselineNodeClass()
	nc.Spec.UserData = "#!/bin/sh\necho untouched\n"
	cp, fsc := buildBootstrapCP(t, nc)
	if _, err := cp.Create(context.Background(), createNodeClaim()); err != nil {
		t.Fatal(err)
	}
	if got := fsc.lastUserData(); got != nc.Spec.UserData {
		t.Errorf("userData changed without spec.bootstrap: %q", got)
	}
}
