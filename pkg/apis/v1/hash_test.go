package v1

import (
	"testing"
)

func hashOf(mutate func(*HCloudNodeClassSpec)) string {
	nc := &HCloudNodeClass{Spec: HCloudNodeClassSpec{
		Locations:     []string{"nbg1"},
		NetworkID:     1,
		ImageSelector: ImageSelector{Family: "ubuntu"},
		UserData:      "#cloud-config",
		SSHKeyIDs:     []int64{1, 2},
	}}
	if mutate != nil {
		mutate(&nc.Spec)
	}
	return nc.Hash()
}

func boolPtr(b bool) *bool { return &b }

// The encoding is part of the contract: a change here without a
// NodeClassHashVersion bump rolls every node in every cluster on upgrade.
func TestHash_Stable(t *testing.T) {
	const want = "d3a173f73db6d2e3"
	if got := hashOf(nil); got != want {
		t.Errorf("hash of the reference spec changed to %q (was %q): bump NodeClassHashVersion if this is intended", got, want)
	}
}

func TestHash_EquivalentSpecsHashEqual(t *testing.T) {
	base := hashOf(nil)
	for name, mutate := range map[string]func(*HCloudNodeClassSpec){
		"explicit spread":      func(s *HCloudNodeClassSpec) { s.PlacementGroupStrategy = "spread" },
		"explicit public IPv4": func(s *HCloudNodeClassSpec) { s.EnablePublicIPv4 = boolPtr(true) },
		"explicit public IPv6": func(s *HCloudNodeClassSpec) { s.EnablePublicIPv6 = boolPtr(true) },
		"SSH keys reordered":   func(s *HCloudNodeClassSpec) { s.SSHKeyIDs = []int64{2, 1} },
		"locations (live)":     func(s *HCloudNodeClassSpec) { s.Locations = []string{"fsn1"} },
		"network (live)":       func(s *HCloudNodeClassSpec) { s.NetworkID = 2 },
		"firewalls (live)":     func(s *HCloudNodeClassSpec) { s.FirewallIDs = []int64{9} },
		"labels (live)":        func(s *HCloudNodeClassSpec) { s.Labels = map[string]string{"a": "b"} },
		"image selector":       func(s *HCloudNodeClassSpec) { s.ImageSelector.Version = "24.04" },
		"kubelet (descriptive)": func(s *HCloudNodeClassSpec) {
			s.Kubelet = &KubeletConfiguration{KubeReserved: map[string]ReservedQuantity{"cpu": "1"}}
		},
		"vm memory overhead": func(s *HCloudNodeClassSpec) { s.VMMemoryOverheadPercent = "7" },
	} {
		if got := hashOf(mutate); got != base {
			t.Errorf("%s changed the hash; it must not roll nodes", name)
		}
	}
}

func TestHash_HashedFieldsChangeIt(t *testing.T) {
	base := hashOf(nil)
	for name, mutate := range map[string]func(*HCloudNodeClassSpec){
		"userData": func(s *HCloudNodeClassSpec) { s.UserData = "#cloud-config\n# changed" },
		"userDataSecretRef": func(s *HCloudNodeClassSpec) {
			s.UserDataSecretRef = &UserDataSecretReference{Namespace: "ns", Name: "s", Key: "k"}
		},
		"SSH keys":       func(s *HCloudNodeClassSpec) { s.SSHKeyIDs = []int64{1} },
		"placement none": func(s *HCloudNodeClassSpec) { s.PlacementGroupStrategy = "none" },
		"no public IPv4": func(s *HCloudNodeClassSpec) { s.EnablePublicIPv4 = boolPtr(false) },
		"no public IPv6": func(s *HCloudNodeClassSpec) { s.EnablePublicIPv6 = boolPtr(false) },
	} {
		if got := hashOf(mutate); got == base {
			t.Errorf("%s did not change the hash", name)
		}
	}
	ref := func(key string) func(*HCloudNodeClassSpec) {
		return func(s *HCloudNodeClassSpec) {
			s.UserDataSecretRef = &UserDataSecretReference{Namespace: "ns", Name: "s", Key: key}
		}
	}
	if hashOf(ref("a")) == hashOf(ref("b")) {
		t.Error("changing the Secret reference's key did not change the hash")
	}
}

func TestHash_Bootstrap(t *testing.T) {
	base := hashOf(nil)
	boot := func(mutate func(*BootstrapConfiguration)) func(*HCloudNodeClassSpec) {
		return func(s *HCloudNodeClassSpec) {
			s.Bootstrap = &BootstrapConfiguration{Distribution: "k3s", Server: "https://10.0.0.2:6443",
				TokenSecretRef: BootstrapTokenSecretReference{Name: "join", Key: "token"}}
			if mutate != nil {
				mutate(s.Bootstrap)
			}
		}
	}
	with := hashOf(boot(nil))
	if with == base {
		t.Fatal("adding spec.bootstrap did not change the hash")
	}
	for name, mutate := range map[string]func(*BootstrapConfiguration){
		"distribution": func(b *BootstrapConfiguration) { b.Distribution = "rke2" },
		"server":       func(b *BootstrapConfiguration) { b.Server = "https://10.0.0.3:6443" },
		"token ref":    func(b *BootstrapConfiguration) { b.TokenSecretRef.Name = "other" },
		"version":      func(b *BootstrapConfiguration) { b.Version = "v1.31.5+k3s1" },
		"extra config": func(b *BootstrapConfiguration) { b.ExtraConfig = "node-ip: 10.0.0.9" },
	} {
		if hashOf(boot(mutate)) == with {
			t.Errorf("bootstrap %s did not change the hash", name)
		}
	}
	// With bootstrap the kubelet block is rendered onto the node, so it is hashed.
	withKubelet := hashOf(func(s *HCloudNodeClassSpec) {
		boot(nil)(s)
		s.Kubelet = &KubeletConfiguration{KubeReserved: map[string]ReservedQuantity{"cpu": "1"}}
	})
	if withKubelet == with {
		t.Error("kubelet did not change the hash of a bootstrapped node class")
	}
}
