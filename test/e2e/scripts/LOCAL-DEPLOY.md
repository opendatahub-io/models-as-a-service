# MaaS Local Deployment Guide

One-click local deployment of the full MaaS + BBR platform on Kind (macOS + Linux/WSL2).

## Quick Start

```bash
# Deploy everything (~8-10 minutes on first run, ~5 minutes with cached images)
./test/e2e/scripts/local-deploy.sh

# Rebuild a single component after code changes (~30 seconds)
./test/e2e/scripts/local-deploy.sh --rebuild bbr
./test/e2e/scripts/local-deploy.sh --rebuild maas-api
./test/e2e/scripts/local-deploy.sh --rebuild maas-controller
./test/e2e/scripts/local-deploy.sh --rebuild all

# Validate everything works (18 checks: pods, inference, auth, TLS)
./test/e2e/scripts/local-deploy.sh --validate

# Run E2E management tests (API key CRUD, expiration, revocation, etc.)
./test/e2e/scripts/local-test.sh

# Interactive demo (walks through the stack step by step)
./test/e2e/scripts/local-demo.sh

# Check status
./test/e2e/scripts/local-deploy.sh --status

# Teardown
./test/e2e/scripts/local-deploy.sh --teardown
```

## Prerequisites

- **macOS** (Apple Silicon or Intel)
- **Docker Desktop** (6+ CPUs, 8+ GB RAM recommended)
- **Tools**: `kind`, `kubectl`, `kustomize`, `helm`, `jq` (install via `brew install`)
- `istioctl` is installed automatically if missing

No other repos need to be cloned. The script auto-clones what it needs.

> **How BBR works**: The BBR (payload-processing) **deployment manifests** (Deployment, Service,
> EnvoyFilter) live inside this repo at `deployment/base/payload-processing/`. This is the same
> as OpenShift — the MaaS kustomize overlay includes BBR. On Apple Silicon, the pre-built quay.io
> image is x86-only, so the script **auto-clones** `ai-gateway-payload-processing` to
> `../ai-gateway-payload-processing` and builds an arm64 image locally. On x86, the pre-built
> image is used directly.

## What Gets Deployed

30 pods across 8 namespaces. Full stack matching the OpenShift deployment:

```
Kind cluster (maas-local)
+-- istio-system (3 pods)
|   +-- istiod                          # Istio control plane
|   +-- maas-default-gateway-istio      # Gateway (MetalLB assigns LB IP)
|   +-- payload-processing              # BBR ext-proc (body-based routing)
|
+-- kuadrant-system (6 pods)
|   +-- kuadrant-operator               # Manages auth/rate-limit policies
|   +-- authorino                       # Auth service (API key + K8s token validation)
|   +-- limitador                       # Rate limiting
|   +-- authorino-operator, limitador-operator, dns-operator
|
+-- maas-system (3 pods)
|   +-- maas-api                        # REST API (HTTPS 8443, cert-manager TLS)
|   +-- maas-controller                 # Reconciles MaaS CRDs -> Kuadrant policies
|   +-- postgres                        # PostgreSQL 16 (ephemeral, API key storage)
|
+-- kserve (3 pods)
|   +-- kserve-controller-manager       # KServe (opendatahub fork)
|   +-- llmisvc-controller-manager      # LLMInferenceService controller
|   +-- kserve-localmodel-controller    # Local model cache controller
|
+-- cert-manager (3 pods)               # TLS certificate management
+-- metallb-system (2 pods)             # LoadBalancer for Kind
+-- kube-system (7 pods)                # Kubernetes core
|
+-- llm (0 pods, networking only)       # External model namespace
|   +-- ExternalModel: llm-katan-openai # -> 3-13-21-181.sslip.io (AWS, Let's Encrypt TLS)
|   +-- ServiceEntry + DestinationRule  # Created by controller
|   +-- HTTPRoute + AuthPolicy          # Created by controller
|
+-- llm-internal (1 pod)                # Internal model namespace
|   +-- LLMInferenceService: sim-internal  # llm-d inference simulator
|   +-- HTTPRoute + AuthPolicy          # Created by controller
|
+-- models-as-a-service (0 pods)        # Subscription namespace
    +-- MaaSSubscription                # Token rate limits
    +-- MaaSAuthPolicy                  # Group-based access control
```

## How It Matches OpenShift

