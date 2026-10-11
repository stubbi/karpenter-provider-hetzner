package bootstrap

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"sigs.k8s.io/yaml"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
)

var update = flag.Bool("update", false, "rewrite golden files")

func k3s() apiv1.BootstrapConfiguration {
	return apiv1.BootstrapConfiguration{
		Distribution:   "k3s",
		Server:         "https://10.0.0.2:6443",
		TokenSecretRef: apiv1.BootstrapTokenSecretReference{Name: "join", Key: "token"},
		Version:        "v1.31.5+k3s1",
	}
}

func maxPods(n int32) *int32 { return &n }

// parse decodes rendered cloud-config, and the agent config file inside it.
func parse(t *testing.T, out string) (map[string]any, map[string]any) {
	t.Helper()
	if !strings.HasPrefix(out, "#cloud-config\n") {
		t.Fatalf("output does not start with #cloud-config:\n%s", out)
	}
	doc := map[string]any{}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("output is not YAML: %v\n%s", err, out)
	}
	files, _ := doc["write_files"].([]any)
	if len(files) == 0 {
		t.Fatalf("no write_files in:\n%s", out)
	}
	first := files[0].(map[string]any)
	cfg := map[string]any{}
	if err := yaml.Unmarshal([]byte(first["content"].(string)), &cfg); err != nil {
		t.Fatalf("agent config is not YAML: %v", err)
	}
	return doc, cfg
}

func TestRender_Golden(t *testing.T) {
	rke2 := k3s()
	rke2.Distribution = "rke2"
	rke2.Server = "https://10.0.0.2:9345"
	rke2.Version = "v1.31.5+rke2r1"
	for name, in := range map[string]Input{
		"k3s-minimal": {Bootstrap: k3s(), Token: "K10agent::token"},
		"k3s-full": {
			Bootstrap: func() apiv1.BootstrapConfiguration {
				b := k3s()
				b.ExtraConfig = "flannel-iface: enp7s0\nnode-ip: 10.0.1.5\nkubelet-arg: [image-gc-high-threshold=70]\nnode-label: [team=a]\n"
				return b
			}(),
			Kubelet: &apiv1.KubeletConfiguration{
				SystemReserved: map[string]apiv1.ReservedQuantity{"memory": "512Mi", "cpu": "200m"},
				KubeReserved:   map[string]apiv1.ReservedQuantity{"cpu": "200m", "memory": "512Mi"},
				EvictionHard:   map[string]apiv1.EvictionThreshold{"memory.available": "400Mi", "imagefs.available": "5%"},
				MaxPods:        maxPods(200),
			},
			Token:    "K10agent::token",
			UserData: "#cloud-config\npackages: [jq]\nruncmd: [echo joined]\nwrite_files:\n- path: /etc/motd\n  content: hi\n",
		},
		"rke2-minimal": {Bootstrap: rke2, Token: "agent-token"},
	} {
		t.Run(name, func(t *testing.T) {
			out, err := Render(in)
			if err != nil {
				t.Fatal(err)
			}
			golden := filepath.Join("testdata", name+".golden")
			if *update {
				if err := os.WriteFile(golden, []byte(out), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run go test -update to create it)", err)
			}
			if out != string(want) {
				t.Errorf("rendered output differs from %s:\n%s", golden, out)
			}
			// Deterministic: maps are iterated in random order, so rendering the same
			// input repeatedly must still produce identical bytes.
			for range 20 {
				again, _ := Render(in)
				if again != out {
					t.Fatal("rendering is not deterministic")
				}
			}
		})
	}
}

// The contract the provider owns: server, token, the unregistered taint and the
// kubelet flags matching the advertised allocatable.
func TestRender_RegistrationContract(t *testing.T) {
	out, err := Render(Input{Bootstrap: k3s(), Token: "  tok\n"})
	if err != nil {
		t.Fatal(err)
	}
	doc, cfg := parse(t, out)
	if cfg["server"] != "https://10.0.0.2:6443" || cfg["token"] != "tok" {
		t.Errorf("server/token not rendered: %v", cfg)
	}
	taints, _ := cfg["node-taint"].([]any)
	if len(taints) != 1 || taints[0] != UnregisteredTaint {
		t.Errorf("node-taint %v, want only %s", taints, UnregisteredTaint)
	}
	file := doc["write_files"].([]any)[0].(map[string]any)
	if file["path"] != "/etc/rancher/k3s/config.yaml" || file["permissions"] != "0600" {
		t.Errorf("agent config file %v", file)
	}
	runcmd := doc["runcmd"].([]any)
	if len(runcmd) != 1 || !strings.Contains(runcmd[0].(string), "INSTALL_K3S_VERSION=v1.31.5+k3s1") ||
		strings.Contains(runcmd[0].(string), "tok") {
		t.Errorf("install command %v must pin the version and keep the token off the command line", runcmd)
	}
}

