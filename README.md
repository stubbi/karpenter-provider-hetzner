# karpenter-provider-hetzner

[![CI](https://github.com/stubbi/karpenter-provider-hetzner/actions/workflows/ci.yaml/badge.svg)](https://github.com/stubbi/karpenter-provider-hetzner/actions/workflows/ci.yaml)
[![Go Report Card](https://goreportcard.com/badge/github.com/paperclipinc/karpenter-provider-hetzner)](https://goreportcard.com/report/github.com/paperclipinc/karpenter-provider-hetzner)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Artifact Hub](https://img.shields.io/endpoint?url=https://artifacthub.io/badge/repository/karpenter-provider-hetzner)](https://artifacthub.io/packages/helm/karpenter-provider-hetzner/karpenter-provider-hetzner)

A [Karpenter](https://karpenter.sh) cloud provider for [Hetzner Cloud](https://www.hetzner.com/cloud). It provisions, bin-packs, and autoscales Hetzner Cloud servers as Kubernetes nodes, picking the cost-optimal server type for pending pods from Hetzner's real-time pricing.

## Status

**Stable (v1.0.0).** The full CloudProvider surface is implemented with unit and
controller test coverage, and the provision → join → drift → consolidation
lifecycle is validated end-to-end against a live Talos cluster (happy, drift,
consolidation, fallback, and invalid-nodeclass scenarios). Releases are
multi-arch, cosign-signed, and ship SLSA provenance + an SBOM. Pin a released
version tag in production.

## Features

- **All Hetzner Cloud server types** — CX, CPX, CAX, CCX, including ARM (CAX).
- **Cost-optimal scheduling** — offerings are priced from Hetzner's live per-hour net price, so Karpenter bin-packs onto the cheapest type that fits.
- **Capacity-aware** — when Hetzner reports a type as unavailable in a location, that offering is skipped for a short window so scheduling falls back to an alternative instead of looping on a sold-out combination.
- **Drift detection** — image (pinned selectors only), NodeClass spec (userData, SSH keys, placement, public IPs), network, firewall, location, label and server-type drift trigger node replacement. See [Drift](#drift).
- **Talos Linux and Ubuntu images**, resolved per architecture.
- **Placement groups** for spreading nodes across physical hosts.
- **Cost controls** — opt out of the billed public IPv4 (and/or IPv6) per node class for private-network clusters.
- **Multi-cluster safe** — every managed server is tagged with the cluster name, so several clusters can share one Hetzner project without touching each other's nodes.

## How it works

```
┌──────────────────────────────────────────────────────────┐
│ Karpenter core (provisioning, disruption, NodeClaim GC)    │
└───────────────┬────────────────────────────────────────────┘
                │ CloudProvider interface
┌───────────────▼────────────────────────────────────────────┐
│ karpenter-provider-hetzner                                  │
│  • instance      — hcloud server create/delete/get/list     │
│  • instancetype  — server types → priced InstanceTypes      │
│  • imagefamily   — resolve Talos/Ubuntu images per arch     │
│  • nodeclass ctrl— validate HCloudNodeClass, set Ready      │
└───────────────┬────────────────────────────────────────────┘
                │ hcloud API
        ┌───────▼────────┐
        │  Hetzner Cloud  │
        └─────────────────┘
```

A `NodePool` references an `HCloudNodeClass`. When pods are unschedulable, Karpenter asks this provider for instance types, picks the cheapest compatible offering, creates the server, and the node joins the cluster.

## Installation

You need a Hetzner Cloud API token with read/write access and a Kubernetes cluster on Hetzner (the [hcloud Cloud Controller Manager](https://github.com/hetznercloud/hcloud-cloud-controller-manager) should set node provider IDs as `hcloud://<id>`).

```bash
kubectl create secret generic hcloud-token \
  --namespace kube-system \
  --from-literal=token=$HCLOUD_TOKEN

helm install karpenter-provider-hetzner \
  oci://ghcr.io/stubbi/charts/karpenter-provider-hetzner \
  --namespace kube-system \
  --set clusterName=my-cluster \
  --set auth.secretRef.name=hcloud-token
```

`clusterName` is **required** — it scopes which servers this controller manages. The controller fails fast if it is unset.

Three CRDs ship in the chart's `crds/` directory and are installed automatically by Helm: `HCloudNodeClass` (this provider) plus the `NodePool` and `NodeClaim` core CRDs from `karpenter.sh`, which the controller watches. No separate CRD install step is needed.

> **Upgrading:** Helm only ever *installs* resources from `crds/`; it never updates them. When upgrading to a chart whose CRDs changed (this provider's `HCloudNodeClass`, or the karpenter core version), apply the CRDs yourself before `helm upgrade`; the release notes say when that is the case:
>
> ```bash
> VERSION=vX.Y.Z   # the release you are upgrading to
> BASE=https://raw.githubusercontent.com/stubbi/karpenter-provider-hetzner/$VERSION/charts/karpenter-provider-hetzner/crds
> kubectl apply --server-side --force-conflicts \
>   -f $BASE/karpenter.hetzner.cloud_hcloudnodeclasses.yaml \
>   -f $BASE/karpenter.sh_nodeclaims.yaml \
>   -f $BASE/karpenter.sh_nodepools.yaml
> ```
>
> `--force-conflicts` is needed because Helm created the CRDs and still owns their fields. If a GitOps tool manages your CRDs instead, update them there; it would revert a manual apply.

## Usage

Create an `HCloudNodeClass` describing how nodes are built, and a `NodePool` describing what Karpenter may provision:

```yaml
apiVersion: karpenter.hetzner.cloud/v1
kind: HCloudNodeClass
metadata:
  name: default
spec:
  locations: [nbg1, fsn1]          # Hetzner locations to schedule into
  networkID: 123456                # private network the nodes join
  imageSelector:
    family: talos                  # talos | ubuntu
    version: "v1.9"                # optional substring match
  firewallIDs: [987654]            # optional
  sshKeyIDs: [42]                  # optional
  placementGroupStrategy: spread   # spread | none (default spread)
  enablePublicIPv4: false          # default true; false saves the primary-IPv4 charge
  userData: |                      # cloud-init / Talos machine config (or use userDataSecretRef to source from a Secret)
    ...
---
apiVersion: karpenter.sh/v1
kind: NodePool
metadata:
  name: default
spec:
  template:
    spec:
      nodeClassRef:
        group: karpenter.hetzner.cloud
        kind: HCloudNodeClass
        name: default
      requirements:
        # Hetzner has no spot capacity. Pin on-demand, or Karpenter core sets spot on
        # every consolidation replacement and the replacement never launches.
        - key: karpenter.sh/capacity-type
          operator: In
          values: [on-demand]

        # Pinned to amd64 so this pool never picks an architecture your images may
        # not have. Add arm64 (and see examples/nodepool-multiarch.yaml) once your
        # images are published as multi-arch manifests.
        - key: kubernetes.io/arch
          operator: In
          values: [amd64]
        - key: karpenter.hetzner.cloud/server-family
          operator: In
          values: [cax, cpx]
  limits:
    cpu: "100"
  disruption:
    consolidationPolicy: WhenEmptyOrUnderutilized
```

### Node labels

Provisioned nodes carry, in addition to the well-known Karpenter labels:

| Label | Example | Meaning |
|-------|---------|---------|
| `topology.kubernetes.io/zone` | `nbg1` | Hetzner location |
| `node.kubernetes.io/instance-type` | `cax21` | Hetzner server type |
| `karpenter.hetzner.cloud/server-family` | `cax` | Type family (cx/cpx/cax/ccx) |
| `karpenter.hetzner.cloud/cpu-type` | `shared` | `shared` or `dedicated` |

## HCloudNodeClass reference

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `locations` | `[]string` | yes | — | Hetzner locations (min 1) |
| `networkID` | `int64` | yes | — | Private network ID nodes attach to |
| `imageSelector.family` | `talos`\|`ubuntu` | yes | — | OS image family |
| `imageSelector.version` | `string` | no | newest | Version substring to match against the image description |
| `imageSelector.selector` | `map[string]string` | no | — | hcloud label filter applied when listing images (e.g. `{"caph-image-name": "talos-v1.9.5-gvisor"}`). Prefer this over `version` to pin an exact snapshot (version + baked extensions). All labels must match; the provider guards against provisioning a node whose resolved image arch does not match the server type. |
| `firewallIDs` | `[]int64` | no | — | Firewalls to attach |
| `sshKeyIDs` | `[]int64` | no | — | SSH keys to install |
| `placementGroupStrategy` | `spread`\|`none` | no | `spread` | Placement-group behavior. `spread` puts each node pool's servers in spread placement groups the provider creates (`karpenter-<cluster>-<nodepool>`). Hetzner caps a spread group at 10 servers, so a larger pool rolls over to indexed groups (`…-1`, `…-2`), least-full first. **Spread holds only within a group**: nodes in different groups may share a host, so anti-affinity weakens gradually as a pool grows past 10. Provider-created groups are not deleted when a node pool goes away. `none` disables placement groups. |
| `placementGroupIDs` | `[]int64` | no | — | Existing spread placement groups (e.g. from Terraform) to use instead of provider-managed ones. Servers go into the least-full listed group; the provider never creates or deletes groups for this list, and a launch fails once every listed group holds 10 servers. Cannot be combined with `placementGroupStrategy: none`. |
| `enablePublicIPv4` | `*bool` | no | `true` | Attach a public IPv4 (billed separately by Hetzner). Set `false` on private-network clusters to drop the charge. |
| `enablePublicIPv6` | `*bool` | no | `true` | Attach a public IPv6. |
| `labels` | `map[string]string` | no | — | Extra hcloud labels on the Hetzner server (useful for cost attribution or firewall label-selectors) |
| `userData` | `string` | no | — | Inline cloud-init / Talos machine config. Overridden by `userDataSecretRef` when both are set. |
| `userDataSecretRef` | `object {namespace, name, key}` | no | — | Source `userData` from a Secret instead of inline. The Secret is read at server-create time; its value never appears in the NodeClass spec or git. Takes precedence over `userData`. |
| `kubelet` | `object {systemReserved, kubeReserved, evictionHard, maxPods}` | no | `kubeReserved: {cpu: 100m, memory: 100Mi}` | The reservations your `userData` configures on the kubelet, so Karpenter subtracts them from allocatable. Descriptive only, unless `bootstrap` is set: the provider does not configure the kubelet, so keep it in agreement with your bootstrap. With `bootstrap`, the provider renders these values into the kubelet flags and an empty block means the kubelet defaults. Without it (or with it empty) only the default shown is subtracted; once it declares anything, it is taken at its word. `evictionHard` takes a quantity or a percentage of capacity, without the kubelet flag's `<`; a signal you leave out is assumed at the kubelet default (`memory.available: 100Mi`, `nodefs.available: 10%`), so declare `"0%"` for one your kubelet does not enforce. To read the values off a running node on any distribution: `kubectl get --raw /api/v1/nodes/<node>/proxy/configz \| jq '.kubeletconfig \| {systemReserved, kubeReserved, evictionHard, maxPods}'`. `maxPods` (1-1024, default 110) is the pod capacity Karpenter advertises; set it to your kubelet's `maxPods`. It does not affect how reservations are read. |
| `bootstrap` | `object {distribution, server, tokenSecretRef{name,key}, version, extraConfig}` | no | — | Opt-in provider-rendered k3s/RKE2 agent join. The provider writes the agent config (server, token from a Secret in its own namespace, the `karpenter.sh/unregistered` taint, kubelet flags from `kubelet`) and installs the agent; `userData` becomes a merged `#cloud-config` extension. Not allowed with `family: talos`. See [docs/provider-bootstrap.md](docs/provider-bootstrap.md). |
| `vmMemoryOverheadPercent` | `string` (`0`-`49.99`) | no | — (advertised memory) | Share of the advertised memory the guest kernel never sees, removed from capacity before reservations. Measured on registered nodes the gap is about 6.9% (4 GB), 5.4% (8 GB), 4.7% (16 GB) and 4.4% (32 GB), so `"7"` keeps every size at or below what it registers with. Pick the value for the smallest type the node class can launch: a higher value only undersells larger types, a lower one oversells smaller ones. |

Why the default `kubeReserved` is a flat 100m/100Mi rather than a curve scaled by cores and memory: the kubelet reserves nothing by default, and what a node actually reserves is whatever its bootstrap writes into the kubelet config. A GKE/EKS-style tiered formula would be wrong for every bootstrap this provider supports, so the provider keeps the historical flat default (changing it would silently move every existing node's allocatable) and makes the real values declarable through `kubelet` instead.

Status exposes `conditions` (`ImagesReady`, `LocationsReady`, `NetworkReady`, `ResourcesReady`, `UserDataReady`, `BootstrapReady`, aggregated into `Ready`) and `resolvedImages` (image ID per architecture).

### Drift

A node is replaced (`Drifted`) when it no longer matches its NodeClass:

- **Observed on the server**: network, firewalls (every declared one attached), location (still allowed), labels (every declared one present) and server type are compared live, which also catches out-of-band changes.
- **Image**: compared against the image the NodeClass resolves now for the node's architecture, **only when `imageSelector` pins it** with `version` or `selector`. A bare selector means "the newest matching image", and following it would roll the whole fleet whenever someone uploads a matching snapshot, so it never drifts on image. To roll nodes onto a new image, pin the selector and change the pin.
- **Not observable on the server**: `userData`, `userDataSecretRef` (the reference, not the Secret's contents), `sshKeyIDs`, `placementGroupStrategy`, `enablePublicIPv4` and `enablePublicIPv6` are hashed. The hash is recorded on each NodeClaim at launch (`karpenter.hetzner.cloud/nodeclass-hash`), and an edit to any of these fields rolls nodes. Rotating the contents of a userData Secret does not; change the reference to roll deliberately. `kubelet` and `vmMemoryOverheadPercent` only describe nodes to the scheduler, so they do not roll nodes. With `bootstrap` set, the bootstrap block (including the token Secret *reference*, not its value) and `kubelet` are hashed too, because the provider renders them onto the node.

On upgrade to the first release with the hash, existing NodeClaims have no recorded hash. They are not treated as drifted. The provider records the current hash on them instead, so upgrading replaces nothing and the first NodeClass edit after that rolls them.

## Examples

Ready-to-apply examples live in the [`examples/`](examples/) directory.  Each
file is multi-document YAML (NodeClass + NodePool in one file) with inline
comments explaining every field.

| File | Description |
|------|-------------|
| [`examples/talos-nodeclass.yaml`](examples/talos-nodeclass.yaml) | Talos Linux, private-network cluster, image pinned via label selector, machineconfig from a Secret |
| [`examples/ubuntu-nodeclass.yaml`](examples/ubuntu-nodeclass.yaml) | Ubuntu 24.04, kubeadm join via inline cloud-init `userData` |
| [`examples/k3s-nodeclass.yaml`](examples/k3s-nodeclass.yaml) | k3s agent join via cloud-init on an Ubuntu image (for k3s-based clusters) |
| [`examples/k3s-bootstrap-nodeclass.yaml`](examples/k3s-bootstrap-nodeclass.yaml) | k3s agents with the provider-rendered bootstrap (`spec.bootstrap`): no hand-written join config |
| [`examples/nodepool-multiarch.yaml`](examples/nodepool-multiarch.yaml) | Multi-arch pattern: one NodeClass, two NodePools (amd64 CCX + arm64 CAX) |

### Bootstrap guides

- [Provider-rendered k3s / RKE2 bootstrap](docs/provider-bootstrap.md): `spec.bootstrap`, merge rules for `userData`, token handling.
- [Talos bootstrap guide](docs/talos-bootstrap.md) — obtaining the worker machineconfig, pinning images, and verifying node join.
- [Ubuntu bootstrap recipe](docs/ubuntu-bootstrap.md) — kubeadm join via cloud-init, keeping tokens out of git, trade-offs vs Talos.
- [k3s bootstrap recipe](docs/k3s-bootstrap.md) — k3s agent join via cloud-init, using the hcloud CCM for providerID; the natural path for k3s installers (hetzner-k3s, kube-hetzner).

## Configuration

| Env var | Required | Description |
|---------|----------|-------------|
| `HCLOUD_TOKEN` | yes | Hetzner Cloud API token |
| `CLUSTER_NAME` | yes | Cluster identifier; scopes managed servers |
| `METRICS_PORT` | no (8080) | Prometheus metrics port |
| `HEALTH_PROBE_PORT` | no (8081) | Health/readiness probe port |
| `ORPHAN_GC_ENABLED` | no (`true`) | Reclaim orphaned servers (chart: `orphanGC.enabled`); see below |
| `ORPHAN_GC_GRACE_PERIOD` | no (`30m`, min `15m`) | Minimum server age before it can be reclaimed (chart: `orphanGC.gracePeriod`) |

### Orphaned servers

A server can outlive its NodeClaim without ever being recorded on it: hcloud accepts the create, then the controller restarts (rolling update, leader handover, OOM) while the server boots, so no provider ID is written and core never deletes it. Two mechanisms close this:

- **Adoption.** Every server carries a `karpenter.sh/nodeclaim` label. Before creating, the provider looks for this cluster's server with that label and adopts it, and it does the same when hcloud rejects a create as a duplicate name. A retried create therefore never makes a second server.
- **Orphan reclaim.** Every 5 minutes the controller lists this cluster's managed servers and deletes a server only if it carries a `karpenter.sh/nodeclaim` label, is older than `ORPHAN_GC_GRACE_PERIOD`, its NodeClaim no longer exists, and no Node references its provider ID. Servers without the label are never touched. Reclaims are counted in `karpenter_hetzner_orphaned_servers_reclaimed_total`.

Both rely on `CLUSTER_NAME`: two clusters must never share one in a Hetzner project.

### API budget

Hetzner's hourly request budget is per **project** and shared with every client on the same token, including the cloud controller manager and the CSI driver, so when the provider exhausts it, load balancers and volume attaches fail too. The provider keeps its share small:

- Action polling (the wait while a server boots) backs off from 1s to a 5s ceiling instead of the SDK's constant 500ms, roughly 6–8 requests per create instead of ~40.
- Placement-group IDs are resolved once per node pool and cached; resolved images are cached for 5 minutes.

`karpenter_hetzner_hcloud_ratelimit_remaining`, `karpenter_hetzner_hcloud_ratelimit_limit` and `karpenter_hetzner_hcloud_ratelimit_reset_timestamp_seconds` mirror the `RateLimit-*` headers of the provider's own requests. Alert on `remaining` falling toward zero; it is the signal that points at the budget when other components start failing at the same time. `karpenter_hetzner_image_cache_total{result}` shows the image cache hit rate.

At startup the controller makes one authenticated read and exits with a clear error if Hetzner rejects `HCLOUD_TOKEN` (unauthorized or forbidden). A timeout or API outage only logs a warning, and the ongoing readiness probe does not depend on the Hetzner API. A read-only token passes this check because it can still read. It fails on the first create instead, with the reason shown below.

### Failed launches

How a failed server create is handled depends on what the hcloud error means:

| hcloud error | Handling |
|---|---|
| `resource_unavailable`, `invalid_server_type`, unsupported location | Capacity. That server type is quarantined in that location for 5 minutes, and Karpenter picks another shape. |
| `resource_limit_exceeded` | Project quota. Nothing is quarantined. The NodeClaim's `Launched` condition shows reason `ProjectQuotaExceeded`. |
| `unauthorized`, `forbidden`, `token_readonly` | Token problem. `Launched` shows reason `HCloudUnauthorized`, `HCloudForbidden` or `HCloudTokenReadOnly`. |
| `private_net_only_server` | NodeClass problem: firewalls need a public interface. `Launched` shows reason `PrivateNetOnlyServer`. |
| `no_subnet_available` | The NodeClass network has no subnet in that location's network zone. `Launched` shows reason `NoSubnetAvailable`. |
| anything else (rate limits, 5xx, `placement_error`, `uniqueness_error`) | Retried. |

Failed creates and deletes are counted by hcloud error code in `karpenter_hetzner_server_create_errors_total{code}` and `karpenter_hetzner_server_delete_errors_total{code}`. If a delete fails with `protected`, delete protection was turned on outside Karpenter. The NodeClaim stays terminating until someone removes it.

## Cost notes

- Pricing uses the **net** hourly figure, so relative comparisons match your Hetzner invoice (before VAT).
- Hetzner bills the primary IPv4 separately. On private-network clusters, set `enablePublicIPv4: false` to drop it.
- Hetzner has no spot capacity. Pin `karpenter.sh/capacity-type` to `on-demand` in every NodePool. Without the pin, Karpenter core sets `spot` on every consolidation replacement. No offering matches, and the replacement never launches. New nodes, `Empty` and `Drifted` still work, so the fault is easy to miss (#88).
- Selection is cheapest-first, so a NodePool that permits several architectures or families gets whichever priced offering is cheapest for the requested shape — which may be ARM (CAX). Constrain `kubernetes.io/arch` or `server-family` to steer Karpenter, and publish multi-arch images before allowing arm64.

## Development

See [CONTRIBUTING.md](CONTRIBUTING.md). Quick start:

```bash
make test       # unit + controller tests (race)
make lint       # golangci-lint
make generate   # regenerate CRD + deepcopy
make build      # build the controller binary
```

## Contributing

Contributions are welcome — please read [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md).

## Security

To report a vulnerability, see [SECURITY.md](SECURITY.md). Please do not open public issues for security reports.

## License

[Apache 2.0](LICENSE).
