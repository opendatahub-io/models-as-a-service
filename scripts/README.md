# Deployment Scripts

This directory contains scripts for deploying and validating the MaaS platform.

## Scripts

### `deploy.sh` - Quick Deployment Script
Automated deployment script for OpenShift clusters supporting both operator-based and kustomize-based deployments.

**Usage:**
```bash
# Deploy using ODH operator (default)
./scripts/deploy.sh

# Deploy using RHOAI operator
./scripts/deploy.sh --operator-type rhoai

# Deploy using kustomize
./scripts/deploy.sh --deployment-mode kustomize

# See all options
./scripts/deploy.sh --help
```

**What it does:**
- Validates configuration and prerequisites
- Installs optional operators (cert-manager, LeaderWorkerSet) with auto-detection
- Installs rate limiter (RHCL or upstream Kuadrant)
- Installs primary operator (RHOAI or ODH) or deploys via kustomize
- Applies custom resources (DSC, DSCI)
- Configures TLS backend (enabled by default, use `--disable-tls-backend` to skip)
- In operator mode, patches the live `maas-api` AuthPolicy to restore API key auth and optionally enable OIDC from `deployment/overlays/odh/params.env`
- Supports custom operator catalogs and MaaS API images for PR testing

**Options:**
- `--operator-type <odh|rhoai>` - Which operator to install (default: odh)
- `--deployment-mode <operator|kustomize>` - Deployment method (default: operator)
- `--namespace <namespace>` - Target namespace for deployment
- `--enable-tls-backend` - Enable TLS backend (default)
- `--disable-tls-backend` - Disable TLS backend
- `--verbose` - Enable debug logging
- `--dry-run` - Show what would be done without applying changes
- `--operator-catalog <image>` - Custom operator catalog image for PR testing
- `--operator-image <image>` - Custom operator image for PR testing
- `--channel <channel>` - Operator channel override (default: fast-3 for ODH, fast-3.x for RHOAI)

**Requirements:**
- OpenShift cluster (4.16+)
- `oc` CLI installed and logged in
- `kubectl` installed
- `jq` installed
- `kustomize` installed

**Environment Variables:**
- `MAAS_API_IMAGE` - Custom MaaS API container image (works in both operator and kustomize modes)
- `OPERATOR_CATALOG` - Custom operator catalog for PR testing
- `OPERATOR_IMAGE` - Custom operator image for PR testing
- `OPERATOR_TYPE` - Operator type (odh/rhoai)
- `OIDC_ISSUER_URL` - Override the OIDC issuer URL used when patching the operator-managed `maas-api` AuthPolicy
- `LOG_LEVEL` - Logging verbosity (DEBUG, INFO, WARN, ERROR)

**Advanced Usage:**
```bash
# Test MaaS API PR in operator mode
MAAS_API_IMAGE=quay.io/user/maas-api:pr-123 \
  ./scripts/deploy.sh --operator-type odh

# Deploy with verbose logging
LOG_LEVEL=DEBUG ./scripts/deploy.sh --verbose

# Dry-run to preview deployment plan
./scripts/deploy.sh --dry-run
```

---

### `validate-deployment.sh`
Comprehensive validation script to verify the MaaS deployment is working correctly.

**Usage:**
```bash
./scripts/validate-deployment.sh
```

**What it checks:**

1. **Component Status**
   - ✅ MaaS API pods running
   - ✅ Kuadrant system pods running
   - ✅ OpenDataHub/KServe pods running
   - ✅ LLM models deployed

2. **Gateway Status**
   - ✅ Gateway resource is Accepted and Programmed
   - ✅ Gateway Routes are configured
   - ✅ Gateway service is accessible

3. **Policy Status**
   - ✅ AuthPolicy is configured and enforced
   - ✅ TokenRateLimitPolicy is configured and enforced

4. **API Endpoint Tests**
   - ✅ Authentication endpoint works
   - ✅ Models endpoint is accessible
   - ✅ Model inference endpoint works
   - ✅ Rate limiting is enforced
   - ✅ Authorization is enforced (401 without token)

**Output:**
The script provides:
- ✅ **Pass**: Check succeeded
- ❌ **Fail**: Check failed with reason and suggestion
- ⚠️  **Warning**: Non-critical issue detected

**Exit codes:**
- `0`: All critical checks passed
- `1`: Some checks failed

