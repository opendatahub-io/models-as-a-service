# E2E Test: TRLP Merge Strategy (RHOAIENG-53869)

## Overview

This test verifies that multiple MaaSModelRefs pointing to the same HTTPRoute can coexist without conflicts by using `defaults.strategy: merge` in TokenRateLimitPolicy resources.

## Prerequisites

- MaaS deployed with maas-controller containing the merge strategy fix
- Access to create MaaSModelRef and MaaSSubscription resources
- At least one deployed LLMInferenceService

## Test Scenario

### Setup

1. **Identify an existing model**:
   ```bash
   LLMISVC_NAME=$(kubectl get llminferenceservice -A -o jsonpath='{.items[0].metadata.name}')
   LLMISVC_NS=$(kubectl get llminferenceservice -A -o jsonpath='{.items[0].metadata.namespace}')
   HTTPROUTE=$(kubectl get maasmodelref -A -l maas.opendatahub.io/model=${LLMISVC_NAME} -o jsonpath='{.items[0].status.httpRouteName}' 2>/dev/null)
   ```

2. **Create namespace for test resources** (if needed):
   ```bash
   kubectl create namespace models-as-a-service
   ```

### Test Steps

#### Step 1: Create Two MaaSModelRefs Pointing to the Same Model

```bash
kubectl apply -f - <<EOF
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: e2e-test-model-ref-a
  namespace: ${LLMISVC_NS}
spec:
  modelRef:
    kind: LLMInferenceService
    name: ${LLMISVC_NAME}
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: e2e-test-model-ref-b
  namespace: ${LLMISVC_NS}
spec:
  modelRef:
    kind: LLMInferenceService
    name: ${LLMISVC_NAME}
EOF
```

#### Step 2: Verify Both Models Resolve to the Same HTTPRoute

```bash
kubectl get maasmodelref e2e-test-model-ref-a e2e-test-model-ref-b -n ${LLMISVC_NS} \
  -o custom-columns=NAME:.metadata.name,PHASE:.status.phase,HTTPROUTE:.status.httpRouteName
```

**Expected**: Both show the same HTTPRoute name and both are in `Ready` phase.

#### Step 3: Create Subscriptions for Each Model

```bash
kubectl apply -f - <<EOF
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: e2e-test-sub-a
  namespace: models-as-a-service
spec:
  owner:
    users: ["test-user-a"]
  modelRefs:
  - name: e2e-test-model-ref-a
    namespace: ${LLMISVC_NS}
    tokenRateLimits:
    - limit: 1000
      window: 1m
---
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: e2e-test-sub-b
  namespace: models-as-a-service
spec:
  owner:
    users: ["test-user-b"]
  modelRefs:
  - name: e2e-test-model-ref-b
    namespace: ${LLMISVC_NS}
    tokenRateLimits:
    - limit: 5000
      window: 1m
EOF
```

#### Step 4: Wait for Controller Reconciliation

```bash
sleep 10
```

#### Step 5: Verify Both TRLPs Are Created

```bash
kubectl get tokenratelimitpolicy -n ${LLMISVC_NS} -l app.kubernetes.io/managed-by=maas-controller | grep e2e-test-model-ref
```

**Expected**: Two TRLPs visible:
- `maas-trlp-e2e-test-model-ref-a`
- `maas-trlp-e2e-test-model-ref-b`

#### Step 6: Verify Both TRLPs Have Merge Strategy

```bash
echo "Checking TRLP-A strategy:"
kubectl get tokenratelimitpolicy maas-trlp-e2e-test-model-ref-a -n ${LLMISVC_NS} \
  -o jsonpath='{.spec.defaults.strategy}' && echo

echo "Checking TRLP-B strategy:"
kubectl get tokenratelimitpolicy maas-trlp-e2e-test-model-ref-b -n ${LLMISVC_NS} \
  -o jsonpath='{.spec.defaults.strategy}' && echo
```

**Expected**: Both output `merge`

#### Step 7: Verify Both TRLPs Target the Same HTTPRoute

