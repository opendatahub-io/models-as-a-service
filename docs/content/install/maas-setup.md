# Install MaaS Components

Complete [Operator Setup](platform-setup.md) before proceeding.

**Installation flow:**

1. [Database Setup](#database-setup) — Create the PostgreSQL connection Secret
2. [Create Gateway](#create-gateway) — Deploy maas-default-gateway (required before modelsAsService)
3. [Configure DataScienceCluster](#configure-datasciencecluster) — Enable KServe and modelsAsService in your DataScienceCluster
4. [Model Setup](model-setup.md) — Deploy sample models
5. [Validation](validation.md) — Verify the deployment

## Database Setup

`maas-api` uses PostgreSQL as its persistence layer for API key metadata: hashed tokens, subscription bindings, expiration dates, and revocation state. The database must be reachable before `maas-api` starts; the pod will crash-loop until the connection succeeds and the schema migration completes.

Create the `maas-db-config` Secret in your ODH/RHOAI namespace (typically `opendatahub` for ODH or `redhat-ods-applications` for RHOAI):

```bash
kubectl create secret generic maas-db-config \
  -n opendatahub \
  --from-literal=DB_CONNECTION_URL='postgresql://username:password@hostname:5432/database?sslmode=require'
```

**Connection string format:**
```
postgresql://USERNAME:PASSWORD@HOSTNAME:PORT/DATABASE?sslmode=require
```

!!! note "Development"
    For development, you can deploy a PostgreSQL instance and Secret using the setup script:

    ```bash
    ./scripts/setup-database.sh
    ```

    **Setting the namespace:** The script defaults to `opendatahub`. Set the `NAMESPACE` environment variable if your MaaS deployment uses a different namespace:

    ```bash
    # RHOAI uses redhat-ods-applications
    NAMESPACE=redhat-ods-applications ./scripts/setup-database.sh

    # Custom namespace
    NAMESPACE=my-maas-namespace ./scripts/setup-database.sh
    ```

    The full `scripts/deploy.sh` script also creates PostgreSQL automatically when deploying MaaS.

!!! note "Using deploy.sh with an external database"
    If you use `scripts/deploy.sh`, you can supply your own PostgreSQL connection string with the `--postgres-connection` flag. This skips the built-in POC PostgreSQL deployment and creates the `maas-db-config` Secret automatically:

    ```bash
    ./scripts/deploy.sh --postgres-connection 'postgresql://username:password@hostname:5432/database?sslmode=require'
    ```

!!! note "Restarting maas-api"
    If you add or update the Secret after the DataScienceCluster already has modelsAsService in managed state, restart the maas-api deployment to pick up the config:

    ```bash
    kubectl rollout restart deployment/maas-api -n opendatahub
    ```

    This is not required when the Secret exists before enabling modelsAsService in your DataScienceCluster.

## Create Gateway

Create `maas-default-gateway` in `openshift-ingress` **before** enabling `modelsAsService` in your DataScienceCluster.

`scripts/deploy.sh` runs this step automatically in **route** mode. Run the script yourself when installing via DataScienceCluster first, using **clusterip** mode, or on disconnected clusters.

| Environment | Command |
|-------------|---------|
| ROSA, OSD, cloud (default) | `./scripts/setup-gateway.sh` |
| On-prem, bare-metal, disconnected | `INGRESS_MODE=clusterip ./scripts/setup-gateway.sh` |
| Air-gapped (no GitHub fetch) | `DISCONNECTED=true INGRESS_MODE=clusterip ./scripts/setup-gateway.sh` |

Common overrides: `CLUSTER_DOMAIN`, `CERT_NAME` (route mode only), `DRY_RUN`, `MAAS_MANIFEST_REF` (pinned git ref for remote kustomize fallback). For the full variable list, TLS auto-detection order, and examples, see [scripts/README.md](https://github.com/opendatahub-io/models-as-a-service/blob/main/scripts/README.md#setup-gatewaysh).

**Verify:**

```bash
kubectl wait --for=condition=Programmed gateway/maas-default-gateway -n openshift-ingress --timeout=120s
```

Expected output:
```
gateway.gateway.networking.k8s.io/maas-default-gateway condition met
```

For ClusterIP mode, also verify the Route:
```bash
kubectl get route maas-gateway-route -n openshift-ingress
```

Expected output:
```
NAME                  HOST/PORT                           PATH   SERVICES                           PORT   TERMINATION   WILDCARD
maas-gateway-route    maas.apps.example.cluster.com              maas-default-gateway-openshift-default   443    reencrypt     None
```

For ClusterIP architecture and troubleshooting, see [Gateway patterns — ClusterIP + Route](../configuration-and-management/gateway-patterns.md#clusterip-gateway-with-openshift-route-re-encrypt).

!!! note "Required annotations"
    The script applies `opendatahub.io/managed: "false"` and `security.opendatahub.io/authorino-tls-bootstrap: "true"` on the Gateway. See [TLS Configuration](../configuration-and-management/tls-configuration.md) for custom certificates and Authorino integration. The `authorino-tls-bootstrap` annotation is interim until [CONNLINK-528](https://issues.redhat.com/browse/CONNLINK-528).

!!! tip "Authorino TLS"
    After the Gateway exists, run `./scripts/setup-authorino-tls.sh`. For RHOAI/RHCL, set `AUTHORINO_NAMESPACE=rh-connectivity-link`.

## Configure DataScienceCluster

After creating the database Secret and Gateways, create or update your DataScienceCluster. Choose your deployment method:

=== "Managed (Recommended)"

    The operator deploys `maas-controller`, which self-bootstraps a `default-tenant` CR and reconciles the MaaS platform workloads (maas-api, gateway policies, telemetry). Create or update your DataScienceCluster with `modelsAsService` in Managed state:

    ```yaml
    kubectl apply -f - <<EOF
    apiVersion: datasciencecluster.opendatahub.io/v2
    kind: DataScienceCluster
    metadata:
      name: default-dsc
    spec:
      components:
        kserve:
          managementState: Managed
          rawDeploymentServiceConfig: Headed
          modelsAsService:
            managementState: Managed
        dashboard:
          managementState: Managed
        llamastackoperator:
          managementState: Managed
    EOF
    ```

    !!! tip "LlamaStack Operator (GenAI Studio)"
        The `llamastackoperator` component is required for **GenAI Studio** functionality.
        If you do not need GenAI Studio, you can omit it or set it to `Removed`.
        You also need to enable the `genAiStudio` feature flag on `OdhDashboardConfig` —
        see [OdhDashboardConfig Feature Flags](#odhdashboardconfig-feature-flags) below.

    !!! note "Connectivity Link warning (ODH with Kuadrant)"
        When using ODH with Kuadrant (upstream), you may see `Warning: Red Hat Connectivity Link is not installed, LLMInferenceService cannot be used` in the Kserve status initially. This typically resolves after a few minutes as the operator reconciles. If it persists, run `kubectl describe datasciencecluster default-dsc` and check that the Kuadrant/Connectivity Link operator is installed and healthy.

    **Validate DataScienceCluster:**

    ```bash
    # Check DataScienceCluster status
    kubectl get datasciencecluster default-dsc

    # Wait for KServe and ModelsAsService to be ready (optional)
    kubectl wait --for=jsonpath='{.status.conditions[?(@.type=="KserveReady")].status}'=True \
      datasciencecluster/default-dsc --timeout=300s
    kubectl wait --for=jsonpath='{.status.conditions[?(@.type=="ModelControllerReady")].status}'=True \
      datasciencecluster/default-dsc --timeout=300s

    # Verify maas-api deployment is running (use opendatahub for ODH, redhat-ods-applications for RHOAI)
    kubectl get deployment maas-api -n opendatahub
    kubectl rollout status deployment/maas-api -n opendatahub --timeout=120s
    ```

    The `maas-controller` (deployed by the operator) will automatically create a `default-tenant` CR and reconcile:

    - **MaaS API** (Deployment, Service, ServiceAccount, ClusterRole, ClusterRoleBinding, HTTPRoute)
    - **MaaS API AuthPolicy** (maas-api-auth-policy) - Protects the MaaS API endpoint
    - **Gateway default AuthPolicy** (gateway-default-auth) - Denies unauthenticated traffic
    - **Gateway default TokenRateLimitPolicy** (gateway-default-deny) - Denies unsubscribed traffic
    - **TelemetryPolicy and Istio Telemetry** (when telemetry is enabled)
    - **DestinationRule** (when TLS is enabled)
    - **NetworkPolicy** (maas-authorino-allow) - Allows Authorino to reach MaaS API

    ### Tenant CR

    With `modelsAsService` **Managed**, the [Open Data Hub operator](https://github.com/opendatahub-io/opendatahub-operator) deploys `maas-controller`, which self-bootstraps a **namespace-scoped** `Tenant` object on startup. The resource name **must** be `default-tenant` (enforced via CEL validation). The `Tenant` CR lives in the `models-as-a-service` namespace (same namespace as `MaaSSubscription` and `MaaSAuthPolicy`). The authoritative API definition is in the maas-controller repo: [`tenant_types.go`](https://github.com/opendatahub-io/models-as-a-service/blob/main/maas-controller/api/maas/v1alpha1/tenant_types.go).

    **Nothing in `spec` is required for a default install.** If you omit `spec`, the controller uses the same defaults as this guide: Gateway **`openshift-ingress` / `maas-default-gateway`**, and telemetry metric toggles use the defaults described below.

    | Field | What to set |
    | ----- | ----------- |
    | `spec.gatewayRef.namespace` | Namespace of your Gateway API `Gateway` (default `openshift-ingress`). |
    | `spec.gatewayRef.name` | Name of that `Gateway` (default `maas-default-gateway`). Set these if your MaaS hostname is exposed through a different Gateway than the default. |
    | `spec.apiKeys.maxExpirationDays` | Maximum allowed API key lifetime in **days**. When set, users cannot mint keys with a longer lifetime than this value (via `expiresIn`). Optional; if unset, the controller does not apply a cap through this field (see also `maas-api` / `API_KEY_MAX_EXPIRATION_DAYS` in your deployment). |
    | `spec.externalOIDC.issuerUrl` | OIDC issuer URL for external identity provider (optional; enables OIDC on the maas-api AuthPolicy). |
    | `spec.externalOIDC.clientId` | OIDC client ID (required when `issuerUrl` is set). |
    | `spec.telemetry.enabled` | Enable TelemetryPolicy and Istio Telemetry (default `true`). |
    | `spec.telemetry.metrics.captureOrganization` | Include `organization_id` on metrics (default `true`). |
    | `spec.telemetry.metrics.captureUser` | Include user labels on metrics (default `false`; privacy-sensitive). |
    | `spec.telemetry.metrics.captureGroup` | Include group labels on metrics (default `false`; higher cardinality). |
    | `spec.telemetry.metrics.captureModelUsage` | Include model labels on usage metrics (default `true`). |

    Example (patch common values):

    ```yaml
    apiVersion: maas.opendatahub.io/v1alpha1
    kind: Tenant
    metadata:
      name: default-tenant
      namespace: models-as-a-service
    spec:
      gatewayRef:
        namespace: openshift-ingress
        name: maas-default-gateway
      apiKeys:
        maxExpirationDays: 90
      telemetry:
        enabled: true
        metrics:
          captureUser: false
          captureGroup: false
    ```

    ```bash
    kubectl apply -f tenant.yaml
    kubectl get tenant default-tenant -n models-as-a-service -o yaml
    ```

=== "Kustomize"

    !!! note "Development and early testing"
        Kustomize deployment can be used for **development and early testing purposes**. For production, use the Managed tab above.

    Set `modelsAsService` to **Removed** so the operator does not deploy the MaaS API, then deploy MaaS directly from the canonical build root:

    ```yaml
    kubectl apply -f - <<EOF
    apiVersion: datasciencecluster.opendatahub.io/v2
    kind: DataScienceCluster
    metadata:
      name: default-dsc
    spec:
      components:
        kserve:
          managementState: Managed
          rawDeploymentServiceConfig: Headed
          modelsAsService:
            managementState: Removed
        dashboard:
          managementState: Managed
    EOF
    ```

    Deploy the MaaS controller (run from the project root; ensure the `maas-db-config` Secret exists per [Database Setup](#database-setup)):

    ```bash
    kubectl apply -k deployment/base/maas-controller/default
    ```

    On a fresh cluster, use **`./scripts/deploy.sh`** (or run `install_maas_controller_crds_and_wait` from `scripts/deployment-helpers.sh` before applying) so CRDs are **Established** before `Config` and other CRs in the same bundle.

!!! tip "Troubleshooting"
    If components do not become ready, run `kubectl describe datasciencecluster default-dsc` to inspect conditions and events.

## OdhDashboardConfig Feature Flags

The RHOAI Dashboard uses feature flags in the `OdhDashboardConfig` resource to control which tabs
and features are visible in the UI. The operator creates this resource automatically when the
Dashboard component is deployed, but the following flags may need to be enabled manually.

Patch the `OdhDashboardConfig` in your applications namespace (typically `redhat-ods-applications`
for RHOAI or `opendatahub` for ODH):

```bash
kubectl patch odhdashboardconfig odh-dashboard-config \
  -n redhat-ods-applications --type=merge \
  -p '{"spec":{"dashboardConfig":{"genAiStudio":true,"observabilityDashboard":true}}}'
```

| Flag | Effect | Prerequisites |
|------|--------|---------------|
| `genAiStudio: true` | Shows the **GenAI Studio** tab in the Dashboard | `llamastackoperator` set to `Managed` in DSC |
| `observabilityDashboard: true` | Shows the **Observability** tab in the Dashboard | COO, OpenTelemetry Operator installed; DSCI `monitoring.metrics` configured |

!!! note "Namespace"
    For ODH installations, replace `redhat-ods-applications` with `opendatahub` (or your configured
    applications namespace from DSCI `spec.applicationsNamespace`).

!!! note "Flag names"
    These flags are defined in the odh-dashboard source. The `OdhDashboardConfig` CRD is of
    API version `opendatahub.io/v1alpha`. The operator re-creates this resource with factory
    defaults if it is deleted, but does not overwrite user changes to existing fields.

## Next steps

* **Deploy models.** See [Model Setup](model-setup.md) for sample model deployments.
* **Perform validation.** Follow the [validation guide](validation.md) to verify that
  MaaS is working correctly.
