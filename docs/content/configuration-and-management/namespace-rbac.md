# Namespace User Permissions

This page describes the RBAC permissions for MaaS custom resources in user namespaces.

## ClusterRoles

MaaS provides two aggregated ClusterRoles that extend the standard Kubernetes/OpenShift roles with permissions for MaaS resources:

- **`maas-user-admin-role`** - Aggregates to `admin` and `edit` roles
- **`maas-user-view-role`** - Aggregates to `view` role

This allows namespace admins and contributors to create and manage MaaS resources without requiring cluster-admin intervention.

## Permission Matrix

| User Role | Resources | Permissions |
|-----------|-----------|-------------|
| **admin** | `MaaSModelRef`, `ExternalModel` | `create`, `delete`, `get`, `list`, `patch`, `update`, `watch` |
| **edit** | `MaaSModelRef`, `ExternalModel` | `create`, `delete`, `get`, `list`, `patch`, `update`, `watch` |
| **view** | `MaaSModelRef`, `ExternalModel` | `get`, `list`, `watch` |

### Included Resources

- **MaaSModelRef** - References to model backends (LLMInferenceService, ExternalModel)
- **ExternalModel** - External LLM provider definitions (OpenAI, Anthropic, etc.)

### Excluded Resources

The following platform-managed resources are **not** included:
- **MaaSSubscription** - Managed in the `models-as-a-service` namespace by platform admins
- **MaaSAuthPolicy** - Managed in the `models-as-a-service` namespace by platform admins

## Usage

### Creating a MaaSModelRef

Users with `admin` or `edit` role in their namespace can create MaaSModelRef resources:

```bash
kubectl create -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: MaaSModelRef
metadata:
  name: my-llm-model
  namespace: my-models
spec:
  modelRef:
    name: llama-3-8b-instruct
    kind: LLMInferenceService
EOF
```

### Creating an ExternalModel

```bash
kubectl create -f - <<EOF
apiVersion: maas.opendatahub.io/v1alpha1
kind: ExternalModel
metadata:
  name: openai-gpt-4
  namespace: my-models
spec:
  provider: openai
  endpoint: api.openai.com
  targetModel: gpt-4
  credentialRef:
    name: openai-credentials
EOF
```

### View-Only Access

Users with `view` role can list and inspect resources but cannot create or modify them:

```bash
# List MaaSModelRefs (allowed)
kubectl get maasmodelref -n my-models

# Get details (allowed)
kubectl describe maasmodelref my-llm-model -n my-models

# Delete (forbidden)
kubectl delete maasmodelref my-llm-model -n my-models
# Error: forbidden
```

## Verification

To verify your permissions:

```bash
# Check if you can create MaaSModelRef
kubectl auth can-i create maasmodelref -n my-models

# Check if you can list MaaSModelRef
kubectl auth can-i list maasmodelref -n my-models
```

To verify the ClusterRoles are installed:

```bash
# Check that admin role includes MaaS permissions
kubectl get clusterrole admin -o yaml | grep -A 5 "maas.opendatahub.io"

# Check that view role includes MaaS permissions
kubectl get clusterrole view -o yaml | grep -A 5 "maas.opendatahub.io"
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
kubectl create rolebinding my-models-admin \
  --clusterrole=admin \
  --user=user@example.com \
  -n my-models
```

### Cannot Create MaaSSubscription

**Problem:** You get a "Forbidden" error when trying to create a MaaSSubscription.

**Solution:** This is expected. `MaaSSubscription` and `MaaSAuthPolicy` are platform-managed resources and can only be created by cluster administrators. Contact your platform administrator if you need a new subscription.

## Related Documentation

- [Model Setup Guide](model-setup.md) - How to configure models for MaaS
- [Quota and Access Configuration](quota-and-access-configuration.md) - Platform admin guide for subscriptions
- [Self-Service Model Access](../user-guide/self-service-model-access.md) - End user guide for using models via API
