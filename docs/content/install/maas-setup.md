# Install MaaS Components

Complete [Operator Setup](platform-setup.md) before proceeding.

**Installation flow:**

1. [Gateway Setup](gateway-setup.md) — Deploy maas-default-gateway (must be completed first)
2. [Database Setup](#database-setup) — Create the PostgreSQL connection Secret
3. [Configure DataScienceCluster](#configure-datasciencecluster) — Enable KServe and modelsAsService in your DataScienceCluster
4. [Model Setup (On Cluster)](model-setup.md) — Deploy sample models
5. [Validation](validation.md) — Verify the deployment

## Database Setup

A PostgreSQL database is required. Create the `maas-db-config` Secret in your ODH/RHOAI namespace (typically `opendatahub` for ODH or `redhat-ods-applications` for RHOAI):

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

    Use `NAMESPACE=redhat-ods-applications` for RHOAI. The full `scripts/deploy.sh` script also creates PostgreSQL automatically when deploying MaaS.

!!! note "Restarting maas-api"
    If you add or update the Secret after the DataScienceCluster already has modelsAsService in managed state, restart the maas-api deployment to pick up the config:

    ```bash
    kubectl rollout restart deployment/maas-api -n opendatahub
    ```

    This is not required when the Secret exists before enabling modelsAsService in your DataScienceCluster.

## Configure DataScienceCluster

!!! warning "Gateway Required"
    The `maas-default-gateway` must exist and be in `Programmed` state before proceeding.
    If you have not created it yet, complete [Gateway Setup](gateway-setup.md) first.

After creating the database Secret and Gateway, create or update your DataScienceCluster. Choose your deployment method:

=== "Managed (Recommended)"

    The operator deploys and manages the MaaS API. Create or update your DataScienceCluster with `modelsAsService` in Managed state:

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
    EOF
    ```

    !!! note "Connectivity Link warning (ODH with Kuadrant)"
        When using ODH with Kuadrant (upstream), you may see `Warning: Red Hat Connectivity Link is not installed, LLMInferenceService cannot be used` in the Kserve status initially. This typically resolves after a few minutes as the operator reconciles. If it persists, apply the `scripts/workaround-odh-rhcl-check.yaml` workaround.

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

    The operator will automatically deploy:

    - **MaaS API** (Deployment, Service, ServiceAccount, ClusterRole, ClusterRoleBinding, HTTPRoute)
    - **MaaS API AuthPolicy** (maas-api-auth-policy) - Protects the MaaS API endpoint
    - **NetworkPolicy** (maas-authorino-allow) - Allows Authorino to reach MaaS API

=== "Kustomize"

    !!! note "Development and early testing"
        Kustomize deployment can be used for **development and early testing purposes**. For production, use the Managed tab above.

    Set `modelsAsService` to **Unmanaged** so the operator does not deploy the MaaS API, then deploy MaaS via the ODH overlay:

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

    Apply the ODH overlay to deploy the MaaS API and controller (run from the project root; ensure the `maas-db-config` Secret exists per [Database Setup](#database-setup)):

    ```bash
    kustomize build deployment/overlays/odh | kubectl apply -f -
    ```

!!! tip "Troubleshooting"
    If components do not become ready, run `kubectl describe datasciencecluster default-dsc` to inspect conditions and events.

## Next steps

* **Deploy models.** See [Model Setup (On Cluster)](model-setup.md) for sample model deployments.
* **Perform validation.** Follow the [validation guide](validation.md) to verify that
  MaaS is working correctly.
