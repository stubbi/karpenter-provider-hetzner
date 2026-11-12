# Provider-rendered k3s / RKE2 bootstrap (`spec.bootstrap`)

`spec.bootstrap` makes the provider write the agent join configuration itself,
instead of you hand-writing it in `userData`. It is opt-in: a NodeClass without
it behaves exactly as before, and the hand-written [k3s recipe](k3s-bootstrap.md)
keeps working.

```yaml
apiVersion: karpenter.hetzner.cloud/v1
kind: HCloudNodeClass
metadata:
  name: k3s-agents
spec:
  locations: [nbg1, fsn1]
  networkID: 123456
  imageSelector:
    family: ubuntu
    version: "24.04"
  bootstrap:
    distribution: k3s            # or rke2
    server: https://10.0.0.2:6443 # RKE2: https://<server>:9345
    tokenSecretRef:               # read from the provider's namespace only
      name: k3s-agent-token
      key: token
    version: v1.31.5+k3s1         # match your servers; omit for the stable channel
    extraConfig: |
      flannel-iface: enp7s0
  kubelet:                        # rendered onto the node AND used for allocatable
    systemReserved: {cpu: 200m, memory: 512Mi}
    kubeReserved: {cpu: 200m, memory: 512Mi}
    evictionHard: {memory.available: 400Mi, nodefs.available: 10%}
    maxPods: 110
  userData: |                     # optional extension, merged (see below)
    #cloud-config
    packages: [open-iscsi]
```

A complete example with a NodePool is in
[`examples/k3s-bootstrap-nodeclass.yaml`](../examples/k3s-bootstrap-nodeclass.yaml).

## What the provider renders

- `/etc/rancher/<distribution>/config.yaml` (mode `0600`) containing:
  - `server` and `token` (from the Secret);
  - `node-taint: karpenter.sh/unregistered=:NoExecute`, the startup taint core
    removes once it has synced the NodeClaim onto the Node;
  - `kubelet-arg`: `cloud-provider=external`, plus `eviction-hard`,
    `kube-reserved`, `system-reserved` and `max-pods` from `spec.kubelet`.
- The install command (`get.k3s.io … agent`, or `get.rke2.io` with
  `INSTALL_RKE2_TYPE=agent` followed by enabling `rke2-agent`), pinned to
  `version` when it is set. The token is read from the config file and never
  appears on the command line.

Node labels (`karpenter.sh/nodepool`, instance type, zone, capacity type) are
**not** rendered: Karpenter core copies the NodeClaim's labels onto the Node at
registration, and the provider fills them in when it launches the server.

### Reservations are prescriptive here

Without `spec.bootstrap`, `spec.kubelet` only *describes* your bootstrap to the
scheduler. With it, the provider writes those values into the kubelet flags, so
the node reserves exactly what Karpenter subtracts from capacity. An empty or
absent `kubelet` block renders the kubelet's own defaults
(`eviction-hard=memory.available<100Mi,nodefs.available<10%`, no reservations).
Allocatable is modelled the same way, and the legacy flat 100m/100Mi guess does
not apply. The two eviction signals the model uses are always rendered
explicitly, because a kubelet given any `eviction-hard` drops the signals it does
not list.

## Merging your `userData`

With `spec.bootstrap`, `userData` (or the content of `userDataSecretRef`) must be
a `#cloud-config` document. Admission rejects anything else inline, and the
`BootstrapReady` condition reports a Secret that does not parse. It is merged
per top-level key:

| Key | Policy |
|---|---|
| Only you set it (`packages`, `bootcmd`, `timezone`, …) | Passed through untouched |
| `runcmd` | Combined: the provider's install command first, then yours |
| `write_files` | Combined, the provider's file first. A file at the provider's config path is an **error** |
| A provider key set to a non-list value | **Error**: no silent override in either direction |

Your `runcmd` runs after the agent install. Put node preparation that must happen
before the install in `bootcmd`.

`extraConfig` is merged into the agent config file the same way. Any key passes
through, except:

- `server`, `token` and `token-file`: rejected;
- `node-taint`, `node-label` and `kubelet-arg`: appended after the provider's
  entries;
- a `karpenter.sh/unregistered` taint, or a `kubelet-arg` setting
  `cloud-provider`, `eviction-hard`, `kube-reserved`, `system-reserved` or
  `max-pods`: rejected. Those are rendered from `spec.kubelet` so the node
  matches its advertised allocatable.

Rendering is deterministic: identical inputs give byte-identical userData.

## The join token

- Store it in a Secret **in the namespace the provider runs in**. `tokenSecretRef`
  has no namespace field, and a Secret elsewhere is never read.
- **Use an agent token** (start the servers with `--agent-token`, or use RKE2's
  agent token), not the server token. The rendered userData includes the token,
  and Hetzner serves userData from the metadata service (`169.254.169.254`),
  where any process on the node can read it. A server token there would let a
  compromised node join as a control-plane server.
- Rotating the Secret's value does not replace existing nodes (they have already
  joined); new nodes use the new value. Changing `tokenSecretRef` itself does.

## Validation and status

Admission rejects `bootstrap` with `imageSelector.family: talos` (Talos joins
through its machine config), a non-`https` `server`, an unknown `distribution`,
and inline `userData` that is not a `#cloud-config`.

The `BootstrapReady` condition (part of `Ready`) is `False` when the token Secret
or key is missing (`BootstrapTokenUnavailable`) or the extension cannot be merged
(`BootstrapInvalid`). A launch in that state fails with `NodeClassNotReady`, so
core deletes the NodeClaim instead of retrying an identical server.

The provider cannot observe whether the agent actually joined. If the token is
valid but wrong, or the server is unreachable, the server boots without
registering, and Karpenter core's registration timeout removes the NodeClaim and
the server. Check `journalctl -u k3s-agent` (or `rke2-agent`) on the node.

## Drift

`distribution`, `server`, `tokenSecretRef` (the reference), `version`,
`extraConfig` and, with `bootstrap` set, `kubelet` are part of the NodeClass hash.
Editing any of them replaces existing nodes (`NodeClassDrift`); see the README's
Drift section.
