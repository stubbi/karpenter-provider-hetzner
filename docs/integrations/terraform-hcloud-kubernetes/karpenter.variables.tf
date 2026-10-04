# karpenter.variables.tf — PROPOSAL variables for the Karpenter integration.
# Append to the module's variables.tf (kept separate here for review clarity).

variable "karpenter_helm_repository" {
  type        = string
  default     = "oci://ghcr.io/stubbi/charts"
  description = "OCI repository hosting the karpenter-provider-hetzner chart."
}

variable "karpenter_helm_chart" {
  type        = string
  default     = "karpenter-provider-hetzner"
  description = "Helm chart name for karpenter-provider-hetzner."
}

variable "karpenter_helm_version" {
  type        = string
  default     = "2.0.0"
  description = "Chart version (ships the v1 HCloudNodeClass CRD)."
}

variable "karpenter_image_tag" {
  type        = string
  default     = ""
  description = "Override the controller image tag; empty uses the chart appVersion."
}

variable "karpenter_helm_values" {
  type        = any
  default     = {}
  description = "Extra Helm values merged over the module defaults."
}

# Defining at least one nodepool enables the integration. Empty (default) keeps
# Karpenter off, so existing clusters are unaffected.
variable "karpenter_nodepools" {
  type = list(object({
    name            = string
    locations       = list(string) # Hetzner zones, e.g. ["nbg1","fsn1"]
    architectures   = optional(list(string), ["amd64"])
    server_families = list(string) # e.g. ["cpx"], ["cax"], ["ccx"]
    labels          = optional(map(string), {})
    taints = optional(list(object({
      key    = string
      value  = optional(string)
      effect = string
    })), [])
    limits               = optional(map(string), { cpu = "100" })
    consolidation_policy = optional(string, "WhenEmptyOrUnderutilized")
    consolidate_after    = optional(string, "30s")
  }))
  default     = []
  description = "Karpenter NodePools. Empty list disables the integration."

  validation {
    condition     = length(var.karpenter_nodepools) == length(distinct([for np in var.karpenter_nodepools : np.name]))
    error_message = "karpenter_nodepools names must be unique."
  }

  validation {
    condition     = alltrue([for np in var.karpenter_nodepools : alltrue([for a in np.architectures : contains(["amd64", "arm64"], a)])])
    error_message = "architectures entries must be amd64 or arm64."
  }
}

# Mutual-exclusion guard: Karpenter and the Cluster Autoscaler both want to own
# node provisioning. Running both invites churn/fighting, so fail fast.
# (Place alongside the other validations, or as a check {} block.)
#   condition = !(local.karpenter_enabled && local.cluster_autoscaler_enabled)
