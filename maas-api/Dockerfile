ARG GOLANG_VERSION=1.25

ARG BUILDPLATFORM
ARG TARGETPLATFORM

FROM --platform=$BUILDPLATFORM registry.access.redhat.com/ubi9/go-toolset:$GOLANG_VERSION AS builder
ARG CGO_ENABLED=1
ARG TARGETOS
ARG TARGETARCH

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .

USER root

RUN CGO_ENABLED=${CGO_ENABLED} GOEXPERIMENT=strictfipsruntime GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} go build -a -trimpath -ldflags="-s -w" -o maas-api ./cmd/

FROM --platform=$TARGETPLATFORM registry.access.redhat.com/ubi9/ubi-minimal:latest

WORKDIR /app

COPY --from=builder /app/maas-api .

# Make binary executable and fix permissions for OpenShift
RUN chmod +x maas-api && \
    chgrp -R 0 /app && \
    chmod -R g=u /app

# Use a non-root user (OpenShift will assign random UID)
USER 1001

EXPOSE 8080

CMD ["./maas-api"]
