// Package bootstrap renders the cloud-config that joins a server to a k3s or
// RKE2 cluster as an agent, from an HCloudNodeClass's spec.bootstrap.
//
// The provider owns the parts of the join that track Karpenter rather than the
// user's cluster: the karpenter.sh/unregistered:NoExecute startup taint and
// kubelet flags matching the allocatable the provider advertises. Node labels
// are not rendered: Karpenter core copies the NodeClaim's labels onto the Node
// at registration, and this provider fills those in Create.
package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	apiv1 "github.com/paperclipinc/karpenter-provider-hetzner/pkg/apis/v1"
	"github.com/paperclipinc/karpenter-provider-hetzner/pkg/providers/instancetype"
)

const (
	cloudConfigHeader = "#cloud-config"

	// UnregisteredTaint is Karpenter's startup taint. Core removes it once it
	// has synced the NodeClaim onto the Node; without it pods can land on the
	// node before that sync.
	UnregisteredTaint = "karpenter.sh/unregistered=:NoExecute"
)

// ErrInvalid wraps every error caused by the node class's own content (a bad
// extension, a collision with a provider-owned key). Retrying cannot fix those.
var ErrInvalid = errors.New("invalid bootstrap configuration")

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Input is everything the renderer needs. Token is the resolved Secret value;
// UserData is the operator's cloud-config extension (may be empty).
type Input struct {
	Bootstrap apiv1.BootstrapConfiguration
	Kubelet   *apiv1.KubeletConfiguration
	Token     string
	UserData  string
}

// ConfigPath returns the agent config file the provider owns for a distribution.
func ConfigPath(distribution string) string {
	return "/etc/rancher/" + distribution + "/config.yaml"
}

// providerKubeletFlags are the kubelet flags the provider renders and therefore
// owns. An extraConfig kubelet-arg setting one of them would make the node
// disagree with the advertised allocatable.
var providerKubeletFlags = []string{"cloud-provider", "eviction-hard", "kube-reserved", "max-pods", "system-reserved"}

// Render returns the cloud-config for a node. Output is deterministic: identical
// inputs give byte-identical output (keys sorted, list order fixed), so a
// rendered value can be compared or hashed safely.
func Render(in Input) (string, error) {
	b := in.Bootstrap
	if b.Distribution != "k3s" && b.Distribution != "rke2" {
		return "", invalidf("unsupported distribution %q: must be k3s or rke2", b.Distribution)
	}
	if strings.TrimSpace(in.Token) == "" {
		return "", invalidf("the join token is empty")
	}

	agentConfig, err := renderAgentConfig(b, in.Kubelet, strings.TrimSpace(in.Token))
	if err != nil {
		return "", err
	}
	base := map[string]any{
		"write_files": []any{map[string]any{
			"path":        ConfigPath(b.Distribution),
			"owner":       "root:root",
			"permissions": "0600",
			"content":     agentConfig,
		}},
		"runcmd": installCommands(b),
	}

	merged, err := mergeUserData(base, in.UserData, ConfigPath(b.Distribution))
	if err != nil {
		return "", err
	}
	out, err := yaml.Marshal(merged)
	if err != nil {
		return "", fmt.Errorf("encoding cloud-config: %w", err)
	}
	return cloudConfigHeader + "\n" + string(out), nil
}

