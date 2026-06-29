ARG GOLANG_VERSION=1.25

ARG BUILDPLATFORM
ARG TARGETPLATFORM

# Build stage - runs from repo root context
FROM --platform=$BUILDPLATFORM registry.access.redhat.com/ubi9/go-toolset:$GOLANG_VERSION AS builder
ARG CGO_ENABLED=0
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app

# Copy go mod files first for better layer caching
COPY weight-controller/go.mod weight-controller/go.sum ./
RUN go mod download

# Copy source code
COPY weight-controller/ ./

USER root

RUN CGO_ENABLED=${CGO_ENABLED} GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} \
    go build -a -trimpath -ldflags="-s -w" -o weight-controller ./cmd

# Runtime stage - distroless for minimal attack surface
FROM gcr.io/distroless/static:nonroot

WORKDIR /

COPY --from=builder /app/weight-controller /weight-controller

USER 65532:65532

ENTRYPOINT ["/weight-controller"]
