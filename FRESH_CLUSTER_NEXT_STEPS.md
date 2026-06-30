# 🚀 Fresh Cluster - Next Immediate Steps

**Cluster Ready:** ✅ Logged in  
**Cluster URL:** api.zp936-kh2iy-ns9.ykuj.p3.openshiftapps.com  
**User:** cluster-admin

---

## 📋 Step-by-Step Action Plan

### Step 1: Build All Three Images
**Time Required:** 15-30 minutes (depends on network)

Execute in this order from the respective directories:

```bash
# Image 1: maas-controller
cd /Users/somya/Documents/Projects/redhat/models-as-a-service/maas-controller
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --push \
  -t quay.io/your-org/maas-controller:integrated-latest \
  -f Dockerfile .

# Image 2: ai-gateway-operator
cd /Users/somya/Documents/Projects/redhat/ai-gateway-operator
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --push \
  -t quay.io/your-org/ai-gateway-operator:integrated-latest \
  -f Dockerfile .

# Image 3: opendatahub-operator (CRITICAL: use --no-cache)
cd /Users/somya/Documents/Projects/redhat/opendatahub-operator
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --push \
  --no-cache \
  -f Dockerfiles/Dockerfile \
  -t quay.io/your-org/opendatahub-operator:integrated-latest \
  .
```

**Note:** Update `quay.io/your-org` to your actual registry

---

### Step 2: Execute CRD-First Deployment
**Time Required:** 2-3 minutes

Save this as a script and execute:

```bash
#!/bin/bash
set -e

echo "🚀 === FRESH CLUSTER DEPLOYMENT (CRD-FIRST APPROACH) ==="

REPOS="/Users/somya/Documents/Projects/redhat"

echo ""
echo "Step 1: Deploy CRDs FIRST"
echo "=========================="
cd "$REPOS/opendatahub-operator"
kustomize build config/crd | oc apply -f -
echo "✓ opendatahub-operator CRDs applied"
sleep 10

echo ""
echo "Step 2: Deploy opendatahub-operator"
echo "===================================="
kustomize build config/default | oc apply -f -
echo "✓ opendatahub-operator deployed"
sleep 15

echo ""
echo "Step 3: Deploy ai-gateway-operator CRDs"
echo "========================================"
cd "$REPOS/ai-gateway-operator"
kustomize build config/crd | oc apply -f -
echo "✓ ai-gateway-operator CRDs applied"
sleep 5

echo ""
echo "Step 4: Deploy ai-gateway-operator"
echo "==================================="
kustomize build config/default | oc apply -n opendatahub -f -
echo "✓ ai-gateway-operator deployed"
sleep 15

echo ""
echo "Step 5: Create prerequisite CRs"
echo "==============================="
oc apply -f - << 'YAML'
apiVersion: dsci.opendatahub.io/v1
kind: DSCInitialization
metadata:
  name: default-dsci
  namespace: opendatahub
spec:
  applicationsNamespace: opendatahub-applications
---
apiVersion: dsci.opendatahub.io/v1
kind: DataScienceCluster
metadata:
  name: default-dsc
  namespace: opendatahub
spec:
  components:
    modelsAsService:
      managementState: Managed
    batchGateway:
      managementState: Managed
YAML

echo "✓ DataScienceInitialization and DataScienceCluster created"
sleep 10

echo ""
echo "✅ Deployment complete! Waiting for pods to stabilize..."
sleep 30
echo ""
echo "Checking pod status in opendatahub namespace..."
oc get pods -n opendatahub
echo ""
echo "Checking pod status in opendatahub-applications namespace..."
oc get pods -n opendatahub-applications || echo "Applications namespace may still be initializing..."

echo ""
echo "✅ DEPLOYMENT COMPLETE!"
```

---

### Step 3: Verify Deployment
**Time Required:** 2-3 minutes

Run these verification commands:

```bash
# Check opendatahub-operator is running
oc get pods -n opendatahub | grep opendatahub-operator

# Check ai-gateway-operator is running
oc get pods -n opendatahub | grep ai-gateway-operator

# Check AIGateway CRD has modelsAsService field
oc get crd aigateways.components.platform.opendatahub.io \
  -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties}' \
  | jq 'keys' | grep modelsAsService

# Check AIGateway CR was created
oc get aigateways -n opendatahub

# Wait a few moments and check for maas-controller
sleep 30
oc get pods -n opendatahub-applications | grep maas-controller

# Check maas-api
oc get pods -n opendatahub-applications | grep maas-api

# Check batch-gateway (for comparison)
oc get pods -n opendatahub-applications | grep batch-gateway

# Get full pod view
oc get pods -n opendatahub-applications
```

