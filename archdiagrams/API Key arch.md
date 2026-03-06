# **API Key Management Technical Architecture**

Updated: Feb 23, 2026  
Author: [Ishita Sequeira](mailto:isequeir@redhat.com)

## **1\. Executive Summary**

This document defines the technical architecture for long-living API key management in OpenShift AI. The design addresses the core limitation of Kubernetes Service Account tokens (enforced expiration) while providing an industry-standard key experience.

**Implementation:** PostgreSQL storage is introduced as part of Phase 1\. The architecture uses a pluggable storage interface for future extensibility (e.g., Vault for enterprise compliance), but Phase 1 delivers a complete solution with PostgreSQL alone.

## **2\. Requirements Analysis**

### **2.1 Key Lifecycle Management**

The core problem we're solving is the forced expiration of Kubernetes Service Account tokens. Today, when a token expires, integrations break unexpectedly—CI/CD pipelines fail, production services lose access, and someone has to scramble to rotate credentials. Our API keys flip this model: keys remain valid until the customer explicitly decides to revoke them.

By default, keys are long-lived and do not expire on a fixed schedule, unlike Kubernetes tokens that enforce TTLs. However, we will support configurable key expiration, allowing administrators to define policies that control how long a key remains valid. This capability is essential for enterprise customers who must meet security compliance requirements, where key rotation and revocation are standard audit expectations.

We also support **individual revocation**. If you have five API keys and one gets leaked, you revoke just that one. The other four continue working. This is a significant improvement over the current model where revoking a service account affects all tokens associated with it.

Revocation is **instant** (subject to caching). When you revoke a key, validation quickly starts rejecting it. For performance reasons, Authorino may cache validation results briefly, but the default TTL provides a good balance between latency and security.

Finally, users can attach **metadata** to their keys, names like **Production backend** or **CI pipeline staging**, so they can remember what each key is for when they come back months later to manage them.

### **2.2 Key Format & Security**

Our keys follow industry conventions (OpenAI, Stripe, GitHub)—short, recognizable strings with a prefix that identifies the key type at a glance. This is a deliberate UX improvement over Kubernetes JWTs which are 900+ characters of opaque base64.

Security follows the **show once** pattern: keys are displayed exactly once at creation, then only the hash is stored. Even with full database access, an attacker gets SHA-256 hashes that are computationally infeasible to reverse. See **Section 8** for technical specifications.

For storage, Phase 1 uses PostgreSQL. The storage layer is built around an interface, so future backends (Vault, cloud secret managers) can be added without changing application code.

### **2.3 API & Integration**

The API follows standard REST conventions: `POST` to create, `GET` to list or retrieve, `PATCH` to update, `DELETE` to revoke. The critical detail is that only `POST /api-keys` returns the actual key value. Every other endpoint returns metadata only name, description, creation date, status. This ensures the plaintext key can never be retrieved after creation.

For model inference, applications authenticate using the standard `Authorization: Bearer <key>` header. This is the same format used by OpenAI, Anthropic, and every other AI API provider. Existing tools, SDKs, and scripts work without modification just swap in your key.

At the gateway, Authorino intercepts requests, recognizes the `sk-oai-*` prefix, and validates the key by calling our internal endpoint. On success, it injects identity headers (`X-User-Id`, `X-Key-Id`) so downstream services know who's calling. The model service never sees the raw key; it just sees the authenticated identity.

### **2.4 UI Requirements**

Self-service is a core design goal. Users should create, list, and revoke their own API keys without filing tickets or waiting for admin approval. The friction should be zero for legitimate use cases.

The UI must also clearly communicate the "show once" nature of keys. When a key is created, we show a prominent warning: "This key will only be shown once. Copy it now." A copy button makes this easy, and we may require users to confirm they've saved the key before dismissing the modal. This prevents the frustrating scenario where someone creates a key, closes the dialog, and immediately realizes they forgot to copy it.

## **3\. Personas and Access Patterns**

Our architecture serves three distinct personas, each with different access requirements and use cases.

### **3.1 Model Consumer**

The Model Consumer is an external developer or application that calls AI model endpoints via API. They don't have—and don't need—access to the OpenShift cluster or RHOAI Dashboard. They just want to send prompts and get responses.

