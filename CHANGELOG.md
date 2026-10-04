# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [3.0.1] - 2026-10-03

### Fixed
- The release workflow publishes the chart again. `v3.0.0` was tagged and its image was published and signed, but the SBOM step then failed while attaching the SBOM to the GitHub release with the workflow's read-only token, and the chart publish step after it never ran. The step no longer uploads to the release; the SBOM stays a workflow artifact and an in-registry attestation, as in every earlier release. **The 3.0.0 chart was never published. Use 3.0.1**, which is otherwise identical to 3.0.0.

## [3.0.0] - 2026-10-03

### Changed
- **BREAKING** — the image and the OCI chart moved registries. The repository now lives at `github.com/stubbi/karpenter-provider-hetzner`, so releases publish to `ghcr.io/stubbi/karpenter-provider-hetzner` and `oci://ghcr.io/stubbi/charts/karpenter-provider-hetzner`, and the chart's `image.repository` default points there. Releases up to 2.2.0 stay at `ghcr.io/paperclipinc` and nothing newer is published there, so `helm upgrade` has to name the new chart URL. A release that sets `image.repository` itself keeps its own value.
- Every NodePool in the README, `examples/` and the Terraform integration pins `karpenter.sh/capacity-type` to `on-demand`. Karpenter core sets `spot` on every consolidation replacement when a NodePool allows both capacity types. This provider has on-demand offerings only, so such a replacement never launched and the disruption controller retried without end (#88).
- The k3s guide recommends a scoped k3s agent token (`--agent-token`) over the server `node-token`, and documents that `userDataSecretRef` keeps a token out of git and out of the NodeClass but **not off the node** — Hetzner serves userData from the instance metadata service at `169.254.169.254`, readable from inside the server (#51).
- **BREAKING (chart 3.0.0)** — chart resource names and label selectors are scoped to the Helm release. Every object previously carried the hardcoded name `karpenter-provider-hetzner` and every selector matched on `app.kubernetes.io/name` alone, so two releases in one namespace could not coexist: Helm refuses to adopt an object owned by another release, and even past that the two Deployments' selectors each matched both releases' pods, so two ReplicaSet controllers reconciled the same pods against different desired states. The Service, PodDisruptionBudget and ServiceMonitor selectors had the identical omission. Every selector now carries `app.kubernetes.io/instance` (#73).
- **BREAKING (chart 3.0.0)** — `helm upgrade` from 2.x fails until the existing Deployment is deleted, because `.spec.selector` is immutable on `apps/v1`. See the chart README's *Upgrading to chart 3.0.0* for the `--cascade=orphan` procedure that keeps the controller running through the swap (#73).
- **BREAKING (chart 3.0.0)** — the cluster-scoped `ClusterRole` and `ClusterRoleBinding` names now include the release namespace. They are not namespaced, so two releases in *different* namespaces collided on them just as two in one namespace collided on the Deployment. Helm creates the new pair and removes the old one; no manual step (#73).
- **BREAKING (chart 3.0.0)** — `serviceAccount.name` defaults to the release-scoped name instead of `karpenter`. That old default is also what the upstream karpenter chart creates, so the two charts in one namespace silently shared one ServiceAccount and either release's uninstall revoked the other's API access. Set `serviceAccount.name=karpenter` to keep the previous name (#73).

### Fixed
- The chart's `ClusterRole` now grants everything Karpenter core requires. It was missing `pods` `delete`, which core uses to force-delete pods once a node's termination grace period expires. Without it, NodeRepair and NodePool `terminationGracePeriod` tainted the node and then looped forever on `cannot delete resource "pods"`, so a dead node was never removed. Also added: `nodeoverlays` and `capacitybuffers` (for the NodeOverlay and CapacityBuffer feature gates), `podtemplates`, and the `nodeclaims`/`nodepools` finalizer subresources (for clusters running the `OwnerReferencesPermissionEnforcement` admission plugin). This matches the rules in the upstream karpenter chart.
- The hcloud API client now bounds every HTTP request. `hcloud.NewClient` defaults to a bare `&http.Client{}`, which has no `Timeout`, so a connection that is accepted and never answered — a blackholed route to `api.hetzner.cloud`, a middlebox that swallows the response, a stalled TLS handshake — parked the calling goroutine forever. Every caller is a controller-runtime worker and controller-runtime imposes no per-reconcile deadline, so each hung request permanently consumed a worker while the operator kept reporting healthy. The client now carries a 30s per-request timeout plus explicit dial, TLS-handshake and response-header bounds, which also turns a hang into a `net.Error` the SDK's existing retry policy already treats as retryable (#71).
- `Delete` refuses a server that does not carry `karpenter.sh/managed-by=karpenter` and `karpenter.sh/cluster=<clusterName>` — the labels `List` selects on and every server this provider creates has carried since 1.0. The API token can delete any server in the project, and one project commonly holds several clusters plus servers managed by Terraform or by hand, so a provider ID that points at the wrong server (a hand-edited NodeClaim, a restored backup, a second cluster sharing the project) deleted it. The refusal is a plain error, not `NodeClaimNotFound`, so the NodeClaim stays terminating and visible instead of being dropped. (#91)
- The instance-type catalogue refresh no longer holds the cache lock across the hcloud API call. The catalogue is read on every provisioning decision, so a single slow or hung `GET /server_types` stalled every caller — including the overwhelming majority whose cache was warm and who needed no network at all. From the outside that looks like Karpenter having stopped rather than like an API problem. Refreshes are still serialized, by a separate mutex that only refreshers take, so a burst of concurrent misses still makes one API call rather than one per caller (#72).
- `HCloudNodeClass` now validates declared locations against the live hcloud catalogue and requires the referenced network to have a subnet in every location's network zone. Invalid NodeClasses that previously reported `Ready=True` now report `LocationsReady=False`; API failures report `Unknown` (#67).
- A failed catalogue refresh now serves the previously fetched catalogue instead of returning an error. Hetzner's server-type catalogue changes on the order of years, so a six-hour-old copy is not meaningfully less correct than a fresh one — while an error fails `Create` and `GetInstanceTypes` and stops the cluster provisioning at all. A transient 5xx should not be able to do that. The expiry is not extended, so the next call retries the API rather than settling into the stale copy. Offering availability is unaffected either way: it is computed live from the capacity cache on every call, never baked into the catalogue (#72).
- The k3s bootstrap guide and example now render the `karpenter.sh/unregistered=:NoExecute` taint. Karpenter expects a new node to carry it and removes it once core has synced the NodeClaim's labels, taints and owner references onto the Node; the guide never mentioned it. A missing taint does not fail loudly — core logs an error, emits an `UnregisteredTaintMissing` event, and proceeds — so the symptom was pods occasionally landing on a node Karpenter had not finished syncing, which is a race and does not reproduce on demand. The guide also now states which labels the provider already handles, so nobody hand-renders them wrongly (#51).
- Offerings in a location where hcloud has retired the server type are marked unavailable. hcloud keeps pricing a type after retiring it in a location (`cpx11`–`cpx51` in `fsn1`/`nbg1`/`hel1` since 2026-01-01) and refuses every create there with `unsupported location for server type`. Under cheapest-first those offerings were picked first, and each attempt cost a failed create plus a 5-minute unavailable-cache quarantine that expires before the next burst. The per-location deprecation with a past `unavailable_after` is a published date, unlike the `Locations[].Available` stock flag, so it is safe to act on up front. The offerings stay listed, so running nodes of those types are not drifted. (#92)

### Added
- `karpenter_hetzner_server_create_errors_total{code}` counts failed server creates by hcloud error code (`other` when the error carries none). Quota exhaustion (`resource_limit_exceeded`) is mapped to `InsufficientCapacityError` just like a type being out of stock (`resource_unavailable`), so Karpenter routes around both quietly and `server_create_total{result="error"}` cannot tell them apart. A quota alert can now fire on its own. (#94)
- `HCLOUD_API_TIMEOUT` (chart: `hcloud.apiTimeout`, default `30s`, max `5m`) tunes that per-request timeout. It bounds a single HTTP request, not a whole operation: the SDK's action waiter polls `/actions` in a loop of separate requests, so a server create that legitimately takes minutes is many short requests. An unparseable or out-of-range value fails startup rather than silently falling back (#71).
- `karpenter_hetzner_instance_type_cache_total{result="stale"}` counts refreshes that failed and fell back to the previous catalogue. Nothing else surfaces this — serving stale keeps provisioning working, so the only symptom would be a catalogue that quietly stops tracking hcloud (#72).
- The `ubuntu` image family resolves labelled snapshots. It only listed system images, which carry no labels, so `imageSelector.selector` could never match a custom (e.g. Packer-built) Ubuntu snapshot. With a selector set and no matching system image, the newest snapshot matching the selector (and `version`, if set) is used — the same rule the `talos` family already applies. Without a selector, behaviour is unchanged. (#93)
- Chart `nameOverride` and `fullnameOverride`. A release named after the chart — the documented install — keeps every namespaced object name unchanged; `fullnameOverride: karpenter-provider-hetzner` pins the old names for any other release name (#73).
- CI renders the chart. The chart had no CI at all, which is how #73 shipped: `helm lint`, the documented install, every optional resource, and an assertion that two releases in one namespace render neither the same names nor the same selectors. The last one fails against the chart as it stands on `main` (#73).

## [2.2.0] - 2026-09-02

### Changed
- Server type selection now launches the cheapest compatible offering instead of the first type the hcloud API happened to return. Karpenter core already ranks the instance types it sends by price; the provider iterated them in hcloud's own order (ascending server-type id), which puts the pricier CPX family ahead of CX and could buy a type several times the cost of an identically sized alternative. Selection and launch now derive from a single offering, so the location a server is created in — and the `topology.kubernetes.io/zone` label stamped on the node — always match the offering whose price it was ranked on (#50).
- **Check your NodePools before upgrading.** A NodePool that does not constrain `kubernetes.io/arch` may now get arm64 (CAX) nodes wherever an ARM offering prices below the amd64 ones it permits — the old hcloud-id ordering produced amd64 incidentally, not by policy. Pods with amd64-only images and no arch `nodeSelector` fail on such nodes with `exec format error`. Pin `kubernetes.io/arch: [amd64]` on those NodePools to keep the previous behaviour. The architecture remains the pool's choice, not the provider's: Karpenter core decides which instance types are eligible for the pending pods, and the provider launches the cheapest of the ones core sent (#50).

### Fixed
- Offerings whose hcloud pricing carries neither a usable hourly nor a usable monthly net figure are marked unavailable rather than priced at zero. A zero price sorts as the best deal available, so one malformed pricing entry would have won every selection under cheapest-first. They stay listed in the catalogue: karpenter core treats an offering that disappears as drift and would replace every healthy node of that type in that location (#50).
- Instance-type selection is deterministic when several types tie on price. Karpenter core's `OrderByPrice` sorts with an unstable sort and defines no tiebreak, so tied types were ordered arbitrarily and two NodeClaims from one NodePool could land on different shapes; ties now break on type name (#50).
- `Create` no longer dead-ends on an instance type whose architecture the `HCloudNodeClass` has no image for. The image is resolved after the type is chosen, so a miss returned a hard error without demoting the type or marking it unavailable, and Karpenter core requeued the same candidate indefinitely. Candidate types are now filtered against `status.resolvedImages`, which the nodeclass controller already populates per architecture — a single-arch cluster is explicitly supported there, and cheapest-first made that combination reachable by default. When no architecture has an image, the error now names the missing image and architecture instead of reporting `InsufficientCapacityError`, which sent operators to their Hetzner quotas for a problem in the NodeClass (#50).
- Image resolution now distinguishes a catalogue that answers "no such image" from one that could not be read at all. A 429 or 5xx used to clear `status.resolvedImages` and report `ImagesReady=False` — which, now that instance-type selection reads that list, would make Karpenter delete NodeClaims over an API blip. An architecture whose lookup fails transiently keeps its last known good image ID and the condition goes `Unknown`; only a readable catalogue with no match clears an entry. Preserved IDs are dropped as soon as `metadata.generation` moves, so repinning `imageSelector` can never keep launching the previous image. An architecture proven absent is reported and dropped even when a different architecture failed transiently in the same pass, so a deleted image is not hidden behind an unrelated 503 (#50).
- The nodeclass controller verifies an image's architecture before recording it in `status.resolvedImages`. `Create` launches the recorded ID and takes the architecture from the NodeClaim, so an unverified entry boots a node that fails every workload with `exec format error`. The same check now guards `Create`'s live-lookup fallback; the one it previously ran there compared the architecture against itself and could never fire (#50).

### Added
- `karpenter_hetzner_image_resolution_errors_total` counts image lookups that failed without proving the image absent. Those keep the affected architecture on its previously resolved ID and leave the NodeClass `Ready`, so nothing else surfaces them (#50).
- `karpenter_hetzner_instance_type_selection_skipped_total{arch,reason}` counts launches whose instance-type selection had to pass over an architecture — one per architecture per launch, so the rate tracks how often the provider routes around a missing image rather than how many server types Hetzner publishes (#50).

## [2.1.1] - 2026-08-26

### Fixed
- Chart RBAC grants event `create`/`patch` on the `events.k8s.io` API group alongside the legacy core group. The nodeclass controller records events through controller-runtime's `GetEventRecorder`, which writes `events.k8s.io` Events, so the controller logged `events.events.k8s.io is forbidden` on every event it tried to emit. The core `""` group is kept because karpenter core still uses the legacy recorder (#52).

### Security
- Go toolchain bumped to 1.26.7 (from 1.26.5), clearing five stdlib vulnerabilities flagged by govulncheck (GO-2026-6218, GO-2026-6090, GO-2026-6089, GO-2026-5972, GO-2026-5026; all fixed by 1.26.6). The release image builder is pinned to `golang:1.26.7-alpine` so the shipped binary is built with the same toolchain CI tests with. Local builds now require Go >= 1.26.7 (#57).

### Changed
- `make vendor-core-crds` downloads the `sigs.k8s.io/karpenter` module before resolving its directory, fixing `generate` CI failures on any PR that changes `go.mod`/`go.sum` (cold module cache made `go list -m` return an empty dir) (#57).

### Changed (dependencies)
- Bumped `sigs.k8s.io/karpenter` to 1.14.1, `hetznercloud/hcloud-go` to 2.47.0, and `k8s.io/{api,apimachinery,client-go}` to 0.36.4 (#58).

## [2.1.0] - 2026-08-01

### Fixed
- Pods bound to an hcloud CSI volume can now trigger provisioning. The hcloud CSI driver pins `PersistentVolume` `nodeAffinity` on `csi.hetzner.cloud/location`, which Karpenter did not recognize, so volume-topology scheduling rejected every PVC-backed pod with `label "csi.hetzner.cloud/location" does not have known values`. That domain is now aliased to the standard `topology.kubernetes.io/zone` via `NormalizedLabels`, mirroring how `karpenter-provider-aws` aliases `topology.ebs.csi.aws.com/zone`. No NodePool or StorageClass changes are needed (#46).

### Added
- Chart: `nodeSelector`, `affinity`, `tolerations`, `topologySpreadConstraints`, `imagePullSecrets`, `priorityClassName`, `command` and `args` on the controller Deployment. All empty by default, so existing releases render unchanged (#47).
- Chart: optional `podDisruptionBudget` (off by default; `maxUnavailable: 1` when enabled). `minAvailable` and `maxUnavailable` are mutually exclusive positive integers, and configurations that would block all voluntary drains (`minAvailable >= replicas`, `maxUnavailable: 0`) are rejected at render time. `replicas` is likewise validated as a non-negative integer (#47).

## [2.0.0] - 2026-07-28

### Changed
- **BREAKING:** `HCloudNodeClass` graduated from `karpenter.hetzner.cloud/v1alpha1` to `karpenter.hetzner.cloud/v1`. There is no conversion webhook — update `apiVersion` in your manifests and re-apply your node classes (#37).

### Added
- k3s agent bootstrap support: `examples/k3s-nodeclass.yaml` and `docs/k3s-bootstrap.md`.
- Artifact Hub repository ID and badge (#32).

### Fixed
- The chart ships the Karpenter core CRDs (`NodePool`, `NodeClaim`), so a clean install no longer leaves the controller crash-looping on its own watches. Both are vendored from the pinned `sigs.k8s.io/karpenter` during `make generate`, so a dependency bump that changes either schema fails the `generate-verify` CI gate instead of shipping a stale CRD (#44).
- The operator image is pinned to the chart's `appVersion` instead of floating on `:latest`, so an installed chart runs the operator it was published with (#45).
- Return `NodeClaimNotFoundError` when deleting an already-gone server, instead of a hard error (#40).

### Changed (dependencies)
- Bumped `sigs.k8s.io/karpenter`, `hetznercloud/hcloud-go`, and GitHub Actions (#34, #35, #36, #39, #41, #42).
- CI reads the Go version from `go.mod` instead of pinning it.

## [1.0.0] - 2026-06-16

First stable release: a complete CloudProvider implementation with full drift
detection, observability, supply-chain attestations, and adoption docs.

### Added
- Image label selector: `HCloudNodeClass.spec.imageSelector.selector` filters Hetzner images by arbitrary labels, so you can pin the exact image (version plus baked extensions, e.g. a gVisor-Talos snapshot) instead of fuzzy description matching (#23).
- Wrong-arch guard: provisioning is rejected when the resolved image architecture does not match the architecture the NodeClaim requires (#23).
- Placement group creation and assignment: `placementGroupStrategy: spread` now actually creates/assigns a cluster-scoped Hetzner placement group (previously declared but a no-op) (#24).
- Location drift detection: servers whose Hetzner location is no longer in the NodeClass `locations` are flagged as drifted (#24).
- Label drift detection: servers whose labels no longer cover the NodeClass `labels` are flagged as drifted (#26).
- Structured logging across provider operations (server create/delete, image resolution, drift) via the controller-runtime contextual logger (#26).
- `seccompProfile: RuntimeDefault` on the controller pod for PSS `restricted` compliance (#26).
- Prometheus metrics (`karpenter_hetzner_*`: server create/delete results and duration, hcloud API calls, drift detections, instance-type cache hits/misses) plus a Helm `ServiceMonitor` (#29).
- Warning Events from the nodeclass controller on every NotReady path, so `kubectl describe hcloudnodeclass` explains why a class is not Ready (#29).
- Examples (`talos-nodeclass`, `ubuntu-nodeclass`, `nodepool-multiarch`) and Talos/Ubuntu bootstrap guides (#28).

### Security
- Cosign keyless signing of the release image using GitHub OIDC (no long-lived keys).
- SLSA provenance attestation (`mode=max`) attached in-registry via BuildKit.
- In-registry SBOM attestation (CycloneDX) attached via BuildKit.
- Standalone SPDX SBOM uploaded as a workflow artifact via `anchore/sbom-action`.

## [0.3.0] - 2026-06-13

### Added
- `HCloudNodeClass.spec.userDataSecretRef`: reference a Kubernetes Secret for cloud-init user data instead of inlining it in the NodeClass spec (#20).

## [0.2.0] - 2026-06-13

### Changed
- Upgraded to Karpenter v1.13.0 (#18).
- Bumped Helm chart to 0.2.0 (#19).

## [0.1.0] - 2026-06-13

### Added
- Initial `CloudProvider` implementation covering all 8 Karpenter interface methods.
- Instance provider: Hetzner Cloud server CRUD (create, get, delete, list).
- Image family provider: Talos and Ubuntu image resolution.
- Instance type provider with pricing data and caching.
- `HCloudNodeClass` CRD with labels and cluster-scope fix.
- Helm chart for `karpenter-provider-hetzner`.
- Multi-arch Docker image (`linux/amd64`, `linux/arm64`) built via cross-compilation (no emulation).
- GitHub Actions: test, lint, release, and `govulncheck` security workflows.
- CI publishes Helm chart to OCI registry on release.

### Fixed
- Resolve images per-architecture; NodeClass is `Ready` if any arch resolves (#14).
- Grant full Karpenter-core RBAC in Helm chart (#13).
- Treat `unsupported location for server type` as an unavailable offering rather than a hard error (#16).

[Unreleased]: https://github.com/stubbi/karpenter-provider-hetzner/compare/v3.0.1...HEAD
[3.0.1]: https://github.com/stubbi/karpenter-provider-hetzner/compare/v3.0.0...v3.0.1
[3.0.0]: https://github.com/stubbi/karpenter-provider-hetzner/compare/v2.2.0...v3.0.0
[2.2.0]: https://github.com/paperclipinc/karpenter-provider-hetzner/compare/v2.1.1...v2.2.0
[2.1.1]: https://github.com/paperclipinc/karpenter-provider-hetzner/compare/v2.1.0...v2.1.1
[2.1.0]: https://github.com/paperclipinc/karpenter-provider-hetzner/compare/v2.0.0...v2.1.0
[2.0.0]: https://github.com/paperclipinc/karpenter-provider-hetzner/compare/v1.0.0...v2.0.0
[1.0.0]: https://github.com/paperclipinc/karpenter-provider-hetzner/compare/v0.3.0...v1.0.0
[0.3.0]: https://github.com/paperclipinc/karpenter-provider-hetzner/compare/v0.2.0...v0.3.0
[0.2.0]: https://github.com/paperclipinc/karpenter-provider-hetzner/compare/v0.1.0...v0.2.0
[0.1.0]: https://github.com/paperclipinc/karpenter-provider-hetzner/releases/tag/v0.1.0