**Note:** This script validates the general MaaS deployment. For the interim OIDC flow where OIDC is enabled on `maas-api` and model routes still expect API keys, use `validate-oidc-flow.sh` below.

**Example output:**
```
=========================================
🚀 MaaS Platform Deployment Validation
=========================================

=========================================
1️⃣ Component Status Checks
=========================================

🔍 Checking: MaaS API pods
✅ PASS: MaaS API has 1 running pod(s)

🔍 Checking: Kuadrant system pods
✅ PASS: Kuadrant has 8 running pod(s)

...

=========================================
📊 Validation Summary
=========================================

Results:
  ✅ Passed: 10
  ❌ Failed: 0
  ⚠️  Warnings: 2

✅ PASS: All critical checks passed! 🎉
```

---

### `validate-oidc-flow.sh`
Validates the interim OIDC flow:

1. Get an OIDC token
2. Create a MaaS API key with that token
3. List models with the minted API key
4. Run inference with the minted API key

**Usage:**
```bash
# Use the temporary Keycloak defaults
./scripts/validate-oidc-flow.sh

# Or provide your own token directly
OIDC_TOKEN="<jwt>" ./scripts/validate-oidc-flow.sh
```

**Default assumptions:**
- Keycloak route: `https://keycloak.<cluster-domain>`
- Realm: `maas`
- Client: `maas-cli`
- Test user: `alice / letmein`

**Useful overrides:**
- `MAAS_GATEWAY_HOST`
- `KEYCLOAK_HOST`
- `OIDC_TOKEN_URL`
- `OIDC_CLIENT_ID`
- `OIDC_USERNAME`
- `OIDC_PASSWORD`
- `OIDC_TOKEN`

---

### `install-keycloak.sh`
Installs a temporary Keycloak instance for MaaS OIDC testing.

**What it creates:**
- Namespace `keycloak`
- Temporary admin credentials secret
- Realm import ConfigMap
- Keycloak Deployment and Service
- OpenShift Route at `https://keycloak.<cluster-domain>`

**Usage:**
```bash
./scripts/installers/install-keycloak.sh
```

**Default realm contents:**
- Realm: `maas`
- Public client: `maas-cli`
- Users:
  - `alice / letmein` -> `premium-group`
  - `erin / letmein` -> `enterprise-group`
  - `ada / letmein` -> `admin-group`

**Useful overrides:**
- `KEYCLOAK_NAMESPACE`
- `KEYCLOAK_NAME`
- `KEYCLOAK_IMAGE`
- `KEYCLOAK_REALM`
- `KEYCLOAK_CLIENT_ID`
- `KEYCLOAK_HOST`
- `KEYCLOAK_ADMIN_USER`
- `KEYCLOAK_ADMIN_PASSWORD`
- `CLUSTER_DOMAIN`

The script prints the issuer URL to copy into `deployment/overlays/odh/params.env`.

---

### `setup-authorino-tls.sh`
Configures Authorino for TLS communication with maas-api. Run automatically by `deploy.sh` when `--enable-tls-backend` is set (default).

**Usage:**
```bash
# Configure Authorino TLS (default: kuadrant-system)
./scripts/setup-authorino-tls.sh

# For RHCL, use rh-connectivity-link namespace
AUTHORINO_NAMESPACE=rh-connectivity-link ./scripts/setup-authorino-tls.sh
```

**Note:** This script patches Authorino's service, CR, and deployment. Use `--disable-tls-backend` with `deploy.sh` to skip if you manage Authorino TLS separately.

---

### `install-dependencies.sh`
Installs individual dependencies (Kuadrant, ODH, etc.).

**Usage:**
```bash
# Install all dependencies
./scripts/install-dependencies.sh

# Install specific dependency
./scripts/install-dependencies.sh --kuadrant
```

**Options:**
- `--kuadrant`: Install Kuadrant operator and dependencies
- `--istio`: Install Istio
- `--prometheus`: Install Prometheus

---

## Common Workflows

### Initial Deployment (Operator Mode - Recommended)
```bash
# 1. Deploy the platform using ODH operator (default)
./scripts/deploy.sh

# 2. Validate the deployment
./scripts/validate-deployment.sh

# 3. Deploy a sample model
kustomize build docs/samples/models/simulator | kubectl apply -f -

# 4. Re-run validation to verify model
./scripts/validate-deployment.sh
```

