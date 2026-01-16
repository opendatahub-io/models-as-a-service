# IDP Deployment (Keycloak OAuth)

This document describes the optional Keycloak IDP deployment for MaaS. It is additive and does not change the default deployment unless explicitly enabled.

## Scope and Safety

- The default deployment path is unchanged when `ENABLE_KEYCLOAK_IDP` is unset or `false`.
- Keycloak-related manifests are stored separately under `deployment/idp/`.
- Base manifests under `deployment/base/` remain vanilla (OpenShift identities, default tier mapping).

## Enable IDP Mode

Run the deployment with the feature flag enabled:

```bash
export ENABLE_KEYCLOAK_IDP=true
./scripts/deploy-rhoai-stable.sh
```

For the generic OpenShift script:

```bash
export ENABLE_KEYCLOAK_IDP=true
./scripts/deploy-openshift.sh
```

## What Changes With IDP Enabled

Only when `ENABLE_KEYCLOAK_IDP=true`, the scripts will:

- Deploy Keycloak in `keycloak-system` and import the `maas` realm.
- Apply the Keycloak AuthPolicy in the MaaS API namespace.
- Apply Keycloak group-to-tier mappings in both `maas-api` and `opendatahub` namespaces.
- Exclude `/maas-api` paths from the gateway AuthPolicy.
- Switch MaaS API image to `quay.io/opendatahub/maas-api:latest` unless overridden.

## Key Files (IDP Only)

- `deployment/idp/maas-api/policies/auth-policy-oidc.yaml`
- `deployment/idp/maas-api/resources/tier-mapping-configmap.yaml`
- `deployment/idp/maas-api/resources/allow-gateway-networkpolicy.yaml`

## Base Deployment (Unchanged)

When `ENABLE_KEYCLOAK_IDP=false`, these remain active:

- `deployment/base/maas-api/policies/auth-policy.yaml` (OpenShift token review)
- `deployment/base/maas-api/resources/tier-mapping-configmap.yaml` (default groups)

## Validate IDP Flow

Use the validation steps printed by `./scripts/deploy-rhoai-stable.sh` or follow `install/keycloak-idp.md` for the full end-to-end flow:

- Keycloak token for MaaS API access (`/maas-api/v1/tokens`, `/maas-api/v1/models`)
- MaaS ServiceAccount token for inference

## Token Minting + Inference Flow

This flow is the same regardless of deployment script; the IDP path just replaces the initial auth token with a Keycloak JWT.

1) User authenticates to Keycloak and receives an access token (JWT).
2) User calls `POST /maas-api/v1/tokens` with the Keycloak token.
3) `maas-api-auth-policy` validates the JWT and injects identity headers.
4) MaaS API maps Keycloak groups to a tier from the tier-to-group mapping ConfigMap.
5) MaaS API creates a Kubernetes ServiceAccount for the user in the tier namespace and mints a ServiceAccount token.
6) User calls the model inference endpoint with the MaaS ServiceAccount token.
7) `gateway-auth-policy` validates the ServiceAccount token via Kubernetes TokenReview/SAR.
8) Limitador enforces tier-based rate limits before forwarding to the model.

Kubernetes ServiceAccount mapping:
- Each Keycloak user is mapped to a ServiceAccount created by MaaS API.
- The ServiceAccount is created in a tier namespace (for example, `maas-default-gateway-tier-free`).
- The MaaS token returned from `/maas-api/v1/tokens` is a ServiceAccount token.

## Customization

You can override Keycloak settings and the MaaS API image before running the scripts:

```bash
export KEYCLOAK_REALM="maas"
export KEYCLOAK_CLIENT_ID="maas-cli"
export KEYCLOAK_CLIENT_SECRET="maas-cli-secret"
export MAAS_API_IMAGE="quay.io/opendatahub/maas-api:latest"
```
