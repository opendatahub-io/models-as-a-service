# MaaS Controller

Control plane for the Models-as-a-Service (MaaS) subscription model. It reconciles **MaaSModelRef**, **MaaSAuthPolicy**, and **MaaSSubscription** custom resources and creates the corresponding Kuadrant AuthPolicies and TokenRateLimitPolicies, plus HTTPRoutes where needed.

For a comparison of the old tier-based flow vs the new subscription flow, see [docs/old-vs-new-flow.md](docs/old-vs-new-flow.md).

## Architecture

The controller implements a **dual-gate** model where both gates must pass for a request to succeed:

```text
User Request
    │
    ▼
Gateway (maas-default-gateway)
    │
    ├── Default deny AuthPolicy ──── 401/403 for unconfigured models
    ├── Default deny TokenRateLimitPolicy ──────── 429 safety net (defense-in-depth)
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

Models with no MaaSAuthPolicy or MaaSSubscription are denied at the gateway level by `gateway-default-auth` (AuthPolicy, returns 401/403). Per-route policies created by the controller override the gateway defaults.

### CRDs and what they generate

As MaaS API and controller are conventionally deployed in the operator namespace (e.g., `opendatahub`), MaaS CRs need to be separated so that they can be managed with lower cluster privileges. Therefore,
- **MaasModelRef** is located in the same namespace as the **HTTPRoute** and **LLMInderenceService** it refers to; and
- **MaaSAuthPolicy** and **MaaSSubscription** are located in a dedicated subscription namespace (default: `models-as-a-service`). Set `--maas-subscription-namespace` or the `MAAS_SUBSCRIPTION_NAMESPACE` env var in `maas-controller` deployment to use another namespace. MaaS controller will only watch and reconcile those CRs this configured namespace.

| You create | Controller generates | Per | Targets |
| ---------- | -------------------- | --- | ------- |
| **MaaSModelRef** | (validates HTTPRoute) | 1 per model | References LLMInferenceService |
| **MaaSAuthPolicy** | Kuadrant **AuthPolicy** | 1 per model (aggregated from all auth policies) | Model's HTTPRoute |
| **MaaSSubscription** | Kuadrant **TokenRateLimitPolicy** | 1 per model (aggregated from all subscriptions) | Model's HTTPRoute |

Relationships are many-to-many: multiple MaaSAuthPolicies/MaaSSubscriptions can reference the same model — the controller aggregates them into a single Kuadrant policy per model. Multiple subscriptions for one model use mutually exclusive predicates with priority based on token limit (highest wins).

### Namespace scoping

**MaaSModelRef** resources can exist in any namespace. **MaaSAuthPolicy** and **MaaSSubscription** resources explicitly specify which namespace(s) their referenced models are in via `modelRefs[].namespace`.

Generated Kuadrant policies (AuthPolicy, TokenRateLimitPolicy) are always created **in the model's namespace**, not in the namespace of the MaaSAuthPolicy/MaaSSubscription that references it.

**Cross-namespace example:**

```yaml
# MaaSModelRef in the llm namespace
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: my-model
  namespace: llm
spec:
  modelRef:
    kind: LLMInferenceService
    name: my-llmisvc
---
# MaaSAuthPolicy in opendatahub namespace references model in llm namespace
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSAuthPolicy
metadata:
  name: my-policy
  namespace: opendatahub
spec:
  modelRefs:
    - name: my-model
      namespace: llm    # Required: explicitly specify model's namespace
  subjects:
    groups:
      - name: my-group
```

The controller creates a Kuadrant **AuthPolicy** in the `llm` namespace (where the model and HTTPRoute exist), not in `opendatahub` (where the MaaSAuthPolicy lives).

**Same model name, different namespaces:**

Models with identical names in different namespaces are isolated. Each gets its own generated policies:

```yaml
# team-a/my-model and team-b/my-model are separate models
spec:
  modelRefs:
    - name: my-model
      namespace: team-a
    - name: my-model
      namespace: team-b
