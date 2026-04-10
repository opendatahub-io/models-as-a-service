# Namespace User Permissions

This page describes the RBAC permissions for MaaS custom resources in user namespaces.

## ClusterRoles

MaaS provides two aggregated ClusterRoles that extend the standard Kubernetes/OpenShift roles with permissions for MaaS resources:

- **`maas-owner-role`** - Aggregates to `admin` and `edit` roles
- **`maas-viewer-role`** - Aggregates to `view`, `admin`, and `edit` roles

This allows namespace admins and contributors to create and manage MaaS resources without requiring cluster-admin intervention.

## Permission Matrix

| User Role | Resources | Permissions |
|-----------|-----------|-------------|
| **admin** | `MaaSModelRef`, `ExternalModel` | `create`, `delete`, `get`, `list`, `patch`, `update`, `watch` |
| **edit** | `MaaSModelRef`, `ExternalModel` | `create`, `delete`, `get`, `list`, `patch`, `update`, `watch` |
| **view** | `MaaSModelRef`, `ExternalModel` | `get`, `list`, `watch` |

### Included Resources

- **MaaSModelRef** - References to model backends (LLMInferenceService or ExternalModel backend)
- **ExternalModel** - External LLM provider definitions (OpenAI, Anthropic, etc.)

### Excluded Resources

The following platform-managed resources are **not** included:
- **MaaSSubscription** - Managed in the `models-as-a-service` namespace by platform admins
- **MaaSAuthPolicy** - Managed in the `models-as-a-service` namespace by platform admins


## Verification

### For Namespace Users

To verify your permissions in a namespace:

```bash
# Check if you can create MaaSModelRef
kubectl auth can-i create maasmodelref -n my-models

# Check if you can list MaaSModelRef
kubectl auth can-i list maasmodelref -n my-models
```

### For Platform Administrators

To verify the ClusterRoles are correctly installed and aggregated, run the RBAC verification script at `scripts/verify-rbac-aggregation.sh` in the repository root:

```bash
./scripts/verify-rbac-aggregation.sh
```

## Troubleshooting

### "Forbidden" Error When Creating MaaSModelRef

**Problem:**
```text
Error from server (Forbidden): maasmodelrefs.maas.opendatahub.io is forbidden: 
User "user@example.com" cannot create resource "maasmodelrefs" in API group 
"maas.opendatahub.io" in the namespace "my-models"
```

**Solution:**

You need the `admin` or `edit` role in the namespace. Ask your platform administrator to grant you access:

```bash
kubectl create rolebinding my-models-admin --clusterrole=admin --user=user@example.com -n my-models
```

### Cannot Create MaaSSubscription

**Problem:** You get a "Forbidden" error when trying to create a MaaSSubscription.

**Solution:** This is expected. `MaaSSubscription` and `MaaSAuthPolicy` are platform-managed resources and can only be created by cluster administrators. Contact your platform administrator if you need a new subscription.

## Related Documentation

- [Model Setup Guide](model-setup.md) - How to configure models for MaaS
- [Quota and Access Configuration](quota-and-access-configuration.md) - Platform admin guide for subscriptions
- [Self-Service Model Access](../user-guide/self-service-model-access.md) - End user guide for using models via API
