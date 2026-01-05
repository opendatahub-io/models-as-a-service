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
- **Cluster admin** or equivalent permissions
- **Required tools**:
      - `oc` (OpenShift CLI)
      - `kubectl`
      - `jq`
      - `yq` (v4.50.1+)
      - `kustomize` (v5.7.0+)
      - `gsed` (GNU sed) - **macOS only**: `brew install gnu-sed`

## Quick Start

### Automated OpenShift Deployment (Recommended)

For OpenShift clusters, use the automated deployment script:

```bash
# Basic deployment (interactive, prompts for observability)
./scripts/deploy-openshift.sh

# Deploy with observability stack
./scripts/deploy-openshift.sh --with-observability

# Deploy without observability
./scripts/deploy-openshift.sh --skip-observability

# Use custom namespace
./scripts/deploy-openshift.sh --namespace my-namespace
```

**Alternative: RHOAI Stable Deployment**

For Red Hat OpenShift AI (RHOAI) v3 deployments:

```bash
export MAAS_REF="main"
./scripts/deploy-rhoai-stable.sh
```

### Verify Deployment

The deployment script creates the following core resources:

- **Gateway**: `maas-default-gateway` in `openshift-ingress` namespace
- **HTTPRoutes**: `maas-api-route` in the `openshift-ingress` namespace
- **Policies**: `AuthPolicy`, `TokenRateLimitPolicy`, `RateLimitPolicy`, `TelemetryPolicy`
- **MaaS API**: Deployment and service in `maas-api` namespace
- **Operators**: Cert-manager, LWS, Red Hat Connectivity Link and Red Hat OpenShift AI.
- **Observability** (optional): Grafana instance with dashboards and Prometheus datasource.
- **ServiceMonitors**: Automatically deployed to configure Prometheus metric scraping (deployed even without observability stack).

Check deployment status:

```bash
# Check all namespaces
kubectl get ns | grep -E "maas-api|kuadrant-system|kserve|opendatahub|redhat-ods-applications|llm"

# Check Gateway status
kubectl get gateway -n openshift-ingress maas-default-gateway

# Check policies
kubectl get authpolicy -A
kubectl get tokenratelimitpolicy -A
kubectl get ratelimitpolicy -A

# Check MaaS API
kubectl get pods -n maas-api
kubectl get svc -n maas-api

# Check Kuadrant operators
kubectl get pods -n kuadrant-system

# Check KServe (if deployed)
kubectl get pods -n kserve
kubectl get pods -n opendatahub
kubectl get pods -n redhat-ods-applications
```

## Model Setup (Optional)

### Deploy Sample Models (Optional)

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
