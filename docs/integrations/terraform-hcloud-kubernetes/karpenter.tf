# karpenter.tf — PROPOSAL for hcloud-k8s/terraform-hcloud-kubernetes
#
# Adds karpenter-provider-hetzner as an OPT-IN alternative to the existing
# Cluster Autoscaler. It is disabled by default (length(var.karpenter_nodepools)
# == 0), so it changes nothing for current users. It deliberately mirrors the
# structure of cluster_autoscaler.tf:
#   1. render the provider Helm chart with `data.helm_template`
#   2. generate a Talos *worker* machineconfig (reusing talos_base_config_patches)
#      and ship it as a Secret that the HCloudNodeClass sources via
#      userDataSecretRef — Karpenter passes it verbatim as the server's userData,
#      which Talos consumes as machine config on first boot
#   3. emit HCloudNodeClass + NodePool manifests
#   4. expose everything as `local.karpenter_manifest` for talos_inline_manifests
#
# WIRING (one line, in talos_config_control_plane.tf):
#   talos_inline_manifests = concat(
#     ...
#     local.cluster_autoscaler_manifest != null ? [local.cluster_autoscaler_manifest] : [],
#     local.karpenter_manifest          != null ? [local.karpenter_manifest]          : [],   # <-- add
#     ...
#   )
#
# NOTE: this file is a starting point. It references existing locals/resources in
# the module (hcloud_ssh_key.this, local.firewall_id, hcloud_network_subnet.*,
# local.talos_base_config_patches, local.image_label_selector, var.cluster_name,
# var.kubernetes_version, var.talos_version, local.kube_api_url_internal). Run
# `terraform validate` / `plan` against the module before opening the PR — the
# author has not been able to plan it in isolation.

locals {
  karpenter_enabled      = length(var.karpenter_nodepools) > 0
  karpenter_release_name = "karpenter-provider-hetzner"
  karpenter_namespace    = "kube-system"

  # One shared HCloudNodeClass; NodePools (one per var entry) reference it and
  # constrain server-family/arch/zone. Karpenter then bin-packs the cheapest
  # compatible Hetzner type per pending pod.
  karpenter_nodeclass_name = "default"

  # The provider attaches nodes to the cluster's worker network so the Talos
  # data plane and hcloud CCM see them on the private subnet.
  karpenter_network_id = hcloud_network_subnet.worker_shared.network_id

  karpenter_nodeclass_manifest = local.karpenter_enabled ? {
    apiVersion = "karpenter.hetzner.cloud/v1"
    kind       = "HCloudNodeClass"
    metadata = {
      name = local.karpenter_nodeclass_name
    }
    spec = merge(
      {
        # Schedule into every location any pool may use.
        locations = distinct(flatten([for np in var.karpenter_nodepools : np.locations]))
        networkID = tonumber(local.karpenter_network_id)
        imageSelector = {
          family = "talos"
          # Reuse the module's image label selector so Karpenter resolves the
          # SAME Talos snapshot the static workers boot from, per architecture.
          selector = local.image_label_selector
        }
        placementGroupStrategy = "spread"
        enablePublicIPv4       = var.talos_public_ipv4_enabled
        enablePublicIPv6       = var.talos_public_ipv6_enabled
        userDataSecretRef = {
          namespace = local.karpenter_namespace
          name      = local.karpenter_machineconfig_secret_name
          key       = "worker.yaml"
        }
      },
      local.firewall_id == null ? {} : { firewallIDs = [tonumber(local.firewall_id)] },
    )
  } : null

  # Talos worker machineconfig for Karpenter-provisioned nodes. Identical
  # generation path to data.talos_machine_configuration.cluster_autoscaler, so
  # these nodes are configured exactly like the rest of the worker fleet.
  karpenter_machineconfig_secret_name = "karpenter-worker-machineconfig"

  karpenter_machineconfig_manifest = local.karpenter_enabled ? {
    apiVersion = "v1"
    kind       = "Secret"
    type       = "Opaque"
    metadata = {
      name      = local.karpenter_machineconfig_secret_name
      namespace = local.karpenter_namespace
    }
    stringData = {
      "worker.yaml" = data.talos_machine_configuration.karpenter[0].machine_configuration
    }
  } : null

  karpenter_nodepool_manifests = [
    for np in var.karpenter_nodepools : {
      apiVersion = "karpenter.sh/v1"
      kind       = "NodePool"
      metadata = {
        name   = np.name
        labels = np.labels
      }
      spec = {
        template = {
          spec = {
            nodeClassRef = {
              group = "karpenter.hetzner.cloud"
              kind  = "HCloudNodeClass"
              name  = local.karpenter_nodeclass_name
            }
            requirements = concat(
              [
                {
                  key      = "kubernetes.io/arch"
                  operator = "In"
                  values   = np.architectures
                },
                {
                  key      = "karpenter.hetzner.cloud/server-family"
                  operator = "In"
                  values   = np.server_families
                },
                {
                  key      = "topology.kubernetes.io/zone"
                  operator = "In"
                  values   = np.locations
                },
                {
                  # Hetzner has no spot capacity. Pin on-demand, or Karpenter core sets
                  # spot on every consolidation replacement and it never launches.
                  key      = "karpenter.sh/capacity-type"
                  operator = "In"
                  values   = ["on-demand"]
                },
              ],
            )
            taints = np.taints
          }
        }
        limits = np.limits
        disruption = {
          consolidationPolicy = np.consolidation_policy
          consolidateAfter    = np.consolidate_after
        }
      }
    }
  ]
}