func TestKubeletArgs(t *testing.T) {
	// Nothing declared: the kubelet defaults the provider models, written out so
	// the node and the model agree.
	if got, want := strings.Join(KubeletArgs(nil), " "),
		"cloud-provider=external eviction-hard=memory.available<100Mi,nodefs.available<10%"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	got := KubeletArgs(&apiv1.KubeletConfiguration{
		SystemReserved: map[string]apiv1.ReservedQuantity{"memory": "512Mi", "cpu": "200m"},
		KubeReserved:   map[string]apiv1.ReservedQuantity{"cpu": "100m"},
		EvictionHard:   map[string]apiv1.EvictionThreshold{"memory.available": "400Mi", "nodefs.available": "0%", "pid.available": "5%"},
		MaxPods:        maxPods(64),
	})
	want := []string{
		"cloud-provider=external",
		"eviction-hard=memory.available<400Mi,nodefs.available<0%,pid.available<5%",
		"kube-reserved=cpu=100m",
		"max-pods=64",
		"system-reserved=cpu=200m,memory=512Mi",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("got %q\nwant %q", got, want)
	}
}

// The merge table: list keys combine provider-first, keys only the user sets pass
// through, and anything that would replace provider-owned content is an error.
func TestRender_MergeUserData(t *testing.T) {
	out, err := Render(Input{Bootstrap: k3s(), Token: "t",
		UserData: "#cloud-config\nruncmd: [echo user]\nwrite_files:\n- {path: /etc/motd, content: hi}\ntimezone: Europe/Berlin\n"})
	if err != nil {
		t.Fatal(err)
	}
	doc, _ := parse(t, out)
	runcmd := doc["runcmd"].([]any)
	if len(runcmd) != 2 || !strings.Contains(runcmd[0].(string), "get.k3s.io") || runcmd[1] != "echo user" {
		t.Errorf("runcmd %v: provider install must come first, user command after", runcmd)
	}
	files := doc["write_files"].([]any)
	if len(files) != 2 || files[0].(map[string]any)["path"] != "/etc/rancher/k3s/config.yaml" ||
		files[1].(map[string]any)["path"] != "/etc/motd" {
		t.Errorf("write_files %v: provider file first, user file after", files)
	}
	if doc["timezone"] != "Europe/Berlin" {
		t.Errorf("user-only key not passed through: %v", doc["timezone"])
	}

	for name, userData := range map[string]string{
		"not cloud-config":     "#!/bin/sh\necho hi\n",
		"not a mapping":        "#cloud-config\n- a\n- b\n",
		"owned file path":      "#cloud-config\nwrite_files:\n- {path: /etc/rancher/k3s/config.yaml, content: x}\n",
		"runcmd as a scalar":   "#cloud-config\nruncmd: echo replaced\n",
		"write_files as a map": "#cloud-config\nwrite_files: {path: /x}\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Render(Input{Bootstrap: k3s(), Token: "t", UserData: userData}); !IsInvalid(err) {
				t.Errorf("want an invalid-configuration error, got %v", err)
			}
		})
	}
}

func TestRender_ExtraConfig(t *testing.T) {
	b := k3s()
	b.ExtraConfig = "node-taint: [dedicated=gpu:NoSchedule]\nkubelet-arg: image-gc-high-threshold=70\nnode-ip: 10.0.1.5\n"
	out, err := Render(Input{Bootstrap: b, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	_, cfg := parse(t, out)
	taints := cfg["node-taint"].([]any)
	if len(taints) != 2 || taints[0] != UnregisteredTaint || taints[1] != "dedicated=gpu:NoSchedule" {
		t.Errorf("node-taint %v: provider taint first, user taint appended", taints)
	}
	args := cfg["kubelet-arg"].([]any)
	if args[len(args)-1] != "image-gc-high-threshold=70" || args[0] != "cloud-provider=external" {
		t.Errorf("kubelet-arg %v: provider flags first, user flag appended", args)
	}
	if cfg["node-ip"] != "10.0.1.5" {
		t.Errorf("pass-through key lost: %v", cfg)
	}

	for name, extra := range map[string]string{
		"server":               "server: https://evil:6443",
		"token":                "token: x",
		"token-file":           "token-file: /x",
		"unregistered taint":   "node-taint: [karpenter.sh/unregistered=:NoExecute]",
		"owned kubelet flag":   "kubelet-arg: [max-pods=500]",
		"owned flag with dash": "kubelet-arg: [--kube-reserved=cpu=1]",
		"not a mapping":        "- a",
		"non-string list":      "node-taint: [1]",
	} {
		t.Run(name, func(t *testing.T) {
			b := k3s()
			b.ExtraConfig = extra
			if _, err := Render(Input{Bootstrap: b, Token: "t"}); !IsInvalid(err) {
				t.Errorf("want an invalid-configuration error, got %v", err)
			}
		})
	}
}

func TestRender_RejectsBadInput(t *testing.T) {
	if _, err := Render(Input{Bootstrap: k3s(), Token: " "}); !IsInvalid(err) {
		t.Errorf("empty token: got %v", err)
	}
	b := k3s()
	b.Distribution = "kubeadm"
	if _, err := Render(Input{Bootstrap: b, Token: "t"}); !IsInvalid(err) {
		t.Errorf("unknown distribution: got %v", err)
	}
}
