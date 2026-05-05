# API Key Management

This guide explains how to create and manage API keys for accessing models through the MaaS platform.

!!! tip "API keys for model access"
    The platform uses **API keys** (`sk-oai-*`) stored in PostgreSQL for programmatic access. Create keys via `POST /v1/api-keys` (authenticate with your OpenShift token) and use them with the `Authorization: Bearer` header. Each key is bound to one MaaSSubscription at creation time.

!!! note "Prerequisites"
    This document assumes your administrator has configured subscriptions (MaaSAuthPolicy, MaaSSubscription) that grant you access to models.

---

## Creating API Keys

### Get Your OpenShift Token

First, obtain your OpenShift authentication token:

```bash
OC_TOKEN=$(oc whoami -t)
```

### Set Platform URL

Get the MaaS API URL for your cluster:

```bash
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
MAAS_API_URL="https://maas.${CLUSTER_DOMAIN}"
```

### Create an API Key

Create a new API key with a name, description, and expiration:

```bash
API_KEY_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer ${OC_TOKEN}" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{"name": "my-api-key", "description": "Key for model access", "expiresIn": "90d"}' \
  "${MAAS_API_URL}/maas-api/v1/api-keys")

API_KEY=$(echo $API_KEY_RESPONSE | jq -r .key)
echo "API Key: ${API_KEY}"
```

!!! warning "API key shown only once"
    The plaintext API key is returned **only at creation time**. Store it securely when displayed. If you lose it, you must create a new key.

**Request body fields:**

| Field | Required | Description |
|-------|----------|-------------|
| `name` | Yes | Human-friendly name for the key (e.g., "production-bot") |
| `description` | No | Optional description |
| `expiresIn` | No | TTL string (e.g., `90d`, `30d`, `1h`). Omit to use configured maximum. |
| `subscription` | No | MaaSSubscription name to bind. Omit to auto-select highest priority. |
| `ephemeral` | No | Set to `true` for short-lived keys (max 1 hour). See [Ephemeral Keys](#ephemeral-keys). |

**Response:**

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "key": "sk-oai-...",
  "name": "my-api-key",
  "subscription": "premium-subscription",
  "expiresAt": "2026-07-27T12:00:00Z"
}
```

### Subscription Binding

Each API key binds to one MaaSSubscription at creation, determining which models you can access and what rate limits apply.

- **Automatic** (omit `subscription`): Platform selects your highest-priority subscription
- **Explicit** (set `subscription`): Bind to a specific subscription by name

The response includes the bound `subscription` name.

!!! info "Learn more"
    For technical details, see [API Key Authentication](../concepts/api-key-authentication.md#subscription-binding-and-priority).

---

## Managing Your API Keys

### Listing Your Keys

Search for your API keys with optional filters:

```bash
curl -sSk -X POST "${MAAS_API_URL}/maas-api/v1/api-keys/search" \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -d '{
    "status": "active",
    "limit": 10,
    "offset": 0
  }' | jq .
```

**Filter options:**

| Field | Description |
|-------|-------------|
| `status` | Filter by status: `active`, `revoked`, `expired` |
| `limit` | Number of results per page (default: 10) |
| `offset` | Offset for pagination (default: 0) |
| `includeEphemeral` | Include ephemeral keys (default: false) |

### Get Key Details

Get metadata for a specific key by ID:

```bash
KEY_ID="550e8400-e29b-41d4-a716-446655440000"
curl -sSk "${MAAS_API_URL}/maas-api/v1/api-keys/${KEY_ID}" \
  -H "Authorization: Bearer $(oc whoami -t)" | jq .
```

**Response:**

```json
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "my-api-key",
  "description": "Key for model access",
  "status": "active",
  "subscription": "premium-subscription",
  "createdAt": "2026-04-28T12:00:00Z",
  "expiresAt": "2026-07-27T12:00:00Z",
  "lastUsedAt": "2026-04-29T10:30:00Z"
}
```

---

## Key Expiration

Set `expiresIn` to a duration string (`"90d"`, `"30d"`, `"1h"`), or omit it to use the platform maximum. Expired keys return `valid: false` on validation—create a new key to continue access.

**Best practices:** Long TTL (90d) for stable integrations, short TTL (30d or less) for security-conscious environments, ephemeral keys (≤1h) for temporary access.

---

## Revoking Keys

### Revoke a Single Key

```bash
KEY_ID="550e8400-e29b-41d4-a716-446655440000"
curl -sSk -X DELETE "${MAAS_API_URL}/maas-api/v1/api-keys/${KEY_ID}" \
  -H "Authorization: Bearer $(oc whoami -t)"