### OIDC Smoke Test
```bash
# 1. Install the temporary Keycloak example
./scripts/installers/install-keycloak.sh

# 2. Replace the placeholder oidc-issuer-url in deployment/overlays/odh/params.env
#    with the printed issuer URL

# 3. Deploy or patch MaaS with OIDC enabled
#    (works in both operator mode and kustomize mode via deploy.sh)
./scripts/deploy.sh

# 4. Validate the interim OIDC -> API key -> inference flow
./scripts/validate-oidc-flow.sh
```

### Initial Deployment (Kustomize Mode)
```bash
# 1. Deploy the platform using kustomize
./scripts/deploy.sh --deployment-mode kustomize

# 2. Validate the deployment
./scripts/validate-deployment.sh

# 3. Deploy a sample model
kustomize build docs/samples/models/simulator | kubectl apply -f -

# 4. Re-run validation to verify model
./scripts/validate-deployment.sh
```

### Troubleshooting Failed Validation

If validation fails, the script provides specific suggestions:

**Failed: MaaS API pods**
```bash
# Check pod status
kubectl get pods -n maas-api

# Check pod logs
kubectl logs -n maas-api -l app=maas-api
```

**Failed: Gateway not ready**
```bash
# Check gateway status
kubectl describe gateway maas-default-gateway -n openshift-ingress

# Check for Service Mesh installation
kubectl get pods -n istio-system
```

**Failed: Authentication endpoint**
```bash
# Check AuthPolicy status
kubectl get authpolicy -A
kubectl describe authpolicy gateway-auth-policy -n openshift-ingress

# Check if you're logged into OpenShift
oc whoami
oc login
```

**Failed: Rate limiting not working**
```bash
# Check TokenRateLimitPolicy
kubectl get tokenratelimitpolicy -A
kubectl describe tokenratelimitpolicy -n openshift-ingress

# Check Limitador pods
kubectl get pods -n kuadrant-system -l app.kubernetes.io/name=limitador
```

### Debugging with Validation Script

The validation script is designed to be run repeatedly during troubleshooting:

```bash
# Make changes to fix issues
kubectl apply -f ...

# Re-run validation
./scripts/validate-deployment.sh

# Check specific component logs
kubectl logs -n maas-api deployment/maas-api
kubectl logs -n kuadrant-system -l app.kubernetes.io/name=kuadrant-operator
```

---

## Requirements

All scripts require:
- `kubectl` or `oc` CLI
- `jq` for JSON parsing
- `kustomize` for manifest generation
- Access to an OpenShift or Kubernetes cluster
- Appropriate RBAC permissions (cluster-admin recommended)

## Environment Variables

Scripts will automatically detect:
- `CLUSTER_DOMAIN`: OpenShift cluster domain from `ingresses.config.openshift.io/cluster`
- OpenShift authentication token via `oc whoami -t`
- Gateway hostname from the Gateway resource (no cluster-admin needed for `validate-deployment.sh`)

You can override these by exporting before running:
```bash
export CLUSTER_DOMAIN="apps.my-cluster.example.com"
./scripts/deploy.sh
```

**Non-admin users:** If you cannot read `ingresses.config.openshift.io/cluster`, the validation script will try the Gateway's listener hostname. If that is not available, set the gateway URL explicitly:
```bash
export MAAS_GATEWAY_HOST="https://maas.apps.your-cluster.example.com"
./scripts/validate-deployment.sh
```

---

## Testing

### End-to-End Testing

For comprehensive end-to-end testing including deployment, user setup, and smoke tests:

```bash
./test/e2e/scripts/prow_run_smoke_test.sh
```

This is the same script used in CI/CD pipelines. It supports testing custom images:

```bash
# Test PR-built images
OPERATOR_CATALOG=quay.io/opendatahub/opendatahub-operator-catalog:pr-123 \
MAAS_API_IMAGE=quay.io/opendatahub/maas-api:pr-456 \
./test/e2e/scripts/prow_run_smoke_test.sh
```

See [test/e2e/README.md](../test/e2e/README.md) for complete testing documentation and CI/CD pipeline usage examples.

---

## Support

For issues or questions:
1. Run the validation script to identify specific problems
2. Check the main project [README](../README.md)
3. Review [deployment documentation](../docs/content/quickstart.md)
4. Check sample model configurations in [docs/samples/models/](../docs/samples/models/)

