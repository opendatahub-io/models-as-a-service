# MaaS Controller

Control plane for the Models-as-a-Service (MaaS) subscription model. It reconciles **MaaSModel**, **MaaSAuthPolicy**, and **MaaSSubscription** custom resources and creates the corresponding Kuadrant AuthPolicies and TokenRateLimitPolicies, plus HTTPRoutes where needed.

For a comparison of the old tier-based flow vs the new subscription flow, see [docs/old-vs-new-flow.md](docs/old-vs-new-flow.md).

## Architecture

The controller implements a **dual-gate** model where both gates must pass for a request to succeed:

```
User Request
    │
    ▼
Gateway (maas-default-gateway)
    │
    ├── Default deny (0 tokens) for unsubscribed models
    │
    ▼
HTTPRoute (per model)
    │
    ├── Gate 1: AuthPolicy ──── "Is this user allowed to access this model?"
    │   └── Created from MaaSAuthPolicy → checks group membership → 401/403 on failure
    │
    ├── Gate 2: TokenRateLimitPolicy ──── "Does this user have a subscription?"
    │   └── Created from MaaSSubscription → enforces token limits → 429 on failure
    │
    ▼
Model Endpoint (200 OK)
```

### CRDs and what they generate

| You create | Controller generates | Per | Targets |
|------------|---------------------|-----|---------|
| **MaaSModel** | (validates HTTPRoute) | 1 per model | References LLMInferenceService |
| **MaaSAuthPolicy** | Kuadrant **AuthPolicy** | 1 per model (aggregated from all auth policies) | Model's HTTPRoute |
| **MaaSSubscription** | Kuadrant **TokenRateLimitPolicy** | 1 per model (aggregated from all subscriptions) | Model's HTTPRoute |

Relationships are many-to-many: multiple MaaSAuthPolicies/MaaSSubscriptions can reference the same model — the controller aggregates them into a single Kuadrant policy per model. Multiple subscriptions for one model use mutually exclusive predicates with priority based on token limit (highest wins).

**Model list API:** When the MaaS controller is installed, the MaaS API **GET /v1/models** endpoint lists models by reading **MaaSModel** CRs (in the API’s namespace). Each MaaSModel’s `metadata.name` becomes the model `id`, and `status.endpoint` / `status.phase` supply the URL and readiness. So the set of MaaSModel objects is the source of truth for “which models are available” in MaaS. See [docs/content/configuration-and-management/model-listing-flow.md](../docs/content/configuration-and-management/model-listing-flow.md) in the repo for the full flow.

### Controller watches

The controller watches these resources and re-reconciles automatically:

| Watch | Triggers reconciliation of | Purpose |
|-------|---------------------------|---------|
| MaaSModel changes | MaaSAuthPolicy, MaaSSubscription | Re-reconcile when model created/deleted |
| HTTPRoute changes | MaaSModel, MaaSAuthPolicy, MaaSSubscription | Re-reconcile when KServe creates a route (fixes startup race) |
| Generated AuthPolicy changes | Parent MaaSAuthPolicy | Overwrite manual edits (unless opted out) |
| Generated TokenRateLimitPolicy changes | Parent MaaSSubscription | Overwrite manual edits (unless opted out) |

### Lifecycle: Deletion behavior

**MaaSModel deleted:** The controller uses a finalizer to cascade-delete all generated AuthPolicies and TokenRateLimitPolicies for that model. The parent MaaSAuthPolicy and MaaSSubscription CRs remain intact. The underlying LLMInferenceService is not affected.

**MaaSSubscription deleted:** The aggregated TRLP for the model is deleted, then rebuilt from the remaining subscriptions. If no subscriptions remain, the model falls back to the gateway-default-deny (429 for everyone).

**MaaSAuthPolicy deleted:** Same pattern — the aggregated AuthPolicy is rebuilt from remaining auth policies.

### Multi-subscription priority

When multiple subscriptions target the same model, the controller sorts them by token limit (highest first) and builds mutually exclusive predicates. A user matching multiple subscription groups hits only the highest-limit rule:

```
premium-user (50000 tkn/min): matches "in premium-user"
free-user    (100 tkn/min):   matches "in free-user AND NOT in premium-user"
deny-unsubscribed (0):        matches "NOT in premium-user AND NOT in free-user"
```

## Prerequisites

