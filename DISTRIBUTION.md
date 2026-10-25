# Distribution checklist

Steps to make `karpenter-provider-hetzner` discoverable and drive adoption now
that the signed image and OCI Helm chart are published.

**Canonical link:** every external listing's primary/Homepage link points at the
GitHub repository, `https://github.com/stubbi/karpenter-provider-hetzner`. The chart `home` and the Artifact Hub
"Homepage" link already point there.

Legend: [auto] doable from a repo · [acct] needs an external account/login ·
[ext-pr] needs a PR to an external repo.

## Artifact Hub  [acct]
- Register the OCI chart repo at https://artifacthub.io/control-panel/repositories
  (Add repository -> Helm charts -> OCI -> `oci://ghcr.io/stubbi/charts`).
- Paste the generated `repositoryID` into `artifacthub-repo.yml`, re-push it to
  the OCI registry. The chart's `artifacthub.io/*` annotations already set the
  Homepage link to the GitHub repository.

## karpenter-core community providers docs  [ext-pr]
- PR to `kubernetes-sigs/karpenter` adding Hetzner to the community/third-party
  providers list, linking the GitHub repository.

## awesome-karpenter  [ext-pr]
- PR to the `awesome-karpenter` providers section, linking the GitHub repository.

## CNCF landscape  [ext-pr]
- PR to `cncf/landscape` under the autoscaling category (homepage_url and
  repo_url -> GitHub, license Apache-2.0).

## Hetzner community  [acct]
- Tutorial on the Hetzner Community portal: "Autoscale a Talos k8s cluster on
  Hetzner with Karpenter", linking the GitHub repository.

## README badges  [auto]
- Add the Artifact Hub badge once the listing exists.