**Key provisioning:** Since Model Consumers have no dashboard access, a MaaS Admin generates API keys on their behalf and provides the key through a secure out-of-band channel (e.g., secure messaging, onboarding portal). The Model Consumer then uses this key for all API calls via the standard `Authorization: Bearer <key>` header.

This is similar to how cloud providers provision API keys for service accounts—an admin creates the credential and distributes it to the consumer.

### **3.2 MaaS Admin**

The MaaS Admin is a platform administrator responsible for deploying models, configuring policies, and managing the overall system. They have full OpenShift cluster access—they can kubectl into the cluster, access the OpenShift console, and use the RHOAI Dashboard with elevated privileges.

From a key management perspective, admins have a broader view. They can see all users' keys (metadata only, never the actual key values) and revoke any key. This is essential for incident response: if a key is compromised or a user leaves the organization, an admin can immediately revoke access without waiting for the key owner.

Admins authenticate via OIDC through their normal OpenShift login. They don't typically need API keys for their own use \- they're already authenticated.

### **3.3 Dashboard User (Playground)**

Dashboard Users are internal users with RHOAI Dashboard access. The most common example is someone using the AI Playground to experiment with models. They're logged into the dashboard via OpenShift auth, and their session already grants them access to models.

These users can create API keys if they want programmatic access outside the dashboard—say, for a script or a local development environment. But within the dashboard, their session-based authentication handles everything. They don't need an API key to use the playground.

Dashboard Users manage their own keys only. They can't see other users' keys or perform admin actions. Admin users should be able to view keys for all users.

### **3.4 Session-Based vs API Key Access**

A key design principle: **logged-in users should never re-authenticate**. If you're already in the RHOAI Dashboard with a valid OpenShift session, the system should recognize that and let you manage keys immediately. You shouldn't be prompted for credentials again.

When a logged-in user creates an API key, we extract their identity from the session and associate the new key with that identity. The key is for external use—CI/CD pipelines, scripts, applications running outside the dashboard. Within the dashboard itself, the session token handles authentication.

This dual-path model (session auth for interactive use, API keys for programmatic use) gives users flexibility without compromising security.

### **3.5 Persona Summary**

| Persona | OpenShift Access | Auth Method | Key Management |
| :---- | :---- | :---- | :---- |
| Model Consumer | ❌ Not required | API Key | Keys provisioned by admin |
| MaaS Admin | ✅ Required | External OIDC /Internal OCP Oauth (session) | Create keys for any user, revoke any key |
| Dashboard User | ✅ Required | External OIDC /Internal OCP Oauth(session) | Self-service for own keys |

## **4\. System Architecture**

### **4.1 Component Overview**

Users interact through the OpenShift AI Console or CLI, with requests flowing through the Gateway and Authorino for authentication. The MaaS API handles key management and validation, storing keys in PostgreSQL.

![][image1]  
![][image2]

### **4.2 Component Responsibilities**

| Component | Responsibility |
| :---- | :---- |
| API Key Service | Key lifecycle: create, list, get, update, revoke |
| Validation Service | Fast-path authentication for incoming requests |
| PostgreSQL | Key hash and metadata storage |

### **4.3 Key Creation Flow**

The user requests a key through the UI, MaaS API generates a random key, hashes it, stores the hash, and returns the plaintext key exactly once.

![][image3]

### **4.4 Key Validation Flow (Phase 1\)**

For Phase 1, validation is a direct database lookup—no caching layer. Authorino calls the MaaS API validation endpoint, which queries PostgreSQL.

![][image4]

**Future (Phase 2+):** Caching can be added at the Authorino layer if validation latency becomes a bottleneck at scale.

### **4.5 Key Revocation Flow**

Revocation updates the key status in PostgreSQL. Since Phase 1 has no caching, revocation is immediate, the next validation query will see the revoked status.

![][image5]

## **5\. Data Access Patterns**

The way different personas access key data shapes our storage design. The most important insight is that **key validation is the hot path**—it happens on every single inference request and must be fast. Everything else (listing keys in the UI, admin views) is comparatively rare.

| Operation | Query Pattern | Frequency | Index |
| :---- | :---- | :---- | :---- |
| User lists keys | `WHERE user_id = X` | Low (UI) | `idx_user_status` |
| Admin lists all | `ORDER BY created_at` | Low (admin) | Primary |
| **Validate by hash** | `WHERE key_hash = X` | **High (every request)** | `idx_key_hash` |