- OpenShift cluster with **Gateway API** and **Kuadrant/RHCL** installed
- **Open Data Hub** operator v3.3+ (for the `opendatahub` namespace and MaaS capability)
  - Note: RHOAI 3.2.0 does NOT support `modelsAsService` -- use ODH instead
- `kubectl` or `oc`
- `kustomize` (for examples)

## Authentication

Until API token minting is in place, the controller uses **OpenShift tokens directly** for inference:

```bash
export TOKEN=$(oc whoami -t)
curl -H "Authorization: Bearer $TOKEN" \
  "https://<gateway-host>/llm/<model-name>/v1/chat/completions" \
  -H "Content-Type: application/json" \
  -d '{"model":"<model>","messages":[{"role":"user","content":"Hello"}],"max_tokens":10}'
```

**Important:** The group names in MaaSAuthPolicy and MaaSSubscription must match groups returned by the Kubernetes **TokenReview API** for your user's token. These come from your identity provider (OIDC, LDAP, htpasswd), **not** from OpenShift Group objects created via `oc adm groups`.

To check your token's groups:

```bash
# Create a temporary token and check what groups TokenReview returns
TOKEN=$(kubectl create token default -n default --duration=1m)
echo '{"apiVersion":"authentication.k8s.io/v1","kind":"TokenReview","spec":{"token":"'$TOKEN'"}}' | \
  kubectl create -o jsonpath='{.status.user.groups}' -f -
```

Common groups: `dedicated-admins`, `system:authenticated`, `system:authenticated:oauth`.

## Install

All commands below are meant to be run from the **repository root** (the directory containing `maas-controller/`).

### Option A: Full deploy with subscription controller (recommended)

Deploy the entire MaaS stack including the subscription controller in one command:

```bash
./scripts/deploy.sh -t odh --enable-subscriptions
```

This installs all infrastructure (cert-manager, LWS, Kuadrant, ODH, gateway, policies)
plus the subscription controller. It also disables the old gateway-auth-policy automatically.

### Option B: Add subscription controller to an existing deployment

If MaaS infrastructure is already deployed, install just the controller:

1. Disable the shared gateway-auth-policy (so the controller can manage auth per HTTPRoute):

   ```bash
   NAMESPACE=opendatahub ./maas-controller/hack/disable-gateway-auth-policy.sh
   ```

   The policy may be in `opendatahub` or `openshift-ingress` depending on deployment.

2. Install the controller (CRDs + RBAC + deployment + default deny policy):

   ```bash
   ./maas-controller/scripts/install-maas-controller.sh
   ```

   To install into another namespace:

   ```bash
   ./maas-controller/scripts/install-maas-controller.sh my-namespace
   ```

### Verify

```bash
kubectl get pods -n opendatahub -l app=maas-controller
kubectl get crd | grep maas.opendatahub.io
```

### What gets installed

| Component | Path | Description |
|-----------|------|-------------|
| CRDs | `config/crd/` | MaaSModel, MaaSAuthPolicy, MaaSSubscription |
| RBAC | `config/rbac/` | ClusterRole, ServiceAccount, bindings |
| Controller | `config/manager/` | Deployment (`quay.io/maas/maas-controller:latest`) |
| Default deny policy | `config/policies/` | Gateway-level TokenRateLimitPolicy with 0 tokens (deny unsubscribed) |

## Examples

Install both **regular** and **premium** simulator models and their MaaS policies/subscriptions (from the repository root):

```bash
maas-controller/scripts/install-examples.sh
```

This creates:

**Regular tier**
- `LLMInferenceService/facebook-opt-125m-simulated` in `llm` namespace
- `MaaSModel/facebook-opt-125m-simulated` in `opendatahub`
- `MaaSAuthPolicy/simulator-access` (group: `free-user`) and `MaaSSubscription/simulator-subscription` (100 tokens/min)

**Premium tier**
- `LLMInferenceService/premium-simulated-simulated-premium` in `llm` namespace
- `MaaSModel/premium-simulated-simulated-premium` in `opendatahub`
- `MaaSAuthPolicy/premium-simulator-access` (group: `premium-user`) and `MaaSSubscription/premium-simulator-subscription` (1000 tokens/min)

Replace `free-user` and `premium-user` in the example CRs with groups from your identity provider.

Then verify:

