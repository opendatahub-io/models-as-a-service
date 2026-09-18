# MaaS Discovery Service

Tenant discovery service for multi-tenant Models as a Service (MaaS). Platform clients query `GET /v1/tenants` to discover available tenants and their gateway connection metadata.

See [ODH-ADR-MS-0004](https://github.com/opendatahub-io/architecture-decision-records/blob/main/architecture-decision-records/model-serving/ODH-ADR-MS-0004-ai-gateway-tenancy-discovery.md) for the design.

## Build

```bash
make build     # full pipeline: tidy, lint, test, binary
make binary    # binary only (skip checks)
```

## Run

```bash
# Development (self-signed TLS)
make run
# or
./bin/discovery --self-signed

# Production (provide cert/key)
./bin/discovery --tls-cert /path/to/cert.pem --tls-key /path/to/key.pem
```

## Container

```bash
make build-image    # build container image
make push-image     # push to registry
```

## API

| Endpoint | Description |
|---|---|
| `GET /v1/tenants` | List available tenants with gateway metadata |
| `GET /healthz` | Liveness probe |
| `GET /readyz` | Readiness probe |

## Status

Scaffold only. The informer-based tenant cache is tracked in RHOAIENG-90729.
