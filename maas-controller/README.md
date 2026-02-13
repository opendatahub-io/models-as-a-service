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
| **MaaSAuthPolicy** | Kuadrant **AuthPolicy** | 1 per (policy, model) pair | Model's HTTPRoute |
| **MaaSSubscription** | Kuadrant **TokenRateLimitPolicy** | 1 per (subscription, model) pair | Model's HTTPRoute |

A MaaSAuthPolicy can reference multiple models, and multiple MaaSAuthPolicies can reference the same model (many-to-many). Same for MaaSSubscription.

### Controller watches

The controller watches these resources and re-reconciles automatically:

| Watch | Triggers reconciliation of | Purpose |
|-------|---------------------------|---------|
| MaaSModel changes | MaaSAuthPolicy, MaaSSubscription | Re-reconcile when model created/deleted |
| HTTPRoute changes | MaaSModel, MaaSAuthPolicy, MaaSSubscription | Re-reconcile when KServe creates a route (fixes startup race) |
| Generated AuthPolicy changes | Parent MaaSAuthPolicy | Overwrite manual edits (unless opted out) |
| Generated TokenRateLimitPolicy changes | Parent MaaSSubscription | Overwrite manual edits (unless opted out) |

### Lifecycle: MaaSModel deletion

When a MaaSModel is deleted, the controller uses a finalizer to cascade-delete all generated AuthPolicies and TokenRateLimitPolicies for that model. The parent MaaSAuthPolicy and MaaSSubscription remain intact. The underlying LLMInferenceService is not affected.

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

1. Deploy the base MaaS infrastructure first:

   ```bash
   ./scripts/deploy-rhoai-stable.sh -t odh
   ```

2. Disable the shared gateway-auth-policy (so the controller can manage auth per HTTPRoute):

   ```bash
   # The policy may be in opendatahub or openshift-ingress depending on deployment
   NAMESPACE=opendatahub ./hack/disable-gateway-auth-policy.sh
   ```

3. Install the controller (CRDs + RBAC + deployment + default deny policy):

   ```bash
   ./scripts/install-maas-controller.sh
   ```

   To install into another namespace:

   ```bash
   ./scripts/install-maas-controller.sh my-namespace
   ```

4. Verify:

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

Install the simulator model and example MaaS CRs:

```bash
cd maas-controller
./scripts/install-examples.sh
```

This creates:
- `LLMInferenceService/facebook-opt-125m-simulated` in `llm` namespace
- `MaaSModel/facebook-opt-125m-simulated` in `opendatahub`
- `MaaSAuthPolicy/simulator-access` in `opendatahub` (group: `free-user` -- replace with your group)
- `MaaSSubscription/simulator-subscription` in `opendatahub` (100 tokens/min)

Then verify:

```bash
# Check CRs
kubectl get maasmodel,maasauthpolicy,maassubscription -n opendatahub

# Check generated Kuadrant policies
kubectl get authpolicy,tokenratelimitpolicy -n llm

# Test inference
GATEWAY_HOST="maas.$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')"
TOKEN=$(oc whoami -t)

# Should get 401 (no auth)
curl -sSk -o /dev/null -w "%{http_code}\n" "https://${GATEWAY_HOST}/llm/facebook-opt-125m-simulated/v1/chat/completions" \
  -H "Content-Type: application/json" -d '{"model":"facebook/opt-125m","messages":[{"role":"user","content":"Hi"}],"max_tokens":5}'

# Should get 200 (with auth)
curl -sSk -o /dev/null -w "%{http_code}\n" "https://${GATEWAY_HOST}/llm/facebook-opt-125m-simulated/v1/chat/completions" \
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
make image-build                    # build with podman/buildah/docker
make image-push                     # push to quay.io/maas/maas-controller:latest

# Custom image/tag
make image-build IMAGE=quay.io/myorg/maas-controller IMAGE_TAG=v0.1.0
make image-push IMAGE=quay.io/myorg/maas-controller IMAGE_TAG=v0.1.0
```

## Development

```bash
make build      # build binary to bin/manager
make run        # run locally (uses kubeconfig)
make test       # run tests
make install    # apply config/default to cluster
make uninstall  # remove everything
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
The gateway-auth-policy may still be active. Run `./hack/disable-gateway-auth-policy.sh` (check both `opendatahub` and `openshift-ingress` namespaces).

**Kuadrant policies show `Enforced: False`:**
Check that the WasmPlugin exists: `kubectl get wasmplugins -n openshift-ingress`. If missing, ensure RHCL (not community Kuadrant) is installed from the `redhat-operators` catalog.

## Configuration

- **Controller namespace**: Default is `opendatahub`. Override by passing a namespace to `install-maas-controller.sh`.
- **Image**: Default is `quay.io/maas/maas-controller:latest`. Override in the deployment or via Kustomize.
- **Gateway name**: The default deny policy targets `maas-default-gateway` in `openshift-ingress`. Edit `config/policies/gateway-default-deny.yaml` if your gateway has a different name.
