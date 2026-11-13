# MaaS Portal — Testing & Validation Guide (UI + Backend)

This guide walks you through validating the MaaS consumer portal end to end:
logging in through the UI, and calling the backend API with a token. It is
written for people who are new to this feature — no deep background assumed.

Everything here was validated on a RHOAI dev cluster. Wherever a URL appears,
we show you how to discover it first, because hostnames differ per cluster.

---

## 0. Find your cluster's base domain first

Every URL in this guide is built from the cluster's apps domain. Get it once
and reuse it:

```sh
APPS_DOMAIN=$(oc get route console -n openshift-console \
  -o jsonpath='{.spec.host}' | cut -d'.' -f2-)
echo $APPS_DOMAIN      # e.g. apps.my-cluster.example.com
```

From now on:

| Thing | URL |
|---|---|
| Portal UI | `https://maas.$APPS_DOMAIN/maas-portal` |
| Keycloak (login server) | `https://keycloak.$APPS_DOMAIN` |
| Backend API (default tenant) | `https://maas.$APPS_DOMAIN/maas-api` |

If you create extra tenants (see section 5), each gets its own hostname like
`https://<tenant>-maas.$APPS_DOMAIN`.

---

## 1. Prerequisites (one-time cluster setup)

**a) Keycloak with test realms.** The repo ships ready-made test users and
clients. From the models-as-a-service repo root:

```sh
./scripts/setup-keycloak.sh
./docs/samples/install/keycloak/test-realms/apply-test-realms.sh
```

After this you have two realms (`tenant-a`, `tenant-b`). All test users have
password `letmein`. The portal client in realm `tenant-a` is
`maas-consumer-portal` with secret `maas-e2e-portal-client-secret`.

> Both test realms ship the `maas-consumer-portal` client with the same test
> secret (tenant-b gained it in models-as-a-service#1864).

**b) Enable the MaaS Portal on the Dashboard.** This is switched off by
default. The setting lives on the DataScienceCluster CR:

```sh
oc patch dsc default-dsc --type merge \
  -p '{"spec":{"components":{"dashboard":{"maasPortal":{"managementState":"Managed"}}}}}'
```

Until this is `Managed`, the portal login simply will not exist — you get a
plain 401 at the portal URL.

**c) Trust the Keycloak certificate.** On dev clusters Keycloak uses the
cluster's own certificate, which most components don't trust by default. Add
it to the DSCInitialization trust bundle:

```sh
# extract the cluster ingress CA
oc extract secret/router-certs-default -n openshift-ingress --to=- 2>/dev/null \
  | awk '/BEGIN CERT/,/END CERT/' > /tmp/ingress-ca.pem

# add it to the DSCI trusted CA bundle (one JSON-escaped line)
oc patch dsci dscinitialization -p "{\"spec\":{\"trustedCABundle\":{\"customCABundle\":\"$(cat /tmp/ingress-ca.pem | awk '{printf "%s\\n", $0}')\"}}}" --type merge
```

---

## 2. Configure OIDC on the AITenant

The portal needs to know which login server to use. Point the default tenant
at the Keycloak realm:

```sh
# create the client-credential Secret in the tenant namespace
oc create secret generic demo-portal-client \
  -n models-as-a-service \
  --from-literal=client-secret=maas-e2e-portal-client-secret

# tell the AITenant about the issuer and the secret
oc patch aitenant models-as-a-service -n ai-tenants --type merge -p '{
  "spec": {
    "oidc": {
      "issuerUrl": "https://keycloak.'"$APPS_DOMAIN"'/realms/tenant-a",
      "clientId": "maas-consumer-portal",
      "clientSecretRef": {"name": "demo-portal-client", "key": "client-secret"}
    }
  }
}'
```

**Check the issuer is healthy** (this is the `OIDCReady` condition):

```sh
oc get aitenant models-as-a-service -n ai-tenants \
  -o jsonpath='{.status.conditions[?(@.type=="OIDCReady")]}' | jq
```

You want `"status":"True","reason":"Available"`. If you see `TLSUntrusted`,
re-check step 1c. Changes to the CA bundle are picked up automatically within
~5 minutes — you don't need to edit the AITenant again.

---

## 3. Force the gateway auth policy to refresh (known issue)

**This is a known bug, not something you did wrong.** The gateway's auth
policy is built once when the tenant is created. If you add or change OIDC
afterwards, the policy does not update by itself, and every login/token fails
with 401. Workaround — delete it and let the controller rebuild it with the
current OIDC config (takes a few seconds):

```sh
oc delete authpolicy maas-gateway-auth -n openshift-ingress
```

Verify it came back with the OIDC identity:

```sh
oc get authpolicy maas-gateway-auth -n openshift-ingress -o json \
  | jq '.spec.defaults.rules.authentication | keys'
# should list: "api-keys", "oidc-identities", "openshift-identities"
```

---

## 4. Test the UI login (browser)

1. Open `https://maas.$APPS_DOMAIN/maas-portal`
2. You should be redirected to a Keycloak login form (the URL will contain
   `/realms/tenant-a/`) — **not** a bare 401
3. Log in as `portal_user` / `letmein` (other users: `alice_lead`, `bob_sre`,
   `dave_noaccess` — they have different permissions, useful for testing)
4. You should land on the portal dashboard, signed in

If step 2 gives 401 instead of a redirect: the portal is usually not enabled (re-check 1b), or the gateway auth policy is stale (re-check section 3).

---

## 5. Test the backend API (command line)

Get a token from Keycloak the way an automated client would, then call the
API through the gateway:

```sh
# 1. get a token
TOKEN=$(curl -sk -X POST \
  "https://keycloak.$APPS_DOMAIN/realms/tenant-a/protocol/openid-connect/token" \
  -d grant_type=password \
  -d client_id=maas-consumer-portal \
  -d client_secret=maas-e2e-portal-client-secret \
  -d username=portal_user -d password=letmein \
  | jq -r .access_token)

# 2. call the API
curl -sk "https://maas.$APPS_DOMAIN/v1/me" \
  -H "Authorization: Bearer $TOKEN"
```

A JSON response with your user info means the full chain works:
Keycloak → gateway → auth check → backend.

**Reading the response:**
- `200` + JSON → working
- `401` + JSON with `x-ext-auth-reason` header → authentication failed
  (see Troubleshooting)
- plain-text `404 page not found` → auth actually PASSED, you just used a
  wrong path. Valid paths: `/v1/me`, `/v1/models`, `/v1/subscriptions`,
  `/v1/api-keys`, `/maas-api/v1/*`, `/maas-api/health`

### Testing a second tenant

```sh
ALLOWED_ROUTE_NAMESPACES="redhat-ods-applications" \
  ./scripts/create-ai-tenant.sh team-b team-b-maas.$APPS_DOMAIN
```

Then repeat section 2 for the `team-b` AITenant (realm `tenant-b`, users
`charlie_sec_lead` / `grace_dev`), but note:
- the portal **login page** only exists for the default tenant today (by
  design); other tenants are API-only
- a new tenant's Gateway only accepts routes from its own namespace by
  default, but its HTTPRoutes are created in the shared infra namespace — open
  the listener with `allowedRoutes: {namespaces: {from: All}}` or use a label
  selector, and label the infra namespace for it
- its gateway AuthPolicy is built once at creation with whatever OIDC config
  exists then — configure OIDC before creating the tenant, or apply the
  section 3 workaround on its per-tenant AuthPolicy

---