```bash
echo "TRLP-A target:"
kubectl get tokenratelimitpolicy maas-trlp-e2e-test-model-ref-a -n ${LLMISVC_NS} \
  -o jsonpath='{.spec.targetRef.name}' && echo

echo "TRLP-B target:"
kubectl get tokenratelimitpolicy maas-trlp-e2e-test-model-ref-b -n ${LLMISVC_NS} \
  -o jsonpath='{.spec.targetRef.name}' && echo
```

**Expected**: Both output the same HTTPRoute name

#### Step 8: Verify Both TRLPs Show Enforced Status (**CRITICAL**)

```bash
echo "TRLP-A Enforced status:"
kubectl get tokenratelimitpolicy maas-trlp-e2e-test-model-ref-a -n ${LLMISVC_NS} \
  -o jsonpath='{.status.conditions[?(@.type=="Enforced")].status}' && echo

echo "TRLP-B Enforced status:"
kubectl get tokenratelimitpolicy maas-trlp-e2e-test-model-ref-b -n ${LLMISVC_NS} \
  -o jsonpath='{.status.conditions[?(@.type=="Enforced")].status}' && echo
```

**Expected**: Both output `True`

**Failure Mode** (without merge strategy):
- One TRLP shows `False` with reason `Overridden`
- HTTPRoute affected by only one policy
- Only one rate limit configured in Limitador

#### Step 9: Verify HTTPRoute Recognizes Both Policies

```bash
ROUTE_NAME=$(kubectl get tokenratelimitpolicy maas-trlp-e2e-test-model-ref-a -n ${LLMISVC_NS} \
  -o jsonpath='{.spec.targetRef.name}')

kubectl get httproute ${ROUTE_NAME} -n ${LLMISVC_NS} \
  -o jsonpath='{.status.parents[?(@.controllerName=="kuadrant.io/policy-controller")].conditions[?(@.type=="kuadrant.io/TokenRateLimitPolicyAffected")].message}'
```

**Expected**: Output contains both policy names:
```
Object affected by TokenRateLimitPolicy [<namespace>/maas-trlp-e2e-test-model-ref-a <namespace>/maas-trlp-e2e-test-model-ref-b]
```

#### Step 10: Verify Limitador Has Both Rate Limits

```bash
kubectl get limitador -n rh-connectivity-link -o yaml | grep -A 5 "e2e-test"
```

**Expected**: See limits for both subscriptions:
- `e2e-test-sub-a-e2e-test-model-ref-a-tokens` with `max_value: 1000`
- `e2e-test-sub-b-e2e-test-model-ref-b-tokens` with `max_value: 5000`

### Cleanup

```bash
kubectl delete maassubscription e2e-test-sub-a e2e-test-sub-b -n models-as-a-service
kubectl delete maasmodelref e2e-test-model-ref-a e2e-test-model-ref-b -n ${LLMISVC_NS}
```

## Success Criteria

✅ **All of the following must be true**:

1. Both MaaSModelRefs created successfully and both are `Ready`
2. Both resolve to the **same HTTPRoute**
3. Both TokenRateLimitPolicies created
4. Both TRLPs have `spec.defaults.strategy: merge`
5. Both TRLPs target the **same HTTPRoute**
6. **Both TRLPs show `Enforced: True`** (no "Overridden" status)
7. HTTPRoute lists **both policies** in affected message
8. Limitador has rate limits from **both subscriptions**
9. No errors or conflicts in maas-controller logs

## Failure Indicators

❌ **Test fails if any of these occur**:

1. One TRLP shows `Enforced: False` with reason `Overridden`
2. HTTPRoute only lists one policy in affected message
3. Limitador only has one set of rate limits
4. Controller logs show policy conflicts
5. TRLPs don't have `spec.defaults.strategy: merge`

## Related

- **JIRA**: [RHOAIENG-53869](https://redhat.atlassian.net/browse/RHOAIENG-53869)
- **Related Bug**: [RHOAIENG-53865](https://redhat.atlassian.net/browse/RHOAIENG-53865)
- **Manual Test Results**: `/MANUAL-TEST-RESULTS.md` in repository root
- **Unit Tests**: `maas-controller/pkg/controller/maas/maassubscription_controller_merge_test.go`