```

This creates two separate AuthPolicies: one in `team-a`, one in `team-b`.

**Model list API:** When the MaaS controller is installed, the MaaS API **GET /v1/models** endpoint lists models by reading **MaaSModelRef** CRs (in the API's namespace). Each MaaSModelRef's `metadata.name` becomes the model `id`, and `status.endpoint` / `status.phase` supply the URL and readiness. So the set of MaaSModelRef objects is the source of truth for "which models are available" in MaaS. See [docs/content/configuration-and-management/model-listing-flow.md](../docs/content/configuration-and-management/model-listing-flow.md) in the repo for the full flow.

### Model kinds and the provider pattern

MaaSModelRef's `spec.modelRef.kind` selects how the controller discovers and exposes the model. The controller uses a **provider pattern**: each kind has a **BackendHandler** (route reconciliation, status, endpoint resolution, cleanup) and a **RouteResolver** (HTTPRoute name/namespace for attaching AuthPolicy/TokenRateLimitPolicy). These are registered in `pkg/controller/maas/providers.go`.

| Kind (CRD value) | Behaviour |
| ---------------- | --------- |
| **LLMInferenceService** | Validates that an HTTPRoute exists for the referenced LLMInferenceService (created by KServe). Reads endpoint and readiness from the LLMInferenceService/HTTPRoute. |
| **ExternalModel** | Stub: not yet implemented. Controller sets status **Phase=Failed** and condition **Reason=Unsupported**. When implemented, users supply the HTTPRoute (controller does not create it); see `providers_external.go`. |

The CRD enum for `kind` is `LLMInferenceService` and `ExternalModel` (see `api/maas/v1alpha1/maasmodelref_types.go`). The registry accepts **LLMInferenceService** (and the alias **llmisvc** for backwards compatibility). Use `kind: LLMInferenceService` in MaaSModelRef specs.

**Endpoint override:** MaaSModel supports an optional `spec.endpointOverride` field. When set, the controller uses this value for `status.endpoint` instead of the auto-discovered endpoint. This applies to all kinds and is useful when the discovered endpoint is wrong (e.g. wrong gateway or hostname). The controller still validates the backend normally — only the final endpoint URL is overridden.

**Status for unimplemented kinds:** If a kind returns `ErrKindNotImplemented` (e.g. ExternalModel), the controller updates status with Phase=Failed and Ready condition Reason=**Unsupported** (instead of ReconcileFailed), so UIs can distinguish "not implemented" from other failures.

### Adding a new provider

To support a new model kind (e.g. a new backend type):

1. **Extend the API (optional)**
   If the new kind needs extra fields (e.g. `endpoint` for ExternalModel), add them to `ModelReference` in `api/maas/v1alpha1/maasmodelref_types.go` and add the new value to the `Kind` enum. Run `make generate` and `make manifests` in the controller directory.

2. **Implement BackendHandler**  
   Create a new file (e.g. `providers_mykind.go`) and implement the `BackendHandler` interface:
   - **ReconcileRoute**: create/update the HTTPRoute for this model (or validate it exists), and optionally fill `model.Status` with route/gateway info.
   - **Status**: return `(endpointURL, ready, err)`. The controller sets `model.Status.Endpoint` and Phase (Ready/Pending/Failed) from this.
   - **CleanupOnDelete**: if your kind creates an HTTPRoute, delete it here; otherwise no-op.

3. **Implement RouteResolver**
   Implement the `RouteResolver` interface: **HTTPRouteForModel** should return the HTTPRoute name and namespace for the given MaaSModelRef. This is used by `findHTTPRouteForModel` and by the AuthPolicy/Subscription controllers to attach policies to the correct route.

4. **Register the provider**
   In `providers.go` `init()`, register both the handler and the resolver under the same kind string (the CRD enum value, e.g. `MyNewKind`):
   - `backendHandlerFactories["MyNewKind"] = func(r *MaaSModelRefReconciler) BackendHandler { return &myNewKindHandler{r} }`
   - `routeResolverFactories["MyNewKind"] = func() RouteResolver { return &myNewKindRouteResolver{} }`

5. **Tests**  
   Add tests in `providers_test.go`: `GetBackendHandler("MyNewKind", r)` and `GetRouteResolver("MyNewKind")` return non-nil; add tests for `findHTTPRouteForModel` with a fake client if useful. Run `make test`.

The controller will then route MaaSModelRefs with `spec.modelRef.kind: MyNewKind` to your handler. No changes are required in the main reconciler logic.

### Controller watches

The controller watches these resources and re-reconciles automatically:

| Watch | Triggers reconciliation of | Purpose |
| ----- | -------------------------- | ------- |
| MaaSModelRef changes | MaaSAuthPolicy, MaaSSubscription | Re-reconcile when model created/deleted |
| HTTPRoute changes | MaaSModelRef, MaaSAuthPolicy, MaaSSubscription | Re-reconcile when KServe creates a route (fixes startup race) |
| LLMInferenceService changes | MaaSModelRef | Re-reconcile when backend LLMInferenceService spec changes or Ready condition changes (fixes race where backend becomes ready after MaaSModelRef creation) |
| Generated AuthPolicy changes | Parent MaaSAuthPolicy | Overwrite manual edits (unless opted out) |
| Generated TokenRateLimitPolicy changes | Parent MaaSSubscription | Overwrite manual edits (unless opted out) |

### Lifecycle: Deletion behavior

**MaaSModelRef deleted:** The controller uses a finalizer to cascade-delete all generated AuthPolicies and TokenRateLimitPolicies for that model. The parent MaaSAuthPolicy and MaaSSubscription CRs remain intact. The underlying LLMInferenceService is not affected.

**MaaSSubscription deleted:** The aggregated TokenRateLimitPolicy for the model is deleted, then rebuilt from the remaining subscriptions. If no subscriptions remain, the model falls back to the gateway defaults (401/403 from auth if no MaaSAuthPolicy, or 429 from TokenRateLimitPolicy safety net if auth passes).

**MaaSAuthPolicy deleted:** Same pattern — the aggregated AuthPolicy is rebuilt from remaining auth policies.

### Multi-subscription priority

When multiple subscriptions target the same model, the controller sorts them by token limit (highest first) and builds mutually exclusive predicates. A user matching multiple subscription groups hits only the highest-limit rule:

```text
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
./scripts/deploy.sh --operator-type odh
```

This installs all infrastructure (cert-manager, LWS, Kuadrant, ODH, gateway, policies)
plus the subscription controller.

### Option B: Add subscription controller to an existing deployment

If MaaS infrastructure is already deployed, install just the controller:

```bash
kubectl apply -k deployment/base/maas-controller/default
```

To install into another namespace:

```bash
kustomize build deployment/base/maas-controller/default | sed "s/namespace: opendatahub/namespace: my-namespace/g" | kubectl apply -f -
```

### Verify

```bash
kubectl get pods -n opendatahub -l app=maas-controller
kubectl get crd | grep maas.opendatahub.io
```

### What gets installed

| Component | Path | Description |
| --------- | ---- | ----------- |
| CRDs | `deployment/base/maas-controller/crd/` | MaaSModelRef, MaaSAuthPolicy, MaaSSubscription |
| RBAC | `deployment/base/maas-controller/rbac/` | ClusterRole, ServiceAccount, bindings |
| Controller | `deployment/base/maas-controller/manager/` | Deployment (`quay.io/opendatahub/maas-controller:latest`) |
| Default auth policy | `deployment/base/maas-controller/policies/` | Gateway-level AuthPolicy (deny unauthenticated, 401/403) |
| Default deny policy | `deployment/base/maas-controller/policies/` | Gateway-level TokenRateLimitPolicy with 0 tokens (deny unsubscribed, 429) |

## Examples

Install both **regular** and **premium** simulator models and their MaaS policies/subscriptions (from the repository root):

```bash
kubectl create namespace llm --dry-run=client -o yaml | kubectl apply -f -
kubectl create namespace models-as-a-service --dry-run=client -o yaml | kubectl apply -f -
kustomize build docs/samples/maas-system | kubectl apply -f -
```

This creates:

### Regular tier

- `LLMInferenceService/facebook-opt-125m-simulated` in `llm` namespace
- `MaaSModelRef/facebook-opt-125m-simulated` in `opendatahub`
- `MaaSAuthPolicy/simulator-access` (group: `free-user`) and `MaaSSubscription/simulator-subscription` (100 tokens/min) in `models-as-a-service`

### Premium tier

- `LLMInferenceService/premium-simulated-simulated-premium` in `llm` namespace
- `MaaSModelRef/premium-simulated-simulated-premium` in `opendatahub`
- `MaaSAuthPolicy/premium-simulator-access` (group: `premium-user`) and `MaaSSubscription/premium-simulator-subscription` (1000 tokens/min) in `models-as-a-service`

Replace `free-user` and `premium-user` in the example CRs with groups from your identity provider.

Then verify:

```bash
# Check CRs
kubectl get maasmodelref -n opendatahub
kubectl get maasauthpolicy,maassubscription -n models-as-a-service

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
  -H "x-maas-subscription: simulator-subscription" \
  -H "Content-Type: application/json" -d '{"model":"facebook/opt-125m","messages":[{"role":"user","content":"Hi"}],"max_tokens":5}'