func renderAgentConfig(b apiv1.BootstrapConfiguration, kubelet *apiv1.KubeletConfiguration, token string) (string, error) {
	cfg := map[string]any{}
	if strings.TrimSpace(b.ExtraConfig) != "" {
		if err := yaml.Unmarshal([]byte(b.ExtraConfig), &cfg); err != nil {
			return "", invalidf("extraConfig is not a YAML mapping: %v", err)
		}
		if cfg == nil {
			cfg = map[string]any{}
		}
	}
	for _, owned := range []string{"server", "token", "token-file"} {
		if _, ok := cfg[owned]; ok {
			return "", invalidf("extraConfig sets %q, which the provider renders from spec.bootstrap", owned)
		}
	}

	taints, err := stringList(cfg, "node-taint")
	if err != nil {
		return "", err
	}
	for _, t := range taints {
		if strings.HasPrefix(t, "karpenter.sh/unregistered") {
			return "", invalidf("extraConfig sets the %s taint, which the provider renders", UnregisteredTaint)
		}
	}
	userArgs, err := stringList(cfg, "kubelet-arg")
	if err != nil {
		return "", err
	}
	for _, a := range userArgs {
		flag, _, _ := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if slices.Contains(providerKubeletFlags, flag) {
			return "", invalidf("extraConfig kubelet-arg sets %q, which the provider renders from spec.kubelet so the node matches its advertised allocatable", flag)
		}
	}
	labels, err := stringList(cfg, "node-label")
	if err != nil {
		return "", err
	}

	cfg["server"] = b.Server
	cfg["token"] = token
	cfg["node-taint"] = toAny(append([]string{UnregisteredTaint}, taints...))
	cfg["kubelet-arg"] = toAny(append(KubeletArgs(kubelet), userArgs...))
	if len(labels) > 0 {
		cfg["node-label"] = toAny(labels)
	} else {
		delete(cfg, "node-label")
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return "", fmt.Errorf("encoding agent config: %w", err)
	}
	return string(out), nil
}

// KubeletArgs renders the kubelet flags that make the node reserve exactly what
// the provider subtracts from capacity. The two eviction signals the provider
// models (memory.available, nodefs.available) are always rendered, at the
// declared value or the default the model assumes; other declared signals pass
// through. Without this, a kubelet given any eviction-hard zeroes unlisted
// signals while the model still subtracts them.
func KubeletArgs(k *apiv1.KubeletConfiguration) []string {
	if k == nil {
		k = &apiv1.KubeletConfiguration{}
	}
	args := []string{"cloud-provider=external"}

	eviction := map[string]string{
		"memory.available": instancetype.DefaultMemoryEvictionThreshold,
		"nodefs.available": instancetype.DefaultNodefsEvictionThreshold,
	}
	for signal, v := range k.EvictionHard {
		eviction[signal] = string(v)
	}
	parts := make([]string, 0, len(eviction))
	for _, signal := range sortedKeys(eviction) {
		parts = append(parts, signal+"<"+eviction[signal])
	}
	args = append(args, "eviction-hard="+strings.Join(parts, ","))

	if r := reservation(k.KubeReserved); r != "" {
		args = append(args, "kube-reserved="+r)
	}
	if k.MaxPods != nil {
		args = append(args, fmt.Sprintf("max-pods=%d", *k.MaxPods))
	}
	if r := reservation(k.SystemReserved); r != "" {
		args = append(args, "system-reserved="+r)
	}
	return args
}

func reservation(m map[string]apiv1.ReservedQuantity) string {
	if len(m) == 0 {
		return ""
	}
	plain := make(map[string]string, len(m))
	for k, v := range m {
		plain[k] = string(v)
	}
	parts := make([]string, 0, len(plain))
	for _, k := range sortedKeys(plain) {
		parts = append(parts, k+"="+plain[k])
	}
	return strings.Join(parts, ",")
}

func installCommands(b apiv1.BootstrapConfiguration) []any {
	// The installers read the agent config written above, so server and token are
	// not repeated on the command line (where they would show up in process lists).
	switch b.Distribution {
	case "rke2":
		env := "INSTALL_RKE2_TYPE=agent"
		if b.Version != "" {
			env += " INSTALL_RKE2_VERSION=" + b.Version
		}
		return []any{
			"curl -sfL https://get.rke2.io | " + env + " sh -",
			"systemctl enable --now rke2-agent.service",
		}
	default:
		env := ""
		if b.Version != "" {
			env = "INSTALL_K3S_VERSION=" + b.Version + " "
		}
		return []any{"curl -sfL https://get.k3s.io | " + env + "sh -s - agent"}
	}
}