**Expected Results:**
- ✅ opendatahub-operator: Running 1/1
- ✅ ai-gateway-operator: Running 1/1
- ✅ AIGateway CRD: Has "modelsAsService" field
- ✅ AIGateway CR: Created in opendatahub namespace
- ✅ maas-controller: Running 1/1 in opendatahub-applications
- ✅ maas-api: Running 1/1 in opendatahub-applications
- ✅ batch-gateway: Running in opendatahub-applications

---

### Step 4: Run Integration Tests
**Time Required:** 5-10 minutes

```bash
cd /Users/somya/Documents/Projects/redhat/models-as-a-service/test/e2e

# Run E2E smoke tests
python -m pytest scripts/prow_run_smoke_test.sh -v

# Or run specific MaaS integration tests
python -m pytest tests/ -k "maas" -v
```

---

## ⚠️ Troubleshooting

### If pods don't come up:

```bash
# Check pod logs
oc logs -n opendatahub deployment/opendatahub-operator -f
oc logs -n opendatahub deployment/ai-gateway-operator -f
oc logs -n opendatahub-applications deployment/maas-controller -f

# Describe pods for events
oc describe pod -n opendatahub -l app=opendatahub-operator
oc describe pod -n opendatahub -l app=ai-gateway-operator
oc describe pod -n opendatahub-applications -l app=maas-controller

# Check if CRDs are properly established
oc get crd | grep -E "(dsci|aigateway|maas)"

# Check AIGateway CRD details
oc get crd aigateways.components.platform.opendatahub.io -o yaml | grep -A 5 "x-kubernetes-preserve-unknown-fields"
```

### If modelsAsService field is missing from CRD:

**This means old CRD was deployed.** Delete and redeploy:

```bash
oc delete crd aigateways.components.platform.opendatahub.io
sleep 5
cd /Users/somya/Documents/Projects/redhat/opendatahub-operator
kustomize build config/crd | oc apply -f -
```

### If maas-controller won't start:

```bash
# Check if RELATED_IMAGE env vars are set
oc get deployment -n opendatahub ai-gateway-operator \
  -o jsonpath='{.spec.template.spec.containers[0].env}' | jq '.[] | select(.name | contains("RELATED_IMAGE_ODH_MAAS"))'

# Should show:
# RELATED_IMAGE_ODH_MAAS_CONTROLLER_IMAGE
# RELATED_IMAGE_ODH_MAAS_API_IMAGE
```

---

## 📊 Success Criteria

✅ **All criteria must be met:**

1. opendatahub-operator running (1/1) in opendatahub namespace
2. ai-gateway-operator running (1/1) in opendatahub namespace
3. AIGateway CRD has `x-kubernetes-preserve-unknown-fields: true`
4. AIGateway CRD has `modelsAsService` in properties
5. AIGateway CR created in opendatahub namespace
6. maas-controller running (1/1) in opendatahub-applications namespace
7. maas-api running (1/1) in opendatahub-applications namespace
8. batch-gateway running in opendatahub-applications namespace
9. E2E tests pass without errors
10. No errors in operator logs

---

## 🎯 Timeline

| Step | Duration | Total |
|------|----------|-------|
| 1. Build images | 15-30 min | 15-30 min |
| 2. Deploy | 2-3 min | 17-33 min |
| 3. Verify | 2-3 min | 19-36 min |
| 4. Tests | 5-10 min | 24-46 min |

**Total time: ~30-50 minutes to complete full integration test**

---

## 📚 Documentation Reference

- **FINAL_READINESS_SUMMARY.md** - Complete overview
- **BUILD_AND_DEPLOY_IMAGES.md** - Detailed image build guide
- **IMAGE_DEPLOYMENT_STATUS.md** - Image status tracking
- **TEAM_UPDATE_MESSAGE.md** - Team communication

---

## ✅ Checklist

- [ ] All three images built successfully
- [ ] All three images pushed to registry
- [ ] CRD-First deployment executed
- [ ] opendatahub-operator running
- [ ] ai-gateway-operator running
- [ ] maas-controller running
- [ ] maas-api running
- [ ] batch-gateway running
- [ ] AIGateway CRD verification passed
- [ ] E2E tests passed
- [ ] Results documented

---

**Status: 🟢 READY TO EXECUTE!**

Next action: Run the image builds and deployment!