# Premium model: 401 without auth, 200 with auth (user must be in premium-user)
curl -sSk -o /dev/null -w "%{http_code}\n" "https://${GATEWAY_HOST}/llm/premium-simulated-simulated-premium/v1/chat/completions" \
  -H "Content-Type: application/json" -d '{"model":"facebook/opt-125m","messages":[{"role":"user","content":"Hi"}],"max_tokens":5}'
curl -sSk -o /dev/null -w "%{http_code}\n" "https://${GATEWAY_HOST}/llm/premium-simulated-simulated-premium/v1/chat/completions" \
  -H "Authorization: Bearer $TOKEN" \
  -H "x-maas-subscription: premium-simulator-subscription" \
  -H "Content-Type: application/json" -d '{"model":"facebook/opt-125m","messages":[{"role":"user","content":"Hi"}],"max_tokens":5}'
```

See [docs/samples/maas-system/README.md](../docs/samples/maas-system/README.md) for more details.

## Opting out of controller management

By default, the controller owns generated AuthPolicies and TokenRateLimitPolicies: it overwrites manual edits on reconciliation and deletes them when the owning MaaS resource is removed. To opt a specific policy out of both behaviours, annotate it:

```bash
# AuthPolicy
kubectl annotate authpolicy <name> -n <namespace> opendatahub.io/managed=false