// mergeUserData merges the operator's cloud-config extension into the rendered
// base, per top-level key:
//
//   - a key only the extension sets passes through untouched;
//   - write_files: combined, provider entries first; an entry for the provider's
//     agent config path is an error;
//   - any other key both set (runcmd today) must be a list in the extension and
//     is combined, provider entries first, so a user runcmd can never replace
//     the install commands;
//   - anything else both set is an error, never a silent override.
func mergeUserData(base map[string]any, userData, ownedPath string) (map[string]any, error) {
	trimmed := strings.TrimSpace(userData)
	if trimmed == "" {
		return base, nil
	}
	if !strings.HasPrefix(trimmed, cloudConfigHeader) {
		return nil, invalidf("with spec.bootstrap, userData must be a cloud-config document starting with %s", cloudConfigHeader)
	}
	user := map[string]any{}
	if err := yaml.Unmarshal([]byte(trimmed), &user); err != nil {
		return nil, invalidf("userData is not a YAML mapping: %v", err)
	}
	out := make(map[string]any, len(base)+len(user))
	for k, v := range base {
		out[k] = v
	}
	for k, uv := range user {
		bv, owned := base[k]
		if !owned {
			out[k] = uv
			continue
		}
		ul, ok := uv.([]any)
		bl, baseIsList := bv.([]any)
		if !ok || !baseIsList {
			return nil, invalidf("userData sets %q, which the provider renders, as a non-list value", k)
		}
		if k == "write_files" {
			for _, f := range ul {
				if m, ok := f.(map[string]any); ok && m["path"] == ownedPath {
					return nil, invalidf("userData write_files includes %s, which the provider renders from spec.bootstrap", ownedPath)
				}
			}
		}
		out[k] = append(slices.Clone(bl), ul...)
	}
	return out, nil
}

func stringList(cfg map[string]any, key string) ([]string, error) {
	raw, ok := cfg[key]
	if !ok {
		return nil, nil
	}
	switch v := raw.(type) {
	case string:
		return []string{v}, nil
	case []any:
		out := make([]string, 0, len(v))
		for _, e := range v {
			s, ok := e.(string)
			if !ok {
				return nil, invalidf("extraConfig %s entries must be strings", key)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, invalidf("extraConfig %s must be a string or a list of strings", key)
	}
}

func toAny(in []string) []any {
	out := make([]any, len(in))
	for i, s := range in {
		out[i] = s
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// IsInvalid reports whether err is caused by the node class's own content.
func IsInvalid(err error) bool { return errors.Is(err, ErrInvalid) }

// ReadToken reads the join token from the Secret named by ref in namespace, the
// provider's own. NotFound and an empty key are reported as ErrTokenUnavailable
// so callers can tell a configuration gap from an API error.
func ReadToken(ctx context.Context, c client.Reader, namespace string, ref apiv1.BootstrapTokenSecretReference) (string, error) {
	if namespace == "" {
		return "", fmt.Errorf("%w: the provider's namespace is unknown (set POD_NAMESPACE)", ErrTokenUnavailable)
	}
	secret := &corev1.Secret{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", fmt.Errorf("%w: secret %s/%s not found", ErrTokenUnavailable, namespace, ref.Name)
		}
		return "", fmt.Errorf("reading bootstrap token secret %s/%s: %w", namespace, ref.Name, err)
	}
	v := strings.TrimSpace(string(secret.Data[ref.Key]))
	if v == "" {
		return "", fmt.Errorf("%w: secret %s/%s has no non-empty key %q", ErrTokenUnavailable, namespace, ref.Name, ref.Key)
	}
	return v, nil
}

// ErrTokenUnavailable reports that the join token Secret or key does not exist.
var ErrTokenUnavailable = errors.New("bootstrap token unavailable")
