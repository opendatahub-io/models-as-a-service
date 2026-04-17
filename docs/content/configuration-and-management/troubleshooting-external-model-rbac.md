# Troubleshooting: ExternalModel Service `ownerReference` / finalizers (RBAC)

## Symptoms

- `external-model-reconciler` logs show Service create failing with:

  ```text
  cannot set blockOwnerDeletion if an ownerReference refers to a resource you can't set finalizers on
  ```

- `MaaSModelRef` objects that reference `ExternalModel` backends stay `Pending` with a backend-not-ready reason.

## Cause

The reconciler sets a **controller `ownerReference`** on the `Service` it creates for an `ExternalModel`. With that pattern, the API server checks that the controller identity can **`update` the `externalmodels/finalizers` subresource** (OwnerReferencesPermissionEnforcement).

If the **`maas-controller` ServiceAccount** is not allowed that verb on that subresource, Service creation fails before routes are healthy.

## What to fix

1. **ClusterRole** `maas-controller-role` must include a rule that allows `update` on `externalmodels/finalizers` for API group `maas.opendatahub.io`.

   Source manifest in this repository: `deployment/base/maas-controller/rbac/clusterrole.yaml`.

2. **ClusterRoleBinding** `maas-controller-rolebinding` must bind that `ClusterRole` to the **`maas-controller` ServiceAccount** in the namespace where the controller runs (commonly `opendatahub` when using the ODH overlay).

On OpenShift, the `ModelsAsService` component may **own** these objects; if your live `ClusterRole` is missing the `externalmodels/finalizers` rule, upgrade or re-apply the manifest from this repo, or reconcile the component so the shipped RBAC matches.

## How to verify (important)

`oc auth can-i` **does not** treat `externalmodels/finalizers` as a single resource name the same way RBAC does. Using the slash form often returns **`no` even when the rule is present**.

Use the **`--subresource=finalizers`** form instead:

```bash
# Replace NAMESPACE with the namespace where ExternalModel CRs live (e.g. llm)
# Replace SA_NAMESPACE with the controller ServiceAccount namespace (e.g. opendatahub)

oc auth can-i update externalmodels --subresource=finalizers \
  -n NAMESPACE \
  --as=system:serviceaccount:SA_NAMESPACE:maas-controller
```

You should see **`yes`**.

**Incorrect (misleading false negative):**

```bash
# Often prints "no" even when RBAC is correct — do not use for verification
oc auth can-i update externalmodels/finalizers -n NAMESPACE \
  --as=system:serviceaccount:SA_NAMESPACE:maas-controller
```

## Optional: add the rule with `oc patch`

If you must patch the live `ClusterRole` (for example before an operator update ships the rule):

```bash
oc patch clusterrole maas-controller-role --type=json -p='[
  {
    "op": "add",
    "path": "/rules/-",
    "value": {
      "apiGroups": ["maas.opendatahub.io"],
      "resources": ["externalmodels/finalizers"],
      "verbs": ["update"]
    }
  }
]'
```

Then verify with the **`--subresource=finalizers`** command above, not the slash form.

## What we changed in docs (2026-04-14)

- Documented that **`oc auth can-i update externalmodels/finalizers`** can incorrectly report **`no`** when permission exists.
- Documented the supported check: **`oc auth can-i update externalmodels --subresource=finalizers`**.
- Pointed to **`deployment/base/maas-controller/rbac/clusterrole.yaml`** as the in-repo source for the `maas-controller-role` rules.

## Related

- [Namespace user permissions (RBAC)](namespace-rbac.md)
- [MaaS controller overview](maas-controller-overview.md)
