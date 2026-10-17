# Cross-compile from the native build platform to the target platform so multi-arch
# builds don't run `go build` under slow QEMU emulation.
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine AS builder
ARG TARGETOS
ARG TARGETARCH
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -o /karpenter-provider-hetzner ./cmd/controller

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=builder /karpenter-provider-hetzner /karpenter-provider-hetzner
ENTRYPOINT ["/karpenter-provider-hetzner"]
