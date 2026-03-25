# Dockerfile has specific requirement to put this ARG at the beginning:
# https://docs.docker.com/engine/reference/builder/#understand-how-arg-and-from-interact
ARG BUILDER_IMAGE=golang:1.25
ARG BASE_IMAGE=gcr.io/distroless/static:nonroot

## Multistage build
FROM ${BUILDER_IMAGE} AS builder
ENV CGO_ENABLED=0
ENV GOOS=linux
ENV GOARCH=amd64
ARG COMMIT_SHA=unknown
ARG BUILD_REF

# Dependencies
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download

# Sources
COPY cmd/ cmd/
COPY pkg/ pkg/

# -X needs the exact import path of the dependency's version package (matches go.mod / module graph).
RUN VERSION_PKG="$(go list -f '{{.ImportPath}}' sigs.k8s.io/gateway-api-inference-extension/version)" && \
	go build -ldflags="-X ${VERSION_PKG}.CommitSHA=${COMMIT_SHA} -X ${VERSION_PKG}.BuildRef=${BUILD_REF}" -o /bbr ./cmd

# Multistage deploy
FROM ${BASE_IMAGE}

WORKDIR /
COPY --from=builder /bbr /bbr

ENTRYPOINT ["/bbr"]
