# Deployment Scripts

This directory contains scripts for deploying and validating the MaaS platform.

## Scripts

### `deploy-openshift.sh`
Complete automated deployment script for OpenShift clusters.

**Usage:**
```bash
# Interactive mode (prompts for observability)
./scripts/deploy-openshift.sh

# Install with observability stack
./scripts/deploy-openshift.sh --with-observability

# Install without observability
./scripts/deploy-openshift.sh --skip-observability

# Use custom namespace
./scripts/deploy-openshift.sh --namespace my-namespace

# Show help
./scripts/deploy-openshift.sh --help
```

**Options:**
- `--with-observability`: Install observability stack (Grafana + dashboards) without prompting
- `--skip-observability`: Skip observability installation (no prompt)
- `--namespace NAMESPACE`: MaaS API namespace (default: `maas-api`)
- `-h, --help`: Show help message

**What it does:**
- Checks OpenShift version and Gateway API requirements
- Creates required namespaces
- Installs dependencies (Kuadrant, cert-manager)
- Deploys Gateway infrastructure
- Deploys KServe components (if not already present)
- Configures MaaS API
- Applies policies (AuthPolicy, RateLimitPolicy, TokenRateLimitPolicy)
- Deploys base observability (TelemetryPolicy, ServiceMonitors for metrics collection)
- Creates OpenShift Routes
- Optionally installs full observability stack (Grafana + dashboards)
- Runs deployment validation

**Requirements:**
- OpenShift cluster (4.20+)
- `oc` CLI installed and logged in
- `kubectl` installed
- `jq` installed
- `kustomize` installed
- `yq` installed (for YAML processing)

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
- `--istio`: Install Istio (for vanilla Kubernetes)
- `--grafana`: Install Grafana operator
- `--prometheus`: Install Prometheus (for vanilla Kubernetes; validates on OpenShift)
- `--odh`: Install OpenDataHub operator (OpenShift only)
- `--kserve`: Install KServe (validates on OpenShift)
- `--ocp`: Use OpenShift-specific handling (validates instead of installs)
- `--all`: Install all components

**Note:** On OpenShift, some components (Istio, Prometheus) are provided by the platform. The script validates their presence instead of installing them.

---

### `install-observability.sh`
Installs the observability stack (Grafana instance, dashboards, and Prometheus datasource).

**Usage:**
```bash
# Install to default namespace (maas-api)
./scripts/install-observability.sh

# Install to custom namespace
./scripts/install-observability.sh --namespace my-namespace
```

**What it does:**
- Enables user-workload-monitoring
- Labels namespaces for monitoring (`kuadrant-system`, `maas-api`, `llm`)
- Deploys TelemetryPolicy (for user/tier/model labels in metrics)
- Deploys ServiceMonitors (Limitador, Authorino, Istio Gateway, LLM models)
- Installs Grafana operator (if not present)
- Deploys Grafana instance
- Configures Prometheus datasource with authentication
- Deploys dashboards (Platform Admin, AI Engineer)

**Note:** This script can run standalone or as part of `deploy-openshift.sh`. When run standalone, it deploys all necessary observability components including TelemetryPolicy and ServiceMonitors.

**Requirements:**
- Grafana operator installed (installed automatically if missing)
- User-workload-monitoring enabled
- OpenShift cluster with monitoring stack

**Note:** The Grafana datasource is created dynamically with proper authentication token injection. The static `grafana-datasource.yaml` file is not used.

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


