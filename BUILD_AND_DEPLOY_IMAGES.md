# 🐳 Build Latest Images from Feature Branches

**Status:** All code fixes are committed and ready ✅

## 📦 Images to Build

### 1️⃣ maas-controller Image
**Repository:** `models-as-a-service`  
**Branch:** `integrated-maas-modularization`  
**Latest Commit:** `f3c7682d` - fix: remove unnecessary create/patch/delete verbs from TenantReconciler RBAC

**Build Command:**
```bash
cd /Users/somya/Documents/Projects/redhat/models-as-a-service/maas-controller
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --push \
  -t quay.io/your-org/maas-controller:integrated-latest \
  -f Dockerfile .
```

**What's Fixed:**
- ✅ Correct RBAC permissions (minimal + Kuadrant)
- ✅ APPLICATIONS_NAMESPACE support

---

### 2️⃣ ai-gateway-operator Image
**Repository:** `ai-gateway-operator`  
**Branch:** `integrated-maas-modularization`  
**Latest Commit:** `20ec4bd` - chore: update ai-gateway-operator image to integrated in both params and kustomization

**Build Command:**
```bash
cd /Users/somya/Documents/Projects/redhat/ai-gateway-operator
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --push \
  -t quay.io/your-org/ai-gateway-operator:integrated-latest \
  -f Dockerfile .
```

**What's Fixed:**
- ✅ CRD has `x-kubernetes-preserve-unknown-fields: true` (in source)
- ✅ Supports modelsAsService field in AIGateway CR
- ✅ Least privilege RBAC

---

### 3️⃣ opendatahub-operator Image
**Repository:** `opendatahub-operator`  
**Branch:** `integrated-maas-modularization`  
**Latest Commits:**
- `2447d9e1a` - fix: add MaaS RELATED_IMAGE environment variables ⭐ CRITICAL
- `8ad10b825` - fix: ensure AIGateway CRD has modelsAsService field ⭐ CRITICAL

**Build Command:**
```bash
cd /Users/somya/Documents/Projects/redhat/opendatahub-operator
docker buildx build \
  --platform linux/amd64,linux/arm64 \
  --push \
  --no-cache \
  -f Dockerfiles/Dockerfile \
  -t quay.io/your-org/opendatahub-operator:integrated-latest \
  .
```

**Why `--no-cache`?**
- Ensures fresh manifest fetch from ai-gateway-operator repo
- Gets the latest CRD with all fixes
- Avoids stale embedded manifests

**What's Fixed:**
- ✅ RELATED_IMAGE_ODH_MAAS_CONTROLLER_IMAGE ⭐ CRITICAL
- ✅ RELATED_IMAGE_ODH_MAAS_API_IMAGE ⭐ CRITICAL
- ✅ AIGateway CRD has modelsAsService field (fetched + stored)

---

## 🔐 Verify Images Before Deployment

```bash
# Check maas-controller image exists
docker inspect quay.io/your-org/maas-controller:integrated-latest

# Check ai-gateway-operator image exists
docker inspect quay.io/your-org/ai-gateway-operator:integrated-latest

# Check opendatahub-operator image exists
docker inspect quay.io/your-org/opendatahub-operator:integrated-latest
```

---

## 📋 Deployment Order (CRD-First Strategy)

Once images are built and pushed, deploy in this order:

```bash
# 1. Apply opendatahub-operator CRDs FIRST (includes AIGateway CRD with fixes)
kustomize build config/crd | oc apply -f -

# 2. Wait for CRDs to be established
sleep 10

# 3. Deploy opendatahub-operator deployment
kustomize build config/default | oc apply -f -

# 4. Wait for opendatahub-operator to be ready
sleep 15

# 5. Deploy ai-gateway-operator CRDs
cd ../ai-gateway-operator
kustomize build config/crd | oc apply -f -

# 6. Deploy ai-gateway-operator
kustomize build config/default | oc apply -n opendatahub -f -

# 7. Create prerequisite CRs
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
```

---

## ✅ Verification Checklist

After deployment:

```bash
# Check opendatahub-operator is running
oc get pods -n opendatahub | grep opendatahub-operator

# Check ai-gateway-operator is running  
oc get pods -n opendatahub | grep ai-gateway-operator

# Check AIGateway CRD has modelsAsService
oc get crd aigateways.components.platform.opendatahub.io -o jsonpath='{.spec.versions[0].schema.openAPIV3Schema.properties.spec.properties}' | jq 'keys' | grep modelsAsService

# Check AIGateway CR was created by opendatahub-operator
oc get aigateways -n opendatahub

# Check maas-controller is running
oc get pods -n opendatahub-applications | grep maas-controller

# Check maas-api is running
oc get pods -n opendatahub-applications | grep maas-api

# Check batch-gateway is also running
oc get pods -n opendatahub-applications | grep batch-gateway
```

---

## 🎯 Key Points

| Item | Status | Location |
|------|--------|----------|
| **RELATED_IMAGES Fix** | ✅ Committed | `opendatahub-operator` commit `2447d9e1a` |
| **CRD Preserve Field** | ✅ Committed | `opendatahub-operator` commit `8ad10b825` |
| **maas-controller Fixes** | ✅ Committed | `models-as-a-service` - latest commits |
| **ai-gateway-operator Ready** | ✅ Committed | `ai-gateway-operator` - feature branch |
| **Images Built** | ⏳ Pending | Build from above commands |
| **Deployment CRD-First** | ✅ Ready | Use sequence above |

---

**Everything is code-ready. Just need to build the images! 🚀**
