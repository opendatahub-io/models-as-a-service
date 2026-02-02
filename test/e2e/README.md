# MaaS E2E Testing

## Quick Start

### Prerequisites

- **OpenShift Cluster**: Must be logged in as cluster admin
- **Required Tools**: `oc`, `kubectl`, `kustomize`, `jq`
- **Python**: with pip

### Complete End-to-End Testing
Deploys MaaS platform, creates test users, and runs smoke tests:

```bash
./test/e2e/scripts/prow_run_smoke_test.sh
```

### Smoke Tests Only

If MaaS is already deployed and you just want to run tests:
```bash
./test/e2e/smoke.sh
```
