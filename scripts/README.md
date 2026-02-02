# Deployment Scripts

This directory contains scripts for deploying and validating the MaaS platform.

## Scripts

### `deploy.sh` - Quick Deployment Script
Automated deployment script for OpenShift clusters supporting both operator-based and kustomize-based deployments.

**Usage:**
```bash
# Deploy using RHOAI operator (default)
./scripts/deploy.sh

# Deploy using ODH operator
./scripts/deploy.sh --operator-type odh

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
- Configures TLS backend (enabled by default)
- Supports custom operator catalogs and MaaS API images for PR testing

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
- `--grafana`: Install Grafana
- `--prometheus`: Install Prometheus

---

## Common Workflows

### Initial Deployment (Operator Mode - Recommended)
```bash
# 1. Deploy the platform using RHOAI operator
./scripts/deploy.sh

# 2. Validate the deployment
./scripts/validate-deployment.sh

# 3. Deploy a sample model
kustomize build docs/samples/models/simulator | kubectl apply -f -

# 4. Re-run validation to verify model
./scripts/validate-deployment.sh
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
kubectl describe tokenratelimitpolicy gateway-token-rate-limits -n openshift-ingress

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

You can override these by exporting before running:
```bash
export CLUSTER_DOMAIN="apps.my-cluster.example.com"
./scripts/deploy.sh
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

