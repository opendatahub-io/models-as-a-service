# Installation Guide

This guide provides quickstart instructions for deploying the MaaS Platform infrastructure.

!!! note
    For more detailed instructions, please refer to [Installation under the Administrator Guide](install/prerequisites.md).

## Prerequisites

- **OpenShift cluster** (4.19.9+) with kubectl/oc access
      - **Recommended** 16 vCPUs, 32GB RAM, 100GB storage
- **ODH/RHOAI requirements**:
      - RHOAI 3.0 +
      - ODH 3.0 +
- **RHCL requirements** (Note: This can be installed automatically by the script below):
      - RHCL 1.2 +
- **Authorino TLS**: Listener TLS must be enabled on Authorino (see [Configure Authorino TLS](#configure-authorino-tls))
- **Cluster admin** or equivalent permissions
- **Required tools**:
      - `oc` (OpenShift CLI)
      - `kubectl`
      - `jq`
      - `kustomize` (v5.7.0+)
      - `gsed` (GNU sed) - **macOS only**: `brew install gnu-sed`

## Configure Authorino TLS

Before deploying MaaS, Authorino's listener TLS must be enabled. This is a platform prerequisite for secure `LLMInferenceService` communication:

- **Gateway → Authorino (Listener TLS)**: Enable TLS on Authorino's gRPC listener for incoming authentication requests

For step-by-step commands, see [TLS Configuration: Authorino TLS Configuration](configuration-and-management/tls-configuration.md#authorino-tls-configuration).

!!! tip "Automated configuration"
    The `deploy-rhoai-stable.sh` script automatically configures all remaining TLS settings after deployment, including Gateway TLS bootstrap and Authorino → maas-api outbound TLS.

## Quick Start

### Automated OpenShift Deployment (Recommended)

For OpenShift clusters, use the automated deployment script:

```bash
export MAAS_REF="main"  # Use the latest release tag, or "main" for development
./scripts/deploy-rhoai-stable.sh
```

!!! note "Using Release Tags"
    The `MAAS_REF` environment variable should reference a release tag (e.g., `v1.0.0`) for production deployments.
    The release workflow automatically updates all `MAAS_REF="main"` references in documentation and scripts
    to use the new release tag when a release is created. Use `"main"` only for development/testing.

### Verify Deployment

The deployment script creates the following core resources:

- **Gateway**: `maas-default-gateway` in `openshift-ingress` namespace
- **HTTPRoutes**: `maas-api-route` in the `redhat-ods-applications` namespace (deployed by operator)
- **Policies**:
  - `maas-api-auth-policy` (deployed by operator) - Protects MaaS API
  - `gateway-auth-policy` (deployed by script) - Protects Gateway/model inference
  - `TokenRateLimitPolicy`, `RateLimitPolicy` (deployed by script) - Usage limits
- **MaaS API**: Deployment and service in `redhat-ods-applications` namespace (deployed by operator)
- **Operators**: Cert-manager, LWS, Red Hat Connectivity Link and Red Hat OpenShift AI.

Check deployment status:

```bash
# Check all namespaces
kubectl get ns | grep -E "kuadrant-system|kserve|opendatahub|redhat-ods-applications|llm"

# Check Gateway status
kubectl get gateway -n openshift-ingress maas-default-gateway

# Check policies
kubectl get authpolicy -A
kubectl get tokenratelimitpolicy -A
kubectl get ratelimitpolicy -A

# Check MaaS API (deployed by operator in redhat-ods-applications)
kubectl get pods -n redhat-ods-applications -l app.kubernetes.io/name=maas-api
kubectl get svc -n redhat-ods-applications maas-api

# Check Kuadrant operators
kubectl get pods -n kuadrant-system

# Check RHOAI/KServe
kubectl get pods -n kserve
kubectl get pods -n redhat-ods-applications
```

## Model Setup (Optional)

### Deploy Sample Models

#### Simulator Model (CPU)

```bash
PROJECT_DIR=$(git rev-parse --show-toplevel)
kustomize build ${PROJECT_DIR}/docs/samples/models/simulator/ | kubectl apply -f -
```

#### Facebook OPT-125M Model (CPU)

```bash
PROJECT_DIR=$(git rev-parse --show-toplevel)
kustomize build ${PROJECT_DIR}/docs/samples/models/facebook-opt-125m-cpu/ | kubectl apply -f -
```

#### Qwen3 Model (GPU Required)

!!! warning
    This model requires GPU nodes with `nvidia.com/gpu` resources available in your cluster.

```bash
PROJECT_DIR=$(git rev-parse --show-toplevel)
kustomize build ${PROJECT_DIR}/docs/samples/models/qwen3/ | kubectl apply -f -
```

#### Verify Model Deployment

```bash
# Check LLMInferenceService status
kubectl get llminferenceservices -n llm

# Check pods
kubectl get pods -n llm
```

#### Update Existing Models (Optional)

To update an existing model, modify the `LLMInferenceService` to use the newly created `maas-default-gateway` gateway.

```bash
kubectl patch llminferenceservice my-production-model -n llm --type='json' -p='[
  {
    "op": "add",
    "path": "/spec/gateway/refs/-",
    "value": {
      "name": "maas-default-gateway",
      "namespace": "openshift-ingress"
    }
  }
]'
```

```yaml
apiVersion: serving.kserve.io/v1alpha1
kind: LLMInferenceService
metadata:
  name: my-production-model
spec:
  gateway:
    refs:
      - name: maas-default-gateway
        namespace: openshift-ingress
```

## Next Steps

After installation, proceed to [Validation](install/validation.md) to test and verify your deployment.
