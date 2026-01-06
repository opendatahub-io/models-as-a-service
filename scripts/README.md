# Deployment Scripts

This directory contains scripts for deploying and validating the MaaS platform.

## Scripts

### `deploy-openshift.sh`
Complete automated deployment script for OpenShift clusters.

**Usage:**
```bash
./scripts/deploy-openshift.sh
```

**What it does:**
- Checks OpenShift version and applies necessary feature gates
- Creates required namespaces
- Installs dependencies (Kuadrant)
- Deploys Gateway infrastructure
- Deploys KServe components (if not already present)
- Configures MaaS API
- Generates a self-signed backend certificate and configures MaaS API for HTTPS
- Applies policies (AuthPolicy, RateLimitPolicy, TelemetryPolicy)
- Creates OpenShift Routes
- Applies temporary workarounds for known issues

**Requirements:**
- OpenShift cluster (4.16+)
- `oc` CLI installed and logged in
- `kubectl` installed
- `jq` installed
- `kustomize` installed
- `openssl` installed (used for MaaS API backend TLS)

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

### Initial Deployment
```bash
# 1. Deploy the platform
./scripts/deploy-openshift.sh

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
./scripts/deploy-openshift.sh
```

---

## Support

For issues or questions:
1. Run the validation script to identify specific problems
2. Check the main project [README](../README.md)
3. Review [deployment documentation](../docs/content/quickstart.md)
4. Check sample model configurations in [docs/samples/models/](../docs/samples/models/)

