# Keycloak PoC: Admin ServiceAccount Hack

## Overview

For the Keycloak PoC, we're using a "hack" approach to work around the fact that model authorization checks require ServiceAccount tokens for Kubernetes RBAC validation, but we're using Keycloak for user authentication.

## Implementation

### Admin ServiceAccount

- **Name**: `maas-keycloak-admin`
- **Location**: Created in tier namespaces (e.g., `maas-default-gateway-tier-free`, `maas-default-gateway-tier-premium`)
- **Purpose**: Used for model authorization checks when Keycloak is enabled

### How It Works

1. **Model Listing (`GET /v1/models`)**:
   - User authenticates with Keycloak token
   - Gateway AuthPolicy validates Keycloak token
   - `maas-api` receives request with user context (username, groups)
   - For each model, `doAuthCheck()` is called
   - **Keycloak mode**: Swaps Keycloak token for admin SA token from user's tier namespace
   - Makes HTTP request to model endpoint with admin SA token
   - Gateway validates admin SA token (or Keycloak token if swap fails)
   - Model endpoint performs RBAC check using admin SA token

2. **Chat Completions (`POST /llm/{model}/v1/chat/completions`)**:
   - **IMPORTANT**: AuthPolicy **CANNOT** swap tokens for chat requests
   - User makes request directly to model endpoint with Keycloak token
   - Gateway AuthPolicy validates Keycloak token ✅
   - Request reaches model endpoint with Keycloak token
   - **Problem**: Model endpoint needs ServiceAccount token for RBAC checks
   - **Current Status**: This will fail RBAC checks unless model endpoint accepts Keycloak tokens

## IMPORTANT: Chat Completion Limitation

**AuthPolicy (Authorino/Kuadrant) cannot modify the `Authorization` header that goes downstream to the backend.**

This means:
- ✅ Gateway can validate Keycloak tokens
- ✅ Gateway can add headers (X-MaaS-Username, X-MaaS-Group)
- ❌ Gateway **cannot** replace the Authorization header with a ServiceAccount token

### Workarounds for Chat Completions

1. **Proxy Endpoint** (Recommended for PoC):
   - Create a proxy endpoint in `maas-api`: `POST /v1/proxy/chat/{model}`
   - User sends request to proxy with Keycloak token
   - Proxy validates Keycloak token (via AuthPolicy)
   - Proxy swaps to admin SA token
   - Proxy forwards request to model endpoint with admin SA token
   - Model endpoint validates admin SA token via RBAC ✅

2. **Model Endpoint Accepts Keycloak Tokens**:
   - Modify model endpoint to accept Keycloak tokens directly
   - Skip RBAC checks (rely on Gateway AuthPolicy only)
   - **Risk**: Less granular authorization control

3. **Future: Proper Subscription System**:
   - Implement proper user-to-ServiceAccount mapping
   - Create per-user ServiceAccounts in tier namespaces
   - Use those SAs for all requests (not just admin SA)

## Code Locations

### Admin SA Token Generation
- `maas-api/internal/token/manager.go`: `GetAdminServiceAccountToken()`
- Creates/gets admin SA in tier namespace
- Generates token with correct audience

### Token Swapping in Model Checks
- `maas-api/internal/models/kserve_llmisvc.go`: `doAuthCheck()`
- Detects Keycloak mode
- Gets admin SA token for user's tier
- Uses admin SA token for authorization check

### Tier Determination
- `maas-api/internal/models/kserve_llmisvc.go`: `determineTierFromGroups()`
- Extracts tier name from user groups (e.g., `tier-free-users` → `free`)

## Configuration

The admin SA is automatically created when needed:
- Namespace: Determined from user's tier (e.g., `maas-default-gateway-tier-free`)
- Name: `maas-keycloak-admin`
- Labels: Include tier information for identification

## Future Improvements

1. **Proper User-to-SA Mapping**: Create per-user ServiceAccounts instead of shared admin SA
2. **Chat Proxy**: Implement proxy endpoint for chat completions
3. **Token Caching**: Cache admin SA tokens to reduce API calls
4. **Tier-Based Model Access**: Use tier information to filter models before authorization checks
