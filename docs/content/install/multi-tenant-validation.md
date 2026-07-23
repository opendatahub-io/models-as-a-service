# Multi-Tenant Validation

Validation steps for additional tenants. For default (single-tenant) validation, see [Validation](validation.md).

## 1. Verify AITenant Status

```bash
TENANT_NAME="red-team"
oc get aitenant ${TENANT_NAME} -n ai-tenants
```

Expected: `READY` is `True`. If `False`, check conditions:

```bash
oc get aitenant ${TENANT_NAME} -n ai-tenants -o jsonpath='{.status.conditions}' | jq .
```

## 2. Verify Tenant Namespace

```bash
TENANT_NS="ai-tenant-${TENANT_NAME}"
oc get namespace ${TENANT_NS} -o jsonpath='{.metadata.labels}' | jq .
```

Expected labels:

- `ai-gateway.opendatahub.io/tenant: <tenant-name>`
- `maas.opendatahub.io/managed-by-aitenant: "true"`

Verify the Tenant CR:

```bash
oc get tenant default-tenant -n ${TENANT_NS} -o yaml
```

Expected: `status.phase` is `Active`.

## 3. Verify maas-api Deployment

```bash
INFRA_NS=$(oc get deployment -A -o custom-columns=NS:.metadata.namespace,NAME:.metadata.name --no-headers | grep "maas-api-${TENANT_NAME}" | awk '{print $1}')
oc get deployment maas-api-${TENANT_NAME} -n ${INFRA_NS}
```

Expected: `READY` is `1/1`.

## 4. Verify Gateway

```bash
oc get gateway ${TENANT_NAME} -n openshift-ingress
```

Expected: `PROGRAMMED` is `True`.

Verify the Route exists:

```bash
oc get route ${TENANT_NAME}-gateway -n openshift-ingress
```

## 5. Verify Policies

```bash
oc get authpolicy ${TENANT_NAME}-maas-auth -n openshift-ingress
oc get tokenratelimitpolicy -n ${TENANT_NS}
```

## 6. Test Model Listing

```bash
GATEWAY_HOST=$(oc get gateway ${TENANT_NAME} -n openshift-ingress -o jsonpath='{.spec.listeners[0].hostname}')
TOKEN=$(oc whoami -t)

curl -sSk "https://${GATEWAY_HOST}/maas-api/v1/models" \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" | jq .
```

Expected: `200` with models configured for this tenant.

## 7. Test Authentication

Without a token (expected: `401`):

```bash
curl -sSk -o /dev/null -w "%{http_code}\n" "https://${GATEWAY_HOST}/maas-api/v1/models"
```

With a valid API key (expected: `200`):

```bash
API_KEY=$(curl -sSk \
  -H "Authorization: Bearer ${TOKEN}" \
  -H "Content-Type: application/json" \
  -X POST -d '{"name":"test-key","subscription":"my-subscription"}' \
  "https://${GATEWAY_HOST}/maas-api/v1/api-keys" | jq -r .key)

curl -sSk -w "\nHTTP: %{http_code}\n" \
  -H "Authorization: Bearer ${API_KEY}" \
  "https://${GATEWAY_HOST}/maas-api/v1/models"
```

## 8. Test Tenant Isolation

Verify that a token minted for one tenant cannot access another tenant's models:

```bash
# Get default tenant gateway host
DEFAULT_HOST=$(oc get gateway maas-default-gateway -n openshift-ingress -o jsonpath='{.spec.listeners[0].hostname}')

# Try the additional tenant's API key against the default tenant (expected: 401 or 403)
curl -sSk -o /dev/null -w "%{http_code}\n" \
  -H "Authorization: Bearer ${API_KEY}" \
  "https://${DEFAULT_HOST}/maas-api/v1/models"
```

Each tenant's maas-api instance serves only its own tenant's data. API keys are scoped to the tenant where they were created.

## 9. Verify All Components

```bash
echo "=== AITenant ==="
oc get aitenant ${TENANT_NAME} -n ai-tenants

echo "=== Tenant CR ==="
oc get tenant default-tenant -n ${TENANT_NS}

echo "=== maas-api ==="
oc get deployment maas-api-${TENANT_NAME} -n ${INFRA_NS}

echo "=== Gateway ==="
oc get gateway ${TENANT_NAME} -n openshift-ingress

echo "=== AuthPolicies ==="
oc get authpolicy ${TENANT_NAME}-maas-auth -n openshift-ingress

echo "=== Subscriptions ==="
oc get maassubscription -n ${TENANT_NS}

echo "=== Model Refs ==="
oc get maasmodelref -n ${TENANT_NS}
```

## Troubleshooting

### AITenant stuck in Pending

Check the AITenant conditions for the specific reason:

```bash
oc get aitenant ${TENANT_NAME} -n ai-tenants -o jsonpath='{.status.conditions}' | jq .
```

Common causes:

- Gateway not found or not Programmed
- Database secret (`maas-db-config`) missing in operator namespace
- Authorino not deployed or TLS not configured

### Webhook rejects AITenant creation

AITenant must be created in the configured infrastructure namespace (default: `ai-tenants`):

```
AITenant ai-tenants/red-team must be created in the configured AITenant infrastructure namespace ai-tenants
```

### Gateway uniqueness error

Each AITenant requires its own Gateway:

```
gateway openshift-ingress/red-team is already in use by AITenant ai-tenants/other-tenant
```

### MaaSSubscription rejected

MaaSSubscription and MaaSAuthPolicy must be created in a namespace that contains a `Tenant` CR. Wait for the AITenant controller to create the Tenant CR before creating these resources.

## See Also

- [Multi-Tenant Setup](multi-tenant-setup.md)
- [Validation (Default Tenant)](validation.md)
- [Tenant RBAC](../configuration-and-management/tenant-rbac.md)
- [AITenant CRD Reference](../reference/crds/ai-tenant.md)