When a user opens the key management UI, we query for their keys—this might happen a few times a day per user. When an admin investigates an incident, they might query for all keys or filter by user—this happens rarely.

But validation? That's `WHERE key_hash = X` on every model request. If you have 1000 requests per second, that's 1000 validation lookups per second. This is why we have a dedicated index on `key_hash` and why we cache validation results at the Authorino layer.

PostgreSQL handles all these patterns well. A composite index on `(user_id, status)` covers user listing. The `key_hash` index covers validation. 

## **6\. Storage Backend**

### **6.1 PostgreSQL Storage**

PostgreSQL is introduced as the storage backend for Phase 1\. While this adds a new infrastructure component, PostgreSQL is a well-understood, proven technology that works in environments where external secret managers aren't available.

The pattern is proven at scale. GitHub stores API key hashes in PostgreSQL. GitLab does the same. These platforms handle millions of keys without issues. PostgreSQL's B-tree indexes provide O(log n) lookups, meaning validation performance stays consistent even as key counts grow.

**6.2 Schema**

The schema is intentionally simple. One table holds everything we need:

```
CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    username TEXT NOT NULL,
    name TEXT NOT NULL,
    description TEXT,
    key_hash TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'active',
    user_groups TEXT[] NOT NULL, -- JSON array of user's groups at creation time
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,

    CONSTRAINT api_keys_status_check CHECK (status IN ('active', 'revoked', 'expired'))
);

CREATE INDEX idx_api_keys_hash ON api_keys(key_hash);
CREATE INDEX idx_api_keys_user_status ON api_keys(user_id, status);
```

The `key_hash` is the SHA-256 of the plaintext key—64 hex characters. The `status` field is either `active` or `revoked`. When we revoke a key, we set `status = 'revoked'` and record the timestamp in `revoked_at`. The row stays in the table for audit purposes.

### 

### **6.3 Storage Interface**

The storage layer is built around an interface, not a concrete implementation. The service layer calls `Create()`, `GetByHash()`, `List()`, `Revoke()`—it doesn't know or care whether PostgreSQL, Vault, or something else is behind those methods.

This matters for future extensibility. If an enterprise customer requires Vault for compliance reasons, we can add a Vault implementation of the interface without changing the service layer. But for Phase 1, PostgreSQL is the only implementation, and it's fully complete.

| Operation | Description |
| :---- | :---- |
| Create | Store key hash \+ metadata, return assigned ID |
| Get | Retrieve key metadata by ID |
| GetByHash | Retrieve key metadata by hash (validation path) |
| List | Return all keys for a user |
| Revoke | Mark key as revoked, record timestamp |

### **6.4 Future Extensibility (Phase 2+)**

**Out of scope for Phase 1\.** The storage interface enables future backends without hard dependencies.

Potential future backends: HashiCorp Vault, AWS Secrets Manager, Azure Key Vault, Google Cloud Secret Manager. Development will begin only after Phase 1 is complete and customer demand is validated.

### **6.5 System Design Parameters (Scale Planning)**

API key records are small (a hash, user ID, timestamps, and metadata), so storage overhead is negligible for any realistic deployment size. The critical performance path is key validation, which occurs on every API request. PostgreSQL with an index on `key_hash` provides fast lookups suitable for this workload. Key management operations (create, list, revoke) are infrequent by comparison.

## 

## **7\. API Specification**

### **7.1 Key Management Endpoints**

| Method | Path | Description |
| :---- | :---- | :---- |
| POST | /v1/api-keys | Create new key (returns plaintext once) |
| GET | /v1/api-keys | List user's keys (metadata only) |
| GET | /v1/api-keys/{id} | Get specific key metadata |
| PATCH | /v1/api-keys/{id} | Update name/description |
| DELETE | /v1/api-keys/{id} | Revoke key |

### **7.2 Create Key**

**Request:**

```
POST /v1/api-keys
{
  "name": "Production backend",
  "description": "Used by payment service"
}
```

**Response (201 Created):**

