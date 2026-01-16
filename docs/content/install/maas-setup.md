# Install MaaS Components

After enabling MaaS in your DataScienceCluster (set `modelsAsService.managementState: Managed`
in the `spec.components.kserve` section - see [platform setup guide](platform-setup.md#install-platform-with-model-serving)
for the complete configuration), the operator will automatically deploy:

- **MaaS API** (Deployment, Service, ServiceAccount, ClusterRole, ClusterRoleBinding, HTTPRoute)
- **MaaS API AuthPolicy** (maas-api-auth-policy) - Protects the MaaS API endpoint
- **NetworkPolicy** (maas-authorino-allow) - Allows Authorino to reach MaaS API

You must manually install the following components after completing the [platform setup](platform-setup.md)
(which includes creating the required `maas-default-gateway`):

The tools you will need:

* `kubectl` or `oc` client (this guide uses `kubectl`)
* `kustomize`
* `envsubst`

## Install Gateway AuthPolicy

Install the authentication policy for the Gateway. This policy applies to model inference traffic
and integrates with the MaaS API for tier-based access control:

```shell
# For RHOAI installations (MaaS API in redhat-ods-applications namespace)
kubectl apply --server-side=true \
  -f <(kustomize build "https://github.com/opendatahub-io/models-as-a-service.git/deployment/base/policies/auth-policies?ref=main" | \
       sed "s/maas-api\.maas-api\.svc/maas-api.redhat-ods-applications.svc/g")

# For ODH installations (MaaS API in opendatahub namespace)
kubectl apply --server-side=true \
  -f <(kustomize build "https://github.com/opendatahub-io/models-as-a-service.git/deployment/base/policies/auth-policies?ref=main" | \
       sed "s/maas-api\.maas-api\.svc/maas-api.opendatahub.svc/g")
```

### Configuring Custom Token Review Audience

If your cluster uses a custom token review audience (not the default `https://kubernetes.default.svc`),
you must patch the `maas-api-auth-policy` to include your cluster's audience:

```shell
# Detect your cluster's audience
AUD="$(kubectl create token default --duration=10m 2>/dev/null | cut -d. -f2 | jq -Rr '@base64d | fromjson | .aud[0]' 2>/dev/null)"

echo "Cluster audience: ${AUD}"

# Patch the MaaS API AuthPolicy with your cluster's audience
# For RHOAI installations:
kubectl patch authpolicy maas-api-auth-policy -n redhat-ods-applications --type=merge --patch "
spec:
  rules:
    authentication:
      openshift-identities:
        kubernetesTokenReview:
          audiences:
            - ${AUD}
            - maas-default-gateway-sa"

# For ODH installations:
kubectl patch authpolicy maas-api-auth-policy -n opendatahub --type=merge --patch "
spec:
  rules:
    authentication:
      openshift-identities:
        kubernetesTokenReview:
          audiences:
            - ${AUD}
            - maas-default-gateway-sa"
```

## Install Usage Policies

Install rate limiting policies (TokenRateLimitPolicy and RateLimitPolicy):

```shell
export CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')

kubectl apply --server-side=true \
  -f <(kustomize build "https://github.com/opendatahub-io/models-as-a-service.git/deployment/base/policies/usage-policies?ref=main" | \
       envsubst '$CLUSTER_DOMAIN')
```

These policies define:

* **TokenRateLimitPolicy** - Rate limits based on token consumption per tier
* **RateLimitPolicy** - Request rate limits per tier

See [Tier Management](../configuration-and-management/tier-overview.md) for more details on
configuring usage policies and tiers.

## Next steps

* **Deploy models.** In the Quick Start, we provide
  [sample deployments](../quickstart.md#deploy-sample-models-optional) that you
  can use to try the MaaS capability.
* **Perform validation.** Follow the [validation guide](validation.md) to verify that
  MaaS is working correctly.