```bash
# Check CRs
kubectl get maasmodel,maasauthpolicy,maassubscription -n opendatahub

# Check generated Kuadrant policies
kubectl get authpolicy,tokenratelimitpolicy -n llm

# Test inference (set GATEWAY_HOST and TOKEN once)
GATEWAY_HOST="maas.$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
TOKEN=$(oc whoami -t)

# Regular model: 401 without auth, 200 with auth (user must be in free-user)
curl -sSk -o /dev/null -w "%{http_code}\n" "https://${GATEWAY_HOST}/llm/facebook-opt-125m-simulated/v1/chat/completions" \
  -H "Content-Type: application/json" -d '{"model":"facebook/opt-125m","messages":[{"role":"user","content":"Hi"}],"max_tokens":5}'
curl -sSk -o /dev/null -w "%{http_code}\n" "https://${GATEWAY_HOST}/llm/facebook-opt-125m-simulated/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" -d '{"model":"facebook/opt-125m","messages":[{"role":"user","content":"Hi"}],"max_tokens":5}'

# Premium model: 401 without auth, 200 with auth (user must be in premium-user)
curl -sSk -o /dev/null -w "%{http_code}\n" "https://${GATEWAY_HOST}/llm/premium-simulated-simulated-premium/v1/chat/completions" \
  -H "Content-Type: application/json" -d '{"model":"facebook/opt-125m","messages":[{"role":"user","content":"Hi"}],"max_tokens":5}'
curl -sSk -o /dev/null -w "%{http_code}\n" "https://${GATEWAY_HOST}/llm/premium-simulated-simulated-premium/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" -d '{"model":"facebook/opt-125m","messages":[{"role":"user","content":"Hi"}],"max_tokens":5}'
```

See [examples/README.md](examples/README.md) for more details.

## Opting out of controller management

By default, the controller overwrites manual edits to generated AuthPolicies and TokenRateLimitPolicies. To prevent this for a specific policy, annotate it:

```bash
kubectl annotate authpolicy <name> -n <namespace> maas.opendatahub.io/managed=false
```

Remove the annotation to re-enable controller management:

```bash
kubectl annotate authpolicy <name> -n <namespace> maas.opendatahub.io/managed-
```

## Build and push image

The default deployment uses `quay.io/maas/maas-controller:latest` (temporary).

```bash
make -C maas-controller image-build                    # build with podman/buildah/docker
make -C maas-controller image-push                     # push to quay.io/maas/maas-controller:latest

# Custom image/tag
make -C maas-controller image-build IMAGE=quay.io/myorg/maas-controller IMAGE_TAG=v0.1.0
make -C maas-controller image-push IMAGE=quay.io/myorg/maas-controller IMAGE_TAG=v0.1.0
```

## Development

From the repository root:

```bash
make -C maas-controller build      # build binary to maas-controller/bin/manager
make -C maas-controller run        # run locally (uses kubeconfig)
make -C maas-controller test       # run tests
make -C maas-controller install    # apply config/default to cluster
make -C maas-controller uninstall # remove everything
```

## Troubleshooting

**MaaS CRs stuck in `Failed` state:**
The controller retries with exponential backoff. If the HTTPRoute doesn't exist yet (KServe still deploying), the CRs will auto-recover when it appears. If they stay stuck, check controller logs:
```bash
kubectl logs deployment/maas-controller -n opendatahub --tail=20
```

**Auth returns 403 even though user is in the right group:**
The groups in MaaSAuthPolicy must match your identity provider's groups, not OpenShift Group objects. Check your actual token groups (see Authentication section above).

**Unauthenticated requests return 200 instead of 401:**
The gateway-auth-policy may still be active. From the repository root run `NAMESPACE=opendatahub maas-controller/hack/disable-gateway-auth-policy.sh` (check both `opendatahub` and `openshift-ingress` namespaces).

**Kuadrant policies show `Enforced: False`:**
Check that the WasmPlugin exists: `kubectl get wasmplugins -n openshift-ingress`. If missing, ensure RHCL (not community Kuadrant) is installed from the `redhat-operators` catalog.

## Configuration

- **Controller namespace**: Default is `opendatahub`. Override by passing a namespace to `maas-controller/scripts/install-maas-controller.sh`.
- **Image**: Default is `quay.io/maas/maas-controller:latest`. Override in the deployment or via Kustomize.
- **Gateway name**: The default deny policy targets `maas-default-gateway` in `openshift-ingress`. Edit `config/policies/gateway-default-deny.yaml` if your gateway has a different name.
