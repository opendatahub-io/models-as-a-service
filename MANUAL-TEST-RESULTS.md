# Manual Testing Results: TRLP Merge Strategy

**Date**: 2026-03-19
**Test Engineer**: Egor Lunin
**Controller Image**: `quay.io/elunin/maas-controller:test-merge-strategy`
**Status**: ✅ **SUCCESS**

---

## Test Objective

Verify that the TRLP merge strategy implementation allows multiple MaaSModelRefs pointing to the same HTTPRoute to coexist without conflicts.

---

## Test Setup

### Model
- **LLMInferenceService**: `facebook-opt-125m-simulated` in namespace `llm`
- **HTTPRoute**: `facebook-opt-125m-simulated-kserve-route`
- **Gateway**: `maas-default-gateway` in namespace `openshift-ingress`

### MaaSModelRefs (Both pointing to the same model)
```yaml
# test-model-ref-a in namespace: llm
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: test-model-ref-a
  namespace: llm
spec:
  modelRef:
    kind: LLMInferenceService
    name: facebook-opt-125m-simulated

# test-model-ref-b in namespace: llm
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: test-model-ref-b
  namespace: llm
spec:
  modelRef:
    kind: LLMInferenceService
    name: facebook-opt-125m-simulated  # Same model!
```

### Subscriptions
```yaml
# test-sub-a: 1000 tokens/min
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: test-sub-a
  namespace: models-as-a-service
spec:
  owner:
    users: ["test-user-a"]
  modelRefs:
  - name: test-model-ref-a
    namespace: llm
    tokenRateLimits:
    - limit: 1000
      window: 1m

# test-sub-b: 5000 tokens/min
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSSubscription
metadata:
  name: test-sub-b
  namespace: models-as-a-service
spec:
  owner:
    users: ["test-user-b"]
  modelRefs:
  - name: test-model-ref-b
    namespace: llm
    tokenRateLimits:
    - limit: 5000
      window: 1m
```

---

## Test Results

### ✅ 1. Both MaaSModelRefs Created Successfully

```bash
$ kubectl get maasmodelref -n llm
NAME               PHASE   ENDPOINT                                                                                  HTTPROUTE                                  GATEWAY                AGE
test-model-ref-a   Ready   https://maas.apps.rosa.elunin.rwc6.p3.openshiftapps.com/llm/facebook-opt-125m-simulated   facebook-opt-125m-simulated-kserve-route   maas-default-gateway   5m
test-model-ref-b   Ready   https://maas.apps.rosa.elunin.rwc6.p3.openshiftapps.com/llm/facebook-opt-125m-simulated   facebook-opt-125m-simulated-kserve-route   maas-default-gateway   5m
```

**Observation**: Both models resolve to the **same HTTPRoute**: `facebook-opt-125m-simulated-kserve-route`

---

### ✅ 2. Both TokenRateLimitPolicies Created

```bash
$ kubectl get tokenratelimitpolicy -n llm -l app.kubernetes.io/managed-by=maas-controller
NAMESPACE   NAME                         AGE
llm         maas-trlp-test-model-ref-a   5m
llm         maas-trlp-test-model-ref-b   5m
```

---

### ✅ 3. Both TRLPs Have `strategy: merge`

**TRLP-A**:
```json
{
  "spec": {
    "defaults": {
      "strategy": "merge",
      "limits": { ... }
    }
  }
}
```

**TRLP-B**:
```json
{
  "spec": {
    "defaults": {
      "strategy": "merge",
      "limits": { ... }
    }
  }
}
```

---

### ✅ 4. Both TRLPs Target the Same HTTPRoute

**TRLP-A targetRef**:
```json
{
  "group": "gateway.networking.k8s.io",
  "kind": "HTTPRoute",
  "name": "facebook-opt-125m-simulated-kserve-route"
}
```

**TRLP-B targetRef**:
```json
{
  "group": "gateway.networking.k8s.io",
  "kind": "HTTPRoute",
  "name": "facebook-opt-125m-simulated-kserve-route"
}
```

**✅ SAME HTTPRoute** - This is the critical scenario we're testing!

---

### ✅ 5. BOTH TRLPs Show `Enforced: True` (NO CONFLICTS!)

**TRLP-A Status**:
```json
{
  "type": "Enforced",
  "status": "True",
  "reason": "Enforced",
  "message": "TokenRateLimitPolicy has been successfully enforced"
}
```

