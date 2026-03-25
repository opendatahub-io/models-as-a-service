# MaaS API Overview

This page provides a high-level overview of the MaaS API endpoints. For full request/response schemas and interactive documentation, see [MaaS API (Swagger)](api-reference.md).

## Authentication

All endpoints except `/health` require authentication via the `Authorization: Bearer <token>` header. Use either:

- **OpenShift token** — from `oc whoami -t` for interactive use
- **API key** — created via `POST /v1/api-keys` for programmatic access

---

## Endpoints by Category

### Health

| Method | Path | Description |
|--------|------|-------------|
| GET | `/health` | Health check. No authentication required. Used by load balancers and monitoring. |

### Models

| Method | Path | Description |
|--------|------|-------------|
| GET | `/v1/models` | List available LLMs in OpenAI-compatible format. Returns models the authenticated user can access. |

### Tiers (Legacy)

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/tiers/lookup` | Look up the highest subscription tier for a set of groups. Used by tier-based access control. |

!!! note "Subscription model"
    The subscription-based architecture (MaaSAuthPolicy, MaaSSubscription) is the current approach. The tiers endpoint is retained for backward compatibility with tier-based deployments.

### API Keys

| Method | Path | Description |
|--------|------|-------------|
| POST | `/v1/api-keys` | Create a new API key. Returns plaintext key **once**; only the hash is stored. Optional body field `subscription` selects the MaaSSubscription; if omitted, the highest-priority accessible subscription is used. |
| POST | `/v1/api-keys/search` | Search and filter API keys with pagination, sorting, and status filters. |
| GET | `/v1/api-keys/{id}` | Get metadata for a specific API key. |
| DELETE | `/v1/api-keys/{id}` | Revoke a specific API key. |
| POST | `/v1/api-keys/bulk-revoke` | Revoke all active API keys for a user. Admins can revoke any user's keys. |

---

## Base URL

The MaaS API is typically exposed under a path prefix, for example:

- `https://maas.example.com/maas-api`
- `https://<cluster-domain>/maas-api`

Use the base URL appropriate for your deployment when calling these endpoints.

---

## Next Steps

- **[MaaS API (Swagger)](api-reference.md)** — Interactive API documentation with full schemas and "Try it out"
- **[Token Management](../configuration-and-management/token-management.md)** — How to create and use API keys
- **[Self-Service Model Access](../user-guide/self-service-model-access.md)** — End-user guide for getting an API key and calling models