| Aspect | OpenShift | Kind (this script) |
|--------|-----------|-------------------|
| Gateway namespace | openshift-ingress | istio-system |
| GatewayClass | openshift-default | istio |
| Kuadrant | OLM operator | Helm chart |
| KServe | ODH operator + DataScienceCluster | Vanilla KServe + opendatahub fork images |
| MaaS deploy mode | `deploy.sh --deployment-mode kustomize` (odh overlay) | Custom kustomize overlay (TLS backend) |
| maas-api TLS | OpenShift service-ca auto-cert | cert-manager CA chain |
| Authorino CA trust | service-ca bundle in system trust store | Init container injects CA into SSL_CERT_FILE |
| PostgreSQL | registry.redhat.io/rhel9/postgresql-16 | postgres:16-alpine |
| LoadBalancer | Cloud provider / OpenShift router | MetalLB |
| BBR EnvoyFilter | INSERT_AFTER openshift-ingress.kuadrant-... | INSERT_AFTER istio-system.kuadrant-... |
| External model TLS | Real provider certs (trusted) | Let's Encrypt via sslip.io (trusted) |

## Request Flow

Identical to OpenShift — same filters, same order, same auth chain.
The only difference is infrastructure (Kind vs OpenShift, MetalLB vs cloud LB).

```
Client
  |
  v
Gateway (port 80)                                    # identical to OpenShift
  |
  +-- Kuadrant Wasm filter (auth check)              # identical to OpenShift
  |     |
  |     +-- API key? -> Authorino -> maas-api:8443   # identical to OpenShift
  |     +-- K8s token? -> Authorino -> TokenReview   # identical to OpenShift
  |
  +-- BBR ext-proc (payload-processing)              # identical to OpenShift
  |     |
  |     +-- Extract model name from request body
  |     +-- Resolve ExternalModel -> set X-Gateway-Model-Name header
  |     +-- Inject provider credentials
  |     +-- Translate API format (anthropic/bedrock/vertex -> openai)
  |
  +-- Route to backend (via HTTPRoute header match)  # identical to OpenShift
        |
        +-- External model: ServiceEntry -> llm-katan (AWS, HTTPS 443)
        +-- Internal model: KServe pod (in-cluster, HTTP 8000)
```

## Validating the Deployment

### Check all pods are running

```bash
kubectl get pods -A | grep -v Completed
```

### Check MaaS resources

```bash
# Models
kubectl get externalmodels -A
kubectl get llminferenceservices -A
kubectl get maasmodelrefs -A

# Auth & rate limiting
kubectl get authpolicies -A
kubectl get maassubscriptions -A
kubectl get maasauthpolicies -A

# Networking
kubectl get httproutes -A
kubectl get serviceentries -A
kubectl get destinationrules -A
kubectl get gateway -n istio-system
```

### Test inference

```bash
# Port-forward the gateway
kubectl port-forward -n istio-system svc/maas-default-gateway-istio 8080:80 &

# Create an API key (via kubectl exec, bypassing gateway auth)
API_KEY=$(kubectl exec -n maas-system deployment/maas-api -- curl -sk \
  "https://localhost:8443/v1/api-keys" \
  -H "X-MaaS-Username: test-user" \
  -H 'X-MaaS-Group: ["system:authenticated"]' \
  -H "Content-Type: application/json" \
  -d '{"name":"my-test-key"}' | jq -r '.key')

# List models
curl -s http://localhost:8080/v1/models -H "Authorization: Bearer $API_KEY" | jq .

# External model (llm-katan on AWS)
curl -s http://localhost:8080/llm/llm-katan-openai/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"llm-katan-echo","messages":[{"role":"user","content":"Hello!"}],"max_tokens":20}' | jq .

# Internal model (llm-d simulator)
curl -s http://localhost:8080/llm-internal/sim-internal/v1/chat/completions \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"facebook/opt-125m","messages":[{"role":"user","content":"Hello!"}],"max_tokens":20}' | jq .

# Verify auth rejection
curl -s -o /dev/null -w '%{http_code}' http://localhost:8080/v1/models  # 401
```

### Run E2E management tests

```bash
./test/e2e/scripts/local-test.sh
```

## Troubleshooting

### Logs

```bash
# MaaS API
kubectl logs -n maas-system deployment/maas-api -f

# MaaS controller
kubectl logs -n maas-system deployment/maas-controller -f

# Authorino (auth decisions)
kubectl logs -n kuadrant-system deployment/authorino -f

# BBR (payload processing)
kubectl logs -n istio-system deployment/payload-processing -f

# Gateway proxy (Envoy access logs)
kubectl logs -n istio-system deployment/maas-default-gateway-istio -f
```

### Common issues

**Auth returns 403 (API key valid but rejected)**:
Check Authorino logs — usually means Authorino can't reach maas-api:8443.
Verify the CA trust: `kubectl get configmap maas-ca-bundle -n kuadrant-system`.

**External model returns 503 (TLS error)**:
Check the DestinationRule: `kubectl get dr -n llm -o yaml`.
The llm-katan endpoint uses Let's Encrypt via sslip.io — the cert should be trusted.