```
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "key": "sk-oai-a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0",
  "name": "Production backend",
  "description": "Used by payment service",
  "created_at": "2026-02-10T12:00:00Z",
  "status": "active"
}
```

⚠️ The `key` field is returned **only once**. Store it securely.

### **7.3 List Keys**

**Request:**

```
GET /v1/api-keys
```

**Response (200 OK):**

```
{
  "keys": [tf
    {
      "id": "550e8400-e29b-41d4-a716-446655440000",
      "name": "Production backend",
      "key_prefix": "sk-oai-a1b2...",
      "created_at": "2026-02-10T12:00:00Z",
      "status": "active"
    },
    {
      "id": "660e8400-e29b-41d4-a716-446655440001",
      "name": "CI pipeline",
      "key_prefix": "sk-oai-x9y8...",
      "created_at": "2026-02-09T10:00:00Z",
      "status": "revoked",
      "revoked_at": "2026-02-10T08:00:00Z"
    }
  ]
}
```

Note: `key_prefix` shows first few characters for identification. Full key is never returned after creation.

### **7.4 Get Key**

**Request:**

```
GET /v1/api-keys/550e8400-e29b-41d4-a716-446655440000
```

**Response (200 OK):**

```
{
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "name": "Production backend",
  "description": "Used by payment service",
  "key_prefix": "sk-oai-a1b2...",
  "created_at": "2026-02-10T12:00:00Z",
  "status": "active"
}
```

### **7.5 Update Key**

**Request:**

```
PATCH /v1/api-keys/550e8400-e29b-41d4-a716-446655440000
{
  "name": "Production backend v2",
  "description": "Updated description"
}
```

**Response (200 OK):** Updated key object (same as Get Key response)

### **7.6 Revoke Key**

**Request:**

```
DELETE /v1/api-keys/550e8400-e29b-41d4-a716-446655440000
```

**Response:** `204 No Content`

### **7.7 Internal Validation Endpoint**

This endpoint is called by Authorino to validate API keys on incoming requests.

| Field | Value |
| :---- | :---- |
| Method | POST |
| Path | /internal/v1/api-keys/validate |
| Access | Cluster-internal (NetworkPolicy protected) |

**Request:** `{ "key": "sk-oai-..." }`

**Response (valid):** `{ "valid": true, "user_id": "...", "key_id": "..." }`

**Response (invalid):** `{ "valid": false, "reason": "not_found" | "revoked" }`

**Why is this endpoint needed?** Authorino uses HTTP callbacks to validate custom credential formats. It cannot directly query PostgreSQL. The MaaS API acts as the validation layer.

### **7.8 Admin Endpoints**

**List all keys (with optional filters):**

```
GET /v1/admin/api-keys?user_id=alice@example.com&status=active
```

**Response:** Same format as List Keys (7.3), but includes all users' keys.

**Revoke any key:**

```
DELETE /v1/admin/api-keys/550e8400-e29b-41d4-a716-446655440000
```

**Response:** `204 No Content`