# TokenRateLimitPolicy
kubectl annotate tokenratelimitpolicy <name> -n <namespace> opendatahub.io/managed=false
```

Remove the annotation to re-enable controller management:

```bash
kubectl annotate authpolicy <name> -n <namespace> opendatahub.io/managed-
kubectl annotate tokenratelimitpolicy <name> -n <namespace> opendatahub.io/managed-
```

> **Warning: orphaned resources.** An opted-out policy can become permanently orphaned (no longer reconciled and not deleted) in the following situations:
>
> - **Last owner deleted.** When the last `MaaSAuthPolicy` or `MaaSSubscription` that references a model is deleted, the controller skips deletion of any opted-out generated policy for that model. The policy will persist until it is manually deleted.
> - **Model reference removed.** When a model is removed from a `MaaSAuthPolicy.spec.modelRefs` or `MaaSSubscription.spec.modelRefs` (edit rather than deletion), the controller does not clean up the generated policy for that model regardless of the opt-out annotation.
> - **MaasModel deleted.** When a MaaSModel is deleted, the controller's finalizer skips deletion of any opted-out generated policy for that model. The policy will persist until it is manually deleted.
>
> In all cases, manually delete the orphaned resource when it is no longer needed.

## Build and push image

The default deployment uses `quay.io/opendatahub/maas-controller:latest`.

```bash
make -C maas-controller image-build                    # build with podman/buildah/docker
make -C maas-controller image-push                     # push to quay.io/opendatahub/maas-controller:latest (this image is created automatically on main branch, so preferably push images with different tag and/or to your temp registry if you are doing some testing and verification)

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
make -C maas-controller install    # apply deployment/base/maas-controller/default to cluster
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

- **Controller namespace**: Default is `opendatahub`. Override via `kustomize build deployment/base/maas-controller/default | sed "s/namespace: opendatahub/namespace: <ns>/g" | kubectl apply -f -`.
- **MaaS subscription namespace**: Default is `models-as-a-service`. Override in the deployment or via Kustomize.
- **Image**: Default is `quay.io/opendatahub/maas-controller:latest`. Override in the deployment or via Kustomize.
- **Gateway name**: The default auth policy targets `maas-default-gateway` in `openshift-ingress`. Edit `deployment/base/maas-controller/policies/gateway-default-auth.yaml` if your gateway has a different name.