**Controller keeps restarting**:
The Tenant PR added a watch on `Authentication.config.openshift.io` which doesn't exist
on Kind. The controller logs `no matches for kind "Authentication"` errors but still works
(it retries every 10s). This is expected on non-OpenShift clusters.

**Internal model stays Pending**:
The llm-d simulator pod may take a minute to start. Check:
`kubectl get pods -n llm-internal`.

### Inspecting CRDs (equivalent of OpenShift console)

```bash
# Use kubectl get/describe instead of the OpenShift dashboard

# All MaaS custom resources
kubectl get externalmodels,maasmodelrefs,maassubscriptions,maasauthpolicies -A

# Detailed view of a resource
kubectl describe externalmodel llm-katan-openai -n llm
kubectl describe maassubscription simulator-subscription -n models-as-a-service

# API keys (stored in PostgreSQL, query via maas-api)
kubectl exec -n maas-system deployment/maas-api -- curl -sk \
  "https://localhost:8443/v1/api-keys/search" -X POST \
  -H "X-MaaS-Username: test-user" \
  -H 'X-MaaS-Group: ["system:authenticated"]' \
  -H "Content-Type: application/json" -d '{}' | jq .

# Kuadrant policies (generated by MaaS controller)
kubectl get authpolicies -A -o wide
kubectl describe authpolicy maas-auth-llm-katan-openai -n llm

# Istio networking (generated by ExternalModel controller)
kubectl get serviceentries,destinationrules -A
kubectl describe serviceentry llm-katan-openai -n llm
```

### Docker / Kind commands

```bash
# List Kind clusters
kind get clusters

# Access Kind node
docker exec -it maas-local-control-plane bash

# Check images loaded in Kind
docker exec maas-local-control-plane crictl images | grep maas

# Kind cluster resource usage
docker stats maas-local-control-plane --no-stream
```

## Development Workflow

### Iterating on code changes

After the initial deploy, use `--rebuild` to quickly test code changes without
redeploying the full infrastructure:

```bash
# Example: you changed BBR plugin code
cd ../ai-gateway-payload-processing
# ... edit code ...
cd ../models-as-a-service
./test/e2e/scripts/local-deploy.sh --rebuild bbr    # ~30 seconds

# Example: you changed maas-api handler
# ... edit maas-api code ...
./test/e2e/scripts/local-deploy.sh --rebuild maas-api

# Example: you changed maas-controller reconciler
# ... edit maas-controller code ...
./test/e2e/scripts/local-deploy.sh --rebuild maas-controller

# Rebuild everything after pulling latest from all repos
./test/e2e/scripts/local-deploy.sh --rebuild all    # ~90 seconds
```

`--rebuild` only rebuilds the Docker image, loads it into Kind, and restarts the
deployment. The cluster, Istio, Kuadrant, and all CRDs stay untouched.

### When to use --rebuild vs full redeploy

| Scenario | Command |
|----------|---------|
| Changed Go/Python code in a component | `--rebuild <component>` |
| Changed CRD definitions or kustomize manifests | Full redeploy (teardown + deploy) |
| Changed Kuadrant/Istio/KServe configuration | Full redeploy |
| Cluster is in a weird state | `--teardown` then deploy |

## Resource Consumption

| Component | CPU request | Memory |
|-----------|------------|--------|
| Istio (istiod + gateway) | ~500m | ~1Gi |
| Kuadrant stack (6 pods) | ~450m | ~1Gi |
| cert-manager (3 pods) | ~100m | ~256Mi |
| MaaS (api + controller) | ~60m | ~640Mi |
| PostgreSQL | ~100m | ~512Mi |
| KServe (3 pods) | ~100m | ~384Mi |
| BBR (payload-processing) | ~100m | ~128Mi |
| llm-d simulator | ~100m | ~256Mi |
| **Total** | **~1.5 CPU** | **~4Gi** |

Recommended Docker Desktop settings: **6 CPUs, 8GB RAM, 10GB free disk**.

## Deployment Timing (Apple Silicon, cached images)

| Phase | Time |
|-------|------|
| Kind cluster creation | ~20s |
| Istio + MetalLB | ~30s |
| cert-manager + TLS certs | ~30s |
| Kuadrant (Helm + Authorino CA) | ~60s |
| PostgreSQL | ~15s |
| CRDs (MaaS + KServe) | ~15s |
| KServe + LLMIS controller | ~90s |
| Gateway + networking | ~15s |
| MaaS kustomize build + deploy | ~30s (+ ~120s for arm64 image builds on first run) |
| Test fixtures + reconciliation | ~30s |
| **Total (cached)** | **~5-6 minutes** |
| **Total (first run, arm64 builds)** | **~8-10 minutes** |
