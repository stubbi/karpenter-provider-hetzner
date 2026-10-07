# Ubuntu bootstrap recipe

A concise guide to joining Ubuntu worker nodes via cloud-init and kubeadm with
karpenter-provider-hetzner.

See [`examples/ubuntu-nodeclass.yaml`](../examples/ubuntu-nodeclass.yaml) for
an apply-ready NodeClass and NodePool with a full cloud-init template.

---

## How it works

Hetzner Cloud runs the `userData` string as a cloud-init script on first boot.
The script in `examples/ubuntu-nodeclass.yaml`:

1. Enables required kernel modules and sysctl settings.
2. Installs containerd with `SystemdCgroup = true`.
3. Installs kubeadm, kubelet, and kubectl from the Kubernetes apt repository.
4. Calls `kubeadm join` with the control-plane endpoint, bootstrap token, and
   CA cert hash.

The node appears in `kubectl get nodes` within two to three minutes of the
Hetzner server becoming active.

---

## Step 1: generate a kubeadm join command

On an existing control-plane node:

```bash
kubeadm token create --print-join-command
# Example output:
# kubeadm join 10.0.0.2:6443 --token abcdef.0123456789abcdef \
#   --discovery-token-ca-cert-hash sha256:abc123...
```

Tokens expire after 24 hours by default.  Generate a new token each time you
update the user-data Secret or rotate credentials.

---

## Step 2: put the join command in the NodeClass

Replace the three `<PLACEHOLDER>` values in `examples/ubuntu-nodeclass.yaml`:

```yaml
userData: |
  #cloud-config
  runcmd:
    - kubeadm join <CONTROL_PLANE_ENDPOINT>:6443 \
        --token <BOOTSTRAP_TOKEN> \
        --discovery-token-ca-cert-hash sha256:<CA_CERT_HASH>
```

### Keeping secrets out of git

The bootstrap token carries cluster access.  Consider storing the full
cloud-init blob in a Kubernetes Secret and referencing it with
`userDataSecretRef` instead of placing it inline:

```bash
kubectl create secret generic ubuntu-worker-userdata \
  --namespace kube-system \
  --from-file=cloud-init.yaml=/path/to/cloud-init.yaml
```

```yaml
# In the NodeClass spec — remove userData and add:
userDataSecretRef:
  namespace: kube-system
  name: ubuntu-worker-userdata
  key: cloud-init.yaml
```

`userDataSecretRef` takes precedence over `userData` when both are set.

---

## Step 3: apply and verify

```bash
kubectl apply -f examples/ubuntu-nodeclass.yaml

# Verify the NodeClass is Ready:
kubectl get hcloudnodeclasses ubuntu-default

# Watch for new nodes:
kubectl get nodes -w
```

If a node does not join within five minutes, SSH to it (while `enablePublicIPv4`
is `true`, or via Hetzner console) and inspect cloud-init logs:

```bash
cat /var/log/cloud-init-output.log
journalctl -u kubelet --no-pager | tail -40
```

---

### Kubelet reservations

Once a node has joined, check that the NodeClass's `spec.kubelet` matches the
reservations its kubelet actually applies, or Karpenter will size nodes larger
than they are and leave pods Pending:

```bash
kubectl get --raw /api/v1/nodes/<node>/proxy/configz \
  | jq '.kubeletconfig | {systemReserved, kubeReserved, evictionHard}'
```

Copy any non-empty values into `spec.kubelet` (see the README's NodeClass
reference for the exact semantics).

## Trade-offs vs Talos

| | Ubuntu + kubeadm | Talos |
|--|--|--|
| Custom image required | No (Hetzner public image) | Yes (upload per version + extensions) |
| SSH access | Yes | No (by design) |
| Attack surface | Larger (general-purpose OS) | Minimal (immutable, no shell) |
| Upgrade path | Standard apt + kubeadm upgrade | Talosctl upgrade (atomic) |
| Bootstrap secret handling | Token in cloud-init (short-lived) | Machineconfig CA + token in Secret |
| Best fit | Dev/test, mixed workloads | Production, security-focused |

For production clusters, Talos is the recommended path.  See
[`docs/talos-bootstrap.md`](talos-bootstrap.md) for the full guide.