# Worker machineconfig generation — mirrors talos_config_cluster_autoscaler.tf.
data "talos_machine_configuration" "karpenter" {
  count = local.karpenter_enabled ? 1 : 0

  talos_version      = var.talos_version
  cluster_name       = var.cluster_name
  cluster_endpoint   = local.kube_api_url_internal
  kubernetes_version = var.kubernetes_version
  machine_type       = "worker"
  machine_secrets    = talos_machine_secrets.this.machine_secrets
  docs               = false
  examples           = false

  config_patches = [for patch in local.talos_base_config_patches : yamlencode(patch)]
}

# Render the provider's Helm chart (OCI). Token comes from the existing `hcloud`
# secret (same one the Cluster Autoscaler consumes).
data "helm_template" "karpenter" {
  count = local.karpenter_enabled ? 1 : 0

  name      = local.karpenter_release_name
  namespace = local.karpenter_namespace

  repository   = var.karpenter_helm_repository
  chart        = var.karpenter_helm_chart
  version      = var.karpenter_helm_version
  kube_version = var.kubernetes_version

  values = [
    yamlencode({
      clusterName = var.cluster_name
      replicas    = local.control_plane_sum > 1 ? 2 : 1
      image       = { tag = var.karpenter_image_tag }
      auth = {
        secretRef = {
          name = "hcloud"
          key  = "token"
        }
      }
      nodeSelector = { "node-role.kubernetes.io/control-plane" : "" }
      tolerations = [{
        key      = "node-role.kubernetes.io/control-plane"
        effect   = "NoSchedule"
        operator = "Exists"
      }]
    }),
    yamlencode(var.karpenter_helm_values),
  ]

  depends_on = [
    terraform_data.amd64_image,
    terraform_data.arm64_image,
  ]
}

locals {
  karpenter_manifest = local.karpenter_enabled ? {
    name     = "karpenter-provider-hetzner"
    contents = <<-EOF
      ${data.helm_template.karpenter[0].manifest}
      ---
      ${yamlencode(local.karpenter_machineconfig_manifest)}
      ---
      ${yamlencode(local.karpenter_nodeclass_manifest)}
      ${join("\n", [for m in local.karpenter_nodepool_manifests : "---\n${yamlencode(m)}"])}
    EOF
  } : null
}
