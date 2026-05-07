# MaaS Gateway Setup (ODH/RHOAI)

When the ODH/RHOAI operator is installed from OperatorHub and a DataScienceCluster
is created with `modelsAsService: Managed`, the operator gets stuck with
`ModelsAsServiceReady: False` because several prerequisites are missing (database,
Authorino, gateway — the exact error varies by operator version).

The operator will not fully deploy MaaS until these prerequisites are satisfied.
This script fills that gap.

## Prerequisites

- OpenShift 4.14+ with cluster-admin access
- ODH or RHOAI operator installed with a DataScienceCluster that has `modelsAsService: Managed`
- This repository cloned locally

If you have a completely bare cluster with no ODH installed, run:

```bash
# Install ODH operator (fast-3 channel, v3.4.0-ea.2)
kubectl create namespace opendatahub 2>/dev/null || true
kubectl apply -f - <<'EOF'
apiVersion: operators.coreos.com/v1
kind: OperatorGroup
metadata:
  name: opendatahub
  namespace: opendatahub
spec: {}
---
apiVersion: operators.coreos.com/v1alpha1
kind: Subscription
metadata:
  name: opendatahub-operator
  namespace: opendatahub
spec:
  channel: fast-3
  installPlanApproval: Automatic
  name: opendatahub-operator
  source: community-operators
  sourceNamespace: openshift-marketplace
  startingCSV: opendatahub-operator.v3.4.0-ea.2
EOF

# Wait for operator, then create DSCI + DSC
kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown \
  subscription/opendatahub-operator -n opendatahub --timeout=300s

kubectl apply -f - <<'EOF'
apiVersion: dscinitialization.opendatahub.io/v1
kind: DSCInitialization
metadata:
  name: default-dsci
spec:
  applicationsNamespace: opendatahub
  monitoring:
    managementState: Managed
    namespace: opendatahub-monitoring
    metrics: {}
  trustedCABundle:
    managementState: Managed
EOF

kubectl apply --server-side=true -f - <<'EOF'
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
      managementState: Removed
EOF
```

`ModelsAsServiceReady` will show `False` — that's expected. The next step installs what it needs.

## Install prerequisites

From the repository root:

```bash
./docs/samples/connecting-gateway-odh-rhoai/setup-gateway-prerequisites.sh
```

The script installs Kuadrant, cert-manager, LWS, creates the gateway, deploys a
POC database, applies supplemental RBAC, and configures Authorino TLS. It is
idempotent -- safe to re-run.

## Verify

```bash
DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')
TOKEN=$(oc whoami -t)

# Unauthenticated — expect 401
curl -sk -w '%{http_code}\n' "https://maas.${DOMAIN}/maas-api/v1/models"

# Authenticated — expect 200
curl -sk -w '%{http_code}\n' -H "Authorization: Bearer ${TOKEN}" \
  "https://maas.${DOMAIN}/maas-api/v1/models"
```

401 then 200 confirms gateway routing, Kuadrant auth, and maas-api are working.

## Operator bug workarounds

The script automatically detects and fixes two operator bugs:

1. **`--cluster-audience` flag** — the operator passes this flag to maas-controller, but the
   binary doesn't accept it (it auto-detects the audience). The `cluster-audience` value in
   `params.env` is a kustomize parameter for AuthPolicy, not a controller arg.

2. **maas-api selector labels** — the operator adds `app.opendatahub.io/modelsasservice` to
   `spec.selector.matchLabels`, but the controller uses different labels. Since `spec.selector`
   is immutable, the controller can't reconcile the deployment. The script deletes the
   operator's version and lets the controller recreate it with correct labels.

Both are confirmed operator bugs — `deploy.sh` never hits them because it deploys via kustomize
directly, bypassing the operator's deployment logic.

See [odh-gateway-adding.md](odh-gateway-adding.md#known-issues) for the full list of operator issues.