**TRLP-B Status**:
```json
{
  "type": "Enforced",
  "status": "True",
  "reason": "Enforced",
  "message": "TokenRateLimitPolicy has been successfully enforced"
}
```

**🎉 SUCCESS**: No "Overridden" status! Both policies are enforced simultaneously.

---

### ✅ 6. HTTPRoute Recognizes Both Policies

```json
{
  "type": "kuadrant.io/TokenRateLimitPolicyAffected",
  "status": "True",
  "reason": "Accepted",
  "message": "Object affected by TokenRateLimitPolicy [llm/maas-trlp-test-model-ref-a llm/maas-trlp-test-model-ref-b]"
}
```

**Observation**: HTTPRoute shows **both policies** in the affected list.

---

### ✅ 7. Limitador Has Both Rate Limits Configured

**Limits extracted from Limitador CR**:

```yaml
limits:
  - name: test-sub-a-test-model-ref-a-tokens
    namespace: llm/facebook-opt-125m-simulated-kserve-route
    max_value: 1000
    seconds: 60

  - name: test-sub-b-test-model-ref-b-tokens
    namespace: llm/facebook-opt-125m-simulated-kserve-route
    max_value: 5000
    seconds: 60
```

**✅ Both limits** are present and configured with the correct values!

---

## Comparison: Before vs After

| Aspect | Before (without merge) | After (with merge) |
|--------|------------------------|-------------------|
| **TRLP-A Status** | `Enforced: False` (Overridden) | ✅ `Enforced: True` |
| **TRLP-B Status** | `Enforced: True` | ✅ `Enforced: True` |
| **HTTPRoute Status** | Only one policy recognized | ✅ Both policies recognized |
| **Limitador Limits** | Only one limit configured | ✅ Both limits configured |
| **Actual Behavior** | ❌ Only one model's rate limit works | ✅ Both models' rate limits work |

---

## Conclusion

### ✅ **Test PASSED**

The merge strategy implementation successfully allows multiple TokenRateLimitPolicies to target the same HTTPRoute without conflicts.

**Key Findings**:
1. ✅ Multiple MaaSModelRefs can point to the same LLMInferenceService
2. ✅ Each creates its own TRLP with `defaults.strategy: merge`
3. ✅ Both TRLPs target the same HTTPRoute
4. ✅ Both TRLPs show `Enforced: True` (no "Overridden" conflict)
5. ✅ Kuadrant recognizes both policies on the HTTPRoute
6. ✅ Limitador configures rate limits from both policies
7. ✅ No errors or warnings in controller logs

**Root Cause Fixed**: The issue was that MaaS used top-level `limits` in TRLP specs, which defaulted to Kuadrant's `atomic` strategy. With atomic strategy, only one policy could be enforced per HTTPRoute. By switching to `defaults.strategy: merge`, multiple policies can now coexist and be enforced simultaneously.

---

## Controller Logs

Controller successfully created both TRLPs without errors:

```
{"level":"info","ts":"2026-03-19T15:32:42Z","msg":"TokenRateLimitPolicy created","name":"maas-trlp-test-model-ref-a","model":"test-model-ref-a","subscriptions":["test-sub-a"]}
{"level":"info","ts":"2026-03-19T15:32:43Z","msg":"TokenRateLimitPolicy created","name":"maas-trlp-test-model-ref-b","model":"test-model-ref-b","subscriptions":["test-sub-b"]}
```

---

## Next Steps

1. ✅ **Phase 1 Implementation**: Complete
2. ✅ **Manual Testing**: Complete and successful
3. ⏭️ **Phase 2**: Add unit and E2E tests
4. ⏭️ **Phase 3**: Update documentation
5. ⏭️ **Create PR**: Submit for review

---

## Test Environment

- **Cluster**: ROSA (AWS)
- **Kubernetes**: OpenShift 4.x
- **Kuadrant Version**: v1.3.1
- **MaaS Controller**: `quay.io/elunin/maas-controller:test-merge-strategy`
- **Branch**: `feat/RHOAIENG-53869-trlp-merge-strategy`
- **Commit**: `d9256f0`

---

## References

- **JIRA**: [RHOAIENG-53869](https://redhat.atlassian.net/browse/RHOAIENG-53869)
- **Related Bug**: [RHOAIENG-53865](https://redhat.atlassian.net/browse/RHOAIENG-53865)
- **Experimental Test Results**: `RHOAIENG-53869-experimental-test-results.md`
- **Implementation Decision**: `RHOAIENG-53869-DECISION.md`
