ARG GOLANG_VERSION=1.24

ARG BUILDPLATFORM
ARG TARGETPLATFORM

FROM --platform=$BUILDPLATFORM registry.access.redhat.com/ubi9/go-toolset:$GOLANG_VERSION AS builder
ARG CGO_ENABLED=0
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

USER root

RUN CGO_ENABLED=${CGO_ENABLED} GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -a -trimpath -ldflags="-s -w" -o manager ./cmd/manager

FROM --platform=$TARGETPLATFORM registry.access.redhat.com/ubi9/ubi-minimal:latest

WORKDIR /

COPY --from=builder /app/manager .

RUN chmod +x /manager

# Use a non-root user (OpenShift will assign random UID)
USER 1001

ENTRYPOINT ["/manager"]
