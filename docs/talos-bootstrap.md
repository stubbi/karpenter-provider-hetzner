# Talos bootstrap guide

This guide walks through the most common path for provisioning Talos Linux
worker nodes on Hetzner Cloud with karpenter-provider-hetzner.

See [`examples/talos-nodeclass.yaml`](../examples/talos-nodeclass.yaml) for a
ready-to-apply NodeClass and NodePool you can adapt.

---

## 1. Why Talos needs a worker machineconfig

Talos does not use cloud-init or SSH.  A node's entire runtime configuration
comes from a single "machineconfig" YAML document supplied as the server's
user-data at creation time.  For worker nodes this document must contain:

- The cluster CA certificate (so the worker trusts the control plane).
- A bootstrap token (so the worker can join via the Kubernetes API).
- The control-plane endpoint (IP or DNS name + port 6443).

These are secrets.  They must never appear in a NodeClass spec, a Helm values
file, or any git-tracked file.  `userDataSecretRef` exists precisely to keep
them out of both.

---

## 2. Obtaining the worker machineconfig

### Option A: `talosctl gen config` (fresh cluster or rotation)

```bash
# Generate a full set of cluster secrets and configs.
talosctl gen config <cluster-name> https://<control-plane-ip>:6443 \
  --output-dir /tmp/talos-configs

# /tmp/talos-configs/worker.yaml is the worker machineconfig.
# Store it in a Secret immediately; delete the plaintext copy when done.
```

If your cluster already exists and you need to regenerate configs without
rotating the CA, pass `--with-secrets <secrets-bundle>` (a file produced by
`talosctl gen secrets` and stored securely out of band).

### Option B: reuse the worker cloudInit from a cluster-autoscaler-hetzner Secret

If you previously ran
[cluster-autoscaler-hetzner](https://github.com/johannesfrey/cluster-autoscaler-hetzner),
your Hetzner project likely has an autoscaler config Secret (often
`cluster-autoscaler-hetzner` in `kube-system`).  That Secret embeds a
`worker_cloud_init_<nodepool>` key containing the already-rendered worker
machineconfig.

```bash
# Extract it:
kubectl get secret cluster-autoscaler-hetzner \
  --namespace kube-system \
  -o jsonpath='{.data.worker_cloud_init_default}' \
  | base64 -d > /tmp/worker.yaml
```

Inspect `/tmp/worker.yaml` to confirm it contains the current cluster CA and a
valid join token before storing it as a Karpenter Secret (tokens expire).

---

## 3. Store the machineconfig in a Kubernetes Secret

```bash
kubectl create secret generic talos-worker-machineconfig \
  --namespace kube-system \
  --from-file=worker.yaml=/tmp/worker.yaml

# Delete the plaintext copy.
rm /tmp/worker.yaml
```

Security considerations:

- Restrict access to the Secret with RBAC.  Only the karpenter-provider-hetzner
  ServiceAccount needs read access.
- Kubeadm join tokens expire (default 24 h).  Rotate them before they expire
  and update the Secret; the provider reads the Secret at server-create time,
  so a stale token causes new nodes to fail to join.
- Never commit the Secret manifest with real data.  The commented template in
  `examples/talos-nodeclass.yaml` is a safe starting point.

---

## 4. Pin the exact image with `imageSelector.selector`

Talos images must match three things simultaneously:

1. The Talos version running on your control plane.
2. The system extensions baked into the image (e.g. gVisor, NVIDIA drivers).
3. The CPU architecture of the server type Karpenter selects.

A fuzzy `version` substring match can accidentally resolve an older snapshot
or one missing a required extension.  Use `imageSelector.selector` to pin the
exact image by an hcloud label you applied when uploading it:

```bash
# When you upload your Talos image to Hetzner:
hcloud image update <image-id> --label caph-image-name=talos-v1.9.5-gvisor

# In the NodeClass:
imageSelector:
  family: talos
  selector:
    caph-image-name: "talos-v1.9.5-gvisor"
```

The provider resolves one image per architecture that matches **all** selector
labels.  Each architecture resolves independently, so a NodeClass stays Ready as
long as **at least one** does.  When none resolves, `ImagesReady` goes `False` if
the image catalogue was readable and held nothing, and `Unknown` if the catalogue
could not be read at all — alert on `ImagesReady != True`, not on `False` alone,
and on `karpenter_hetzner_image_resolution_errors_total` for the unreadable case.

Selection then skips any instance type whose architecture has no resolved image.
The consequences differ by NodePool:

- A NodePool permitting several architectures **silently launches one of the
  others**.  A mislabelled arm64 snapshot does not fail — it quietly yields
  amd64 nodes, possibly at a higher price.  Watch
  `karpenter_hetzner_instance_type_selection_skipped_total{reason="no_resolved_image"}`,
  which counts one per launch that had to route around the missing image.
- A NodePool pinned to the architecture with no image fails the launch with a
  `NodeClassNotReady` error naming the missing architecture, and Karpenter
  deletes the NodeClaim rather than retrying it forever.

For the multi-arch pattern (amd64 + arm64 NodePools sharing one NodeClass), the
selector label must be present on both the x86 and ARM snapshots.

---

## 5. Apply the NodeClass and NodePool

```bash
# Adjust networkID, selector label, Secret name, and locations first.
kubectl apply -f examples/talos-nodeclass.yaml
```

Verify the NodeClass becomes Ready:

```bash
kubectl get hcloudnodeclasses talos-default
# NAME            READY
# talos-default   True
```

If `READY` is `False`, describe the resource and inspect the status conditions:

```bash
kubectl describe hcloudnodeclass talos-default
# Look for ImagesReady, NetworkReady, ResourcesReady, UserDataReady.
```

### Verifying node join

Once a pod is pending and Karpenter provisions a node, watch for it:

```bash
kubectl get nodes -w
```

If a node appears but stays `NotReady`, check the Talos API:

```bash
talosctl --nodes <node-ip> --talosconfig /path/to/talosconfig dmesg | tail -50
talosctl --nodes <node-ip> --talosconfig /path/to/talosconfig health
```

Common join failures:

| Symptom | Likely cause |
|---------|-------------|
| Node never appears | Token expired; update the Secret and delete the pending NodeClaim so Karpenter retries. |
| Node appears, `NotReady` | Control-plane endpoint unreachable from the private network; check firewall rules and network routing. |
| `ImagesReady=False` | No image matches the selector for the requested arch; check `hcloud image list --selector caph-image-name=...`. |

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
