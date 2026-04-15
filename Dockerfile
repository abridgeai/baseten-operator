# Build the baseten-operator binary
ARG BUILDER_IMAGE=golang:1.26.2
ARG RUNTIME_IMAGE=gcr.io/distroless/static:nonroot
FROM ${BUILDER_IMAGE} AS builder
ARG TARGETOS
ARG TARGETARCH
ARG CGO_ENABLED=0
ARG VERSION=dev

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go mod download

# Copy the Go source (relies on .dockerignore to filter)
COPY . .

# Build
# the GOARCH has no default value to allow the binary to be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN CGO_ENABLED=${CGO_ENABLED} GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} \
    go build -a -installsuffix cgo -ldflags "-X main.version=${VERSION}" -o baseten-operator cmd/main.go

# Runtime stage
FROM ${RUNTIME_IMAGE}
WORKDIR /
COPY --from=builder /workspace/baseten-operator .
USER 65532:65532

ENTRYPOINT ["/baseten-operator"]
