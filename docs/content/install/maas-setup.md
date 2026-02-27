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

The maas-controller deploys gateway-level AuthPolicy and TokenRateLimitPolicy from
`maas-controller/config/policies`. When using the ODH overlay or deploy script, these
are applied automatically. For manual install:

```shell
kubectl apply --server-side=true \
  -f <(kustomize build "https://github.com/opendatahub-io/models-as-a-service.git/maas-controller/config/policies?ref=main")
```

!!! note "Custom Token Review Audience"
    If you encounter `401 Unauthorized` errors when obtaining tokens, your cluster may use a custom token review audience. See [Troubleshooting - 401 Errors](validation.md#common-issues) for detection and resolution steps.

## Next steps

* **Deploy models.** See the Quick Start for
  [sample model deployments](../quickstart.md#model-setup) that you
  can use to try the MaaS capability.
* **Perform validation.** Follow the [validation guide](validation.md) to verify that
  MaaS is working correctly.