```

Revocation takes effect immediately (Authorino may cache briefly).

### Revoke All Your Keys

Revoke all active keys for your user:

```bash
curl -sSk -X POST "${MAAS_API_URL}/maas-api/v1/api-keys/bulk-revoke" \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" | jq .
```

**Response:**

```json
{
  "revokedCount": 5,
  "message": "Successfully revoked 5 API key(s)"
}
```

**When to revoke all keys:**
- Security incident (compromised credentials)
- Your groups changed and you need fresh group membership
- Rotating all credentials as part of security policy

!!! note "Administrator bulk revocation"
    Administrators can revoke keys for any user. See [API Key Administration](../configuration-and-management/api-key-administration.md#bulk-revocation).

---

## Ephemeral Keys

Ephemeral keys are short-lived credentials for temporary access (e.g., playground sessions, demos).

**Differences from regular keys:**

| Property | Regular Key | Ephemeral Key |
|----------|-------------|---------------|
| Default expiration | Configured maximum (e.g., 90 days) | 1 hour |
| Maximum expiration | Configured maximum | 1 hour |
| Name required | Yes | No (auto-generated if omitted) |
| Visible in default search | Yes | No (`includeEphemeral: true` required) |
| Auto-cleanup | No | Yes (30-minute grace after expiry) |

**Create an ephemeral key:**

```bash
curl -sSk -X POST "${MAAS_API_URL}/maas-api/v1/api-keys" \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -d '{"ephemeral": true, "expiresIn": "30m"}' | jq .
```

Expired ephemeral keys are automatically cleaned up by a CronJob. See [API Key Administration](../configuration-and-management/api-key-administration.md#ephemeral-key-cleanup) for details.

---

## Frequently Asked Questions

**Q: My subscription access is wrong. How do I fix it?**

A: Your access is determined by your group membership at the time the API key was created. Those groups are stored with the key. If your groups have changed, create a new API key to pick up the new membership.

---

**Q: What happens if my group membership changes after I create an API key?**

A: API keys store your groups and bound subscription name at creation time. If your group membership changes, the key retains the **old** groups until it is revoked. To pick up new groups or a different subscription, revoke the old key and create a new one.

For technical details, see [Group Membership Snapshots](../concepts/api-key-authentication.md#group-membership-snapshots).

---

**Q: What if two MaaSSubscriptions use the same `spec.priority`?**

A: API key mint and subscription selection use a deterministic order when priorities tie (e.g. token limit, then name). Operators should still assign distinct priorities when possible. The MaaSSubscription controller sets status condition `SpecPriorityDuplicate` and logs when another subscription shares the same priority—use that to clean up configuration.

---

**Q: What's the difference between my OpenShift/OIDC token and an API key?**

A: Your **OpenShift/OIDC token** is your identity token from authentication. An **API key** is a long-lived credential created via `POST /v1/api-keys` and stored as a hash in PostgreSQL. The API passes your `Authorization` header as-is to each model endpoint, so use whichever credential type the model route accepts. In the current interim OIDC rollout, use the OIDC token to mint an API key first, then use that key for `/v1/models` and inference.

---

**Q: How long should my API keys be valid for?**

A: For interactive use or long-running integrations, keys with long TTL (e.g., 90d) or the default maximum are common. For higher security, use shorter TTLs (e.g., 30d) and rotate keys periodically.

---

**Q: Can I have multiple active API keys at once?**

A: Yes. Each call to `POST /v1/api-keys` creates a new, independent key. You can list and manage them via `POST /v1/api-keys/search` or `GET /v1/api-keys/{id}`.

---

**Q: What happens if the maas-api service is down?**

A: You will not be able to create or validate API keys. Inference requests that use API keys will fail until the service is back.

---

**Q: Can I use one API key to access multiple different models?**

A: Yes. Your API key is bound to a subscription at creation time. If that subscription provides access to multiple models, a single key works for all of them. To access models from a different subscription, create a new API key bound to that subscription.

---

**Q: Where is my API key stored?**

A: Only the SHA-256 hash of your key is stored in PostgreSQL. The plaintext key is returned once at creation and is never stored. If you lose it, you must create a new key.

---

## Next Steps

- **[Model Discovery](model-discovery.md)** - List available models

## Related Documentation

- **[Inference](inference.md)** - Make inference requests with your API key
- **[API Key Authentication](../concepts/api-key-authentication.md)** - Technical deep dive into how authentication works
- **[API Key Administration](../configuration-and-management/api-key-administration.md)** - For administrators: bulk revocation and cleanup operations
- **[Access and Quota Overview](../concepts/subscription-overview.md)** - How policies and subscriptions work together
