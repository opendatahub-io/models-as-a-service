ARG GOLANG_VERSION=1.25

ARG BUILDPLATFORM
ARG TARGETPLATFORM

FROM --platform=$BUILDPLATFORM registry.access.redhat.com/ubi9/go-toolset:$GOLANG_VERSION AS builder
ARG CGO_ENABLED=1
ARG GOEXPERIMENT=strictfipsruntime
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app
COPY maas-controller/go.mod maas-controller/go.sum ./
RUN go mod download
COPY maas-controller/ ./

USER root

RUN CGO_ENABLED=${CGO_ENABLED} GOEXPERIMENT=${GOEXPERIMENT} GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -a -trimpath -ldflags="-s -w" -o manager ./cmd/manager

FROM --platform=$TARGETPLATFORM registry.access.redhat.com/ubi9/ubi-minimal:latest

WORKDIR /

COPY --from=builder /app/manager /manager

RUN chmod +x /manager
COPY maas-api/deploy /maas-api/deploy
COPY deployment/base/maas-api /deployment/base/maas-api
COPY deployment/base/maas-controller/policies /deployment/base/maas-controller/policies
COPY deployment/base/payload-processing /deployment/base/payload-processing
COPY deployment/components /deployment/components
RUN chmod -R g=u /maas-api /deployment

# Use a non-root user (OpenShift will assign random UID)
USER 1001

ENTRYPOINT ["/manager"]
