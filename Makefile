.PHONY: build test lint generate generate-verify vendor-core-crds docker-build test-envtest

BINARY         := karpenter-provider-hetzner
IMAGE          := ghcr.io/stubbi/karpenter-provider-hetzner
TAG            ?= latest
CONTROLLER_GEN := go run sigs.k8s.io/controller-tools/cmd/controller-gen@v0.19.0
ENVTEST        := go run sigs.k8s.io/controller-runtime/tools/setup-envtest@v0.25.2
ENVTEST_K8S_VERSION ?= 1.34.0

build:
	go build -o bin/$(BINARY) ./cmd/controller

test:
	go test -race -count=1 ./...

lint:
	golangci-lint run ./...

generate: vendor-core-crds
	$(CONTROLLER_GEN) object paths="./pkg/apis/..."
	$(CONTROLLER_GEN) crd paths="./pkg/apis/..." output:crd:dir=charts/karpenter-provider-hetzner/crds

# Copy the karpenter core CRDs (NodePool, NodeClaim) out of the pinned
# sigs.k8s.io/karpenter module and into the chart. The controller watches both,
# so the chart is unusable without them. Sourcing them from the module rather
# than checking in a hand-copied snapshot means a version bump that changes the
# schema is caught by `make generate-verify` in CI.
#
# NodeOverlay and CapacityBuffer are deliberately not vendored: both sit behind
# feature gates that default to false, and this chart does not expose them.
vendor-core-crds:
	@set -eu; \
	go mod download sigs.k8s.io/karpenter; \
	dir="$$(go list -m -f '{{.Dir}}' sigs.k8s.io/karpenter)"; \
	[ -n "$$dir" ] || { echo "sigs.k8s.io/karpenter not found in module cache" >&2; exit 1; }; \
	for crd in karpenter.sh_nodepools.yaml karpenter.sh_nodeclaims.yaml; do \
		cp "$$dir/pkg/apis/crds/$$crd" charts/karpenter-provider-hetzner/crds/$$crd; \
		chmod u+w charts/karpenter-provider-hetzner/crds/$$crd; \
	done

generate-verify: generate
	@if [ -n "$$(git status --porcelain pkg/apis charts/karpenter-provider-hetzner/crds)" ]; then \
		echo "generated files are out of date; run 'make generate' and commit"; \
		git --no-pager diff -- pkg/apis charts/karpenter-provider-hetzner/crds; \
		exit 1; \
	fi

test-envtest:
	assets="$$($(ENVTEST) use $(ENVTEST_K8S_VERSION) -p path)" && [ -n "$$assets" ] && \
		KUBEBUILDER_ASSETS="$$assets" go test -race -count=1 ./pkg/controllers/... ./pkg/apis/...

docker-build:
	docker build -t $(IMAGE):$(TAG) .

docker-push: docker-build
	docker push $(IMAGE):$(TAG)
