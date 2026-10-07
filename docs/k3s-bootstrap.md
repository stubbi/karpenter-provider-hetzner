# k3s bootstrap recipe

A concise guide to joining [k3s](https://k3s.io) agent (worker) nodes with
karpenter-provider-hetzner.

See [`examples/k3s-nodeclass.yaml`](../examples/k3s-nodeclass.yaml) for an
apply-ready NodeClass and NodePool with a full cloud-init template.

> Karpenter is **bootstrap-agnostic** — Hetzner runs the `userData` string as
> cloud-init on first boot, so the same provider that joins kubeadm or Talos
> nodes joins k3s agents. Only the cloud-init contents differ.

---

## How it works

The cloud-init in `examples/k3s-nodeclass.yaml`:

1. Writes `/etc/rancher/k3s/config.yaml` with `kubelet-arg: cloud-provider=external`
   so the hcloud cloud controller manager (not k3s) owns the node lifecycle and
   sets the `hcloud://<id>` providerID Karpenter relies on.
2. Runs the official k3s installer in **agent** mode, pointing at the server's
   `:6443` endpoint with the cluster's node-join token.

The node appears in `kubectl get nodes` within two to three minutes of the
Hetzner server becoming active.

---

## The Karpenter registration contract

Because this is a recipe rather than provider-rendered bootstrap, one piece of
Karpenter's contract has to be written by you. This section documents it
explicitly; earlier revisions of this guide did not, which is a documentation
bug rather than a design choice (see #51).

### What the provider already handles — do not render these yourself

The node labels Karpenter's scheduler assumed when it picked the server type —
`karpenter.sh/nodepool`, `node.kubernetes.io/instance-type`,
`karpenter.sh/capacity-type`, `topology.kubernetes.io/zone` — are set on the
**NodeClaim** by this provider and copied onto the Node by karpenter core's
registration controller. Rendering them into cloud-init is unnecessary, and
rendering them *wrongly* is worse than omitting them.

One caveat: hcloud-cloud-controller-manager writes
`topology.kubernetes.io/zone` at node initialization, and if it wins that race
it sets the Hetzner **datacenter** (`nbg1-dc3`) where Karpenter's offerings are
keyed on the **location** (`nbg1`). A node whose zone label is a datacenter
prices at zero and can never be consolidated. Set
`HCLOUD_INSTANCES_ZONE_LABEL_ENABLED=false` on the CCM (v1.35.0+) to leave the
label to Karpenter.

### What you must render — the unregistered taint

Karpenter expects a new node to come up carrying:

```
karpenter.sh/unregistered=:NoExecute
```

Core removes it once it has finished syncing the NodeClaim's labels, taints and
owner references onto the Node. Its purpose is to stop pods landing on a node in
the window before that sync completes.

**A missing taint does not fail loudly.** Core logs an error, emits an
`UnregisteredTaintMissing` event on the NodeClaim, and proceeds — so the node
joins and works, and the only symptom is pods occasionally scheduling onto a
node whose labels and taints Karpenter has not applied yet. That is a race, so
it will not reproduce on demand.

In k3s, set it in `/etc/rancher/k3s/config.yaml`:

```yaml
node-taint:
  - "karpenter.sh/unregistered=:NoExecute"
```

You do **not** need to declare it in the NodePool's `startupTaints`: core
removes this particular taint unconditionally at the end of registration,
whether or not the NodePool mentions it.

### Kubelet reservations

If your k3s agents set `kube-reserved` / `system-reserved` / `eviction-hard`,
declare the same values under the NodeClass's `spec.kubelet`. Without it the
provider assumes a flat 100m/100Mi `kubeReserved`, the scheduler's view of the
node and the kubelet's disagree, and pods are placed on nodes that cannot admit
them.

Give each `eviction-hard` value without the `<` (`memory.available: 400Mi`). A
signal you leave out is assumed at the kubelet default, while a stock kubelet
given any `eviction-hard` enforces only the signals listed, so list every
signal you set, and declare `"0%"` for any you want counted as zero.

If you do not pass `eviction-hard` at all, k3s still sets its own:
`imagefs.available<5%,nodefs.available<5%`, with no memory threshold. Declare
that explicitly, or the kubelet's 100Mi/10% defaults are assumed instead:

```yaml
kubelet:
  evictionHard: {memory.available: "0%", nodefs.available: 5%, imagefs.available: 5%}
```

When in doubt, read the running values off a node, as the README describes.

```yaml
kubelet:
  systemReserved: {cpu: 200m, memory: 512Mi}
  kubeReserved: {cpu: 200m, memory: 512Mi}
  evictionHard: {memory.available: 400Mi, nodefs.available: 10%}
```

with the agent flag `eviction-hard=memory.available<400Mi,nodefs.available<10%`.

---

## Prerequisites

| Requirement | Why |
|--|--|
| k3s server reachable on `:6443` | The agent registers against the supervisor/API port. |
| k3s node-join token | Authenticates the agent. `sudo cat /var/lib/rancher/k3s/server/node-token` on a server. |
| hcloud-cloud-controller-manager installed | Assigns `providerID: hcloud://<id>`, which Karpenter uses to map a NodeClaim to its Node. |
| k3s servers started with `--disable-cloud-controller` | Hands node lifecycle to the hcloud CCM instead of k3s's built-in one. |

> **Name matching:** the hcloud CCM matches a Node to a Hetzner server by name.
> Hetzner sets the node hostname to the server name (which the provider
> controls), so this matches out of the box. If you override `node-name`, keep
> it equal to the server name.

---

## Step 1: get the node-join token

On an existing k3s server:

```bash
sudo cat /var/lib/rancher/k3s/server/node-token
# K10<hash>::server:<secret>
```

This token is long-lived. Treat it as a cluster credential — see "Keeping
secrets out of git" below.

---

## Step 2: fill in the NodeClass

Replace the placeholders in `examples/k3s-nodeclass.yaml`:

```yaml
userData: |
  #cloud-config
  runcmd:
    - |
      curl -sfL https://get.k3s.io | \
        K3S_URL="https://<CONTROL_PLANE_ENDPOINT>:6443" \
        K3S_TOKEN="<NODE_TOKEN>" \
        INSTALL_K3S_VERSION="v1.31.5+k3s1" \
        sh -s - agent
```

Pin `INSTALL_K3S_VERSION` to match your server's k3s version (`k3s --version`)
so agents don't run a newer minor than the control plane.

### Private-network data plane

If your cluster routes pod/node traffic over the Hetzner private network,
uncomment `node-ip` / `flannel-iface` in the NodeClass `config.yaml` block and
set them to the node's private IP and NIC (commonly `enp7s0` on Hetzner). This
keeps kubelet and flannel off the public interface.

### Keeping secrets out of git — and off the node

The node-join token grants cluster membership. Store the full cloud-init blob in
a Secret and reference it with `userDataSecretRef` instead of inlining it:

```bash
kubectl create secret generic k3s-worker-userdata \
  --namespace kube-system \
  --from-file=cloud-init.yaml=/path/to/cloud-init.yaml
```

```yaml
# In the NodeClass spec — remove userData and add:
userDataSecretRef:
  namespace: kube-system
  name: k3s-worker-userdata
  key: cloud-init.yaml
```

`userDataSecretRef` takes precedence over `userData` when both are set.

**`userDataSecretRef` keeps the token out of git and out of the NodeClass — it
does not keep it off the node.** Hetzner serves userData from the instance
metadata service, which is reachable from inside the server:

```bash
curl http://169.254.169.254/hetzner/v1/userdata
```

So anything with code execution on any Karpenter-provisioned node can read the
token that cloud-init joined with. Any workload that can reach link-local from a
pod's network namespace can too, unless you block it.

Two things follow:

1. **Use a scoped agent token, not the server node-token.** Start your k3s
   servers with `--agent-token <secret>` and join agents with that. The
   `node-token` read from `/var/lib/rancher/k3s/server/node-token` also permits
   joining as a *server*, so leaking it is a control-plane compromise rather
   than a worker one. An agent token limits the blast radius to what a worker
   can already do.
2. **Block link-local egress from pods**, e.g. a NetworkPolicy or firewall rule
   denying `169.254.169.254/32`. This is worth doing regardless of Karpenter —
   the metadata service carries the userData of every bootstrap method, not just
   this one.

Provider-rendered bootstrap would not remove this exposure either; cloud-init
has to read the token from somewhere the node can see. Scoping the token is the
mitigation that actually helps.

---

## Step 3: apply and verify

```bash
kubectl apply -f examples/k3s-nodeclass.yaml

# NodeClass should report Ready:
kubectl get hcloudnodeclasses k3s-default

# Watch for new agents joining:
kubectl get nodes -w
```

If a node does not join within five minutes, SSH in (while `enablePublicIPv4`
is `true`, or via the Hetzner console) and inspect the logs:

```bash
cat /var/log/cloud-init-output.log
journalctl -u k3s-agent --no-pager | tail -40
```

A node stuck with the `node.cloudprovider.kubernetes.io/uninitialized` taint
means the hcloud CCM has not initialized it — check the CCM is running and that
the node hostname matches the Hetzner server name.

---

## Trade-offs vs kubeadm and Talos

| | k3s | Ubuntu + kubeadm | Talos |
|--|--|--|--|
| Custom image required | No (Ubuntu public image) | No | Yes |
| Install complexity | Lowest (single installer) | Medium | Medium (machineconfig) |
| Footprint | Small (single binary) | Full kubeadm stack | Minimal immutable OS |
| Bootstrap secret | Long-lived node-token | Short-lived kubeadm token | Machineconfig CA + token |
| Best fit | Edge, small/medium clusters, k3s shops | Mixed/general workloads | Production, security-focused |

For k3s-based installers (e.g. hetzner-k3s, kube-hetzner) this is the natural
path; reuse your existing cluster's k3s token and server endpoint.