**Future:** Bulk operations (e.g., revoke all user's keys) deferred to Phase 2\.

## **8\. Key Format Specification**

This section provides the technical specification for API key generation, storage, and validation.

### **8.1 Format Structure**

Our API keys follow a simple, recognizable format:

```
sk-oai-a1b2c3d4e5f6g7h8i9j0k1l2m3n4o5p6q7r8s9t0
```

The key consists of three parts: a fixed prefix `sk-` (following the convention established by Stripe and OpenAI), an identifier `oai` for OpenShift AI, and a random string generated from 256 bits of cryptographic entropy. The total length is approximately 48 characters.

This format was chosen deliberately. Unlike Kubernetes JWT tokens (which are 900+ characters of base64-encoded JSON), our keys are short enough to paste into configuration files, recognizable enough to spot in logs, and prefixed so developers immediately know what they're looking at. The format matches what developers already expect from services like OpenAI (`sk-proj-...`), Anthropic (`sk-ant-...`), and Stripe (`sk_live_...`).

## **9\. Database Schema**

### **9.1 API Keys Table**

```
CREATE TABLE api_keys (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    user_id         VARCHAR(255) NOT NULL,
    key_hash        VARCHAR(64) NOT NULL UNIQUE,  -- SHA-256 hex
    name            VARCHAR(255) NOT NULL,
    description     TEXT,
    status          VARCHAR(20) NOT NULL DEFAULT 'active',  -- 'active' | 'revoked'
    created_at      TIMESTAMP NOT NULL DEFAULT NOW(),
    revoked_at      TIMESTAMP,

    -- Indexes for common access patterns
    CONSTRAINT idx_key_hash UNIQUE (key_hash)
);

-- Fast validation lookups (hot path)
CREATE INDEX idx_api_keys_hash_status ON api_keys (key_hash, status);

-- List user's keys
CREATE INDEX idx_api_keys_user ON api_keys (user_id, status);
```

**Design notes:**

* `key_hash` is indexed for O(1) validation lookups on every inference request  
* `key_prefix` (e.g., "sk-oai-a1b2...") is computed at runtime, not stored  
* `status` uses soft-delete pattern—revoked keys remain for audit trail  
* `user_id` is a string; format depends on auth source (e.g., `alice`, `alice@example.com`, `system:serviceaccount:ns:name`)

**Future enhancement:** Separate audit table for detailed operation logging (create, update, revoke events with timestamps and actor info).

## 

## **10\. CRD Specifications**

### **10.1 ModelsAsService CRD Changes**

We extend the existing `ModelsAsService` CRD to include API key configuration rather than introducing a new CRD. This keeps all MaaS-related settings together and simplifies operator logic.

```xml
apiVersion: maas.opendatahub.io/v1alpha1
kind: ModelsAsService
metadata:
  name: maas-config
spec:
  authentication:
    apiKey:
      enabled: true                          # Enable API key authentication
      storage:
        postgresql:
          connectionSecretRef:
            name: maas-db-credentials        # Secret containing DB connection string
      keyFormat:
        prefix: "sk-oai-"                    # Customizable key prefix (default)
```

The MaaS operator watches this CRD and generates the appropriate Authorino `AuthPolicy` resources automatically. No separate gateway configuration is required—the operator handles the translation from declarative config to Authorino policies.

## 

## **11\. Authorino Integration**

Authorino is the policy engine at our gateway layer that validates credentials before allowing access to model endpoints. We configure it to support API keys alongside existing JWT/OIDC authentication.

### **11.1 How It Works**

When a request arrives at the gateway, Authorino examines the `Authorization` header. If the token starts with `sk-oai-`, Authorino calls our internal validation endpoint to verify it. If valid, Authorino injects identity headers (`X-MaaS-User-Id`, `X-MaaS-Key-Id`) and allows the request to proceed. Standard JWTs continue through normal OIDC validation.

![][image6]

**11.2 AuthPolicy Configuration**

The MaaS gateway's AuthPolicy uses Authorino's HTTP callback authentication to validate API keys:

```xml
apiVersion: authorino.kuadrant.io/v1beta2
kind: AuthPolicy
metadata:
  name: maas-gateway-auth
spec:
  authentication:
    api-key-auth:
      when:
        - selector: request.headers.authorization
          operator: matches
          value: "^Bearer sk-oai-.*"
      http:
        url: "http://maas-api.maas.svc:8080/internal/v1/api-keys/validate"
        method: POST
        body:
          selector: request.headers.authorization|split(' ')[1]
        credentials:
          in: custom_header
          name: X-Internal-Token
  response:
    success:
      headers:
        x-maas-user-id:
          selector: auth.identity.user_id
        x-maas-key-id:
          selector: auth.identity.key_id
```

Downstream model endpoints receive these identity headers and can use them for authorization without needing to re-validate the key.

### **11.3 Open Question: RBAC Integration**

**⚠️ Requires investigation:** The existing `odh-model-controller` RBAC uses Kubernetes identities (via TokenReview). API keys resolve to a `user_id` (e.g., `alice@example.com`), which is not a Kubernetes identity. We need to determine how API key users are authorized to access specific models.

**Possible Solution:** 

  1\. API key created → tier determined from original groups → stored in DB  
  2\. API key validated → returns synthetic group:  
  \["system:serviceaccounts:maas-default-gateway-tier-free"\]  
  3\. Authorino calls /v1/tiers/lookup with those synthetic groups  
  4\. Tier mapper matches the synthetic group (which was auto-appended to  
  tier's groups) → returns tier info  
  5\. Model access authorized based on tier
