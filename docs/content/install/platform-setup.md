# Install Open Data Hub

This guide covers the installation of Open Data Hub (ODH) with the required configuration
to enable the Models-as-a-Service capability (MaaS).

!!! note "Red Hat OpenShift AI"
    Red Hat OpenShift AI (RHOAI) is also compatible. The installation steps are similar;
    platform-specific differences are noted in the tabs throughout this guide.

## Prerequisites

You need a Red Hat OpenShift cluster version 4.19.9 or later. Older OpenShift versions are not suitable.

**Required tools:** See [Prerequisites Overview](prerequisites.md#required-tools).

This section walks through the installation of the required Operators:

* cert-manager
* LeaderWorkerSet
* Kuadrant (or RHCL)
* Platform operator (ODH or RHOAI)

!!! note "Documentation References"
    This guide is provided for convenience. In case of any issues or more advanced setups:

    - **ODH**: Refer to [Kuadrant documentation](https://docs.kuadrant.io)
    - **RHOAI**: Refer to [Red Hat documentation](https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed)

## Install cert-manager

cert-manager is required for TLS certificate management. It must be installed before
LeaderWorkerSet.

=== "Open Data Hub"

    Install cert-manager using the upstream kubectl method:

    ```shell
    kubectl apply -f https://github.com/cert-manager/cert-manager/releases/latest/download/cert-manager.yaml
    ```

=== "Red Hat OpenShift AI"

    Install the cert-manager Operator from OpenShift's built-in OperatorHub:

    ```yaml
    kubectl apply -f - <<EOF
    apiVersion: v1
    kind: Namespace
    metadata:
      name: cert-manager-operator
    ---
    apiVersion: operators.coreos.com/v1
    kind: OperatorGroup
    metadata:
      name: cert-manager-operator
      namespace: cert-manager-operator
    ---
    apiVersion: operators.coreos.com/v1alpha1
    kind: Subscription
    metadata:
      name: openshift-cert-manager-operator
      namespace: cert-manager-operator
    spec:
      channel: stable-v1
      installPlanApproval: Automatic
      name: openshift-cert-manager-operator
      source: redhat-operators
      sourceNamespace: openshift-marketplace
    EOF
    ```

    Wait for the subscription to install successfully:

    ```shell
    kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown subscription/openshift-cert-manager-operator -n cert-manager-operator --timeout=300s
    ```

### Verification

Verify that the cert-manager CRDs are available:

```shell
kubectl get crd certificates.cert-manager.io clusterissuers.cert-manager.io issuers.cert-manager.io
```

Check that the cert-manager pods are running:

=== "Open Data Hub"

    ```shell
    kubectl get pods -n cert-manager
    ```

=== "Red Hat OpenShift AI"

    ```shell
    kubectl get pods -n cert-manager-operator
    ```

## Install LeaderWorkerSet API

=== "Open Data Hub"

    Install the latest version of LWS by using the _kubectl_ method from
    [LWS official documentation](https://lws.sigs.k8s.io/docs/installation/#install-by-kubectl):

    ```shell
    GH_LATEST_LWS_ENTRY_URL="https://api.github.com/repos/kubernetes-sigs/lws/releases"
    LATEST_LWS_VERSION=$(curl -sSf ${GH_LATEST_LWS_ENTRY_URL} | jq -r 'sort_by(.tag_name|ltrimstr("v")|split(".")|map(tonumber)) | last | .tag_name')

    kubectl apply --server-side -f https://github.com/kubernetes-sigs/lws/releases/download/${LATEST_LWS_VERSION}/manifests.yaml
    ```

=== "Red Hat OpenShift AI"

    Install Red Hat LeaderWorkerSet API (LWS) Operator from OpenShift's built-in OperatorHub:

    ```yaml
    kubectl apply -f - <<EOF
    apiVersion: v1
    kind: Namespace
    metadata:
      name: openshift-lws-operator
    ---
    apiVersion: operators.coreos.com/v1
    kind: OperatorGroup
    metadata:
      name: leader-worker-set
      namespace: openshift-lws-operator
    spec:
      targetNamespaces:
      - openshift-lws-operator
    ---
    apiVersion: operators.coreos.com/v1alpha1
    kind: Subscription
    metadata:
      name: leader-worker-set
      namespace: openshift-lws-operator
    spec:
      channel: stable-v1.0
      installPlanApproval: Automatic
      name: leader-worker-set
      source: redhat-operators
      sourceNamespace: openshift-marketplace
    EOF
    ```

    Wait for the subscription to install successfully:

    ```shell
    kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown subscription/leader-worker-set -n openshift-lws-operator --timeout=300s
    ```

    Once the LWS operator is ready, set up the LWS API:

    ```yaml
    kubectl apply -f - <<EOF
    apiVersion: operator.openshift.io/v1
    kind: LeaderWorkerSetOperator
    metadata:
      name: cluster
      namespace: openshift-lws-operator
    spec:
      managementState: Managed
    EOF
    ```

    Check [Red Hat LWS documentation](https://docs.redhat.com/en/documentation/openshift_container_platform/latest/html/ai_workloads/leader-worker-set-operator)
    if you need further guidance.

### Verification

Check that LWS deployments are ready:

=== "Open Data Hub"

    ```shell
    kubectl get deployments --namespace lws-system -w
    ```

    ```
    NAME                     READY   UP-TO-DATE   AVAILABLE   AGE
    lws-controller-manager   2/2     2            2           35s
    ```

=== "Red Hat OpenShift AI"

    ```shell
    kubectl get deployments --namespace openshift-lws-operator -w
    ```

    ```
    NAME                     READY   UP-TO-DATE   AVAILABLE   AGE
    lws-controller-manager   2/2     2            2           61s
    openshift-lws-operator   1/1     1            1           4m26s
    ```

## Install Gateway API Controller

Initialize OpenShift's provided Gateway API implementation:

```yaml
kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: openshift-default
spec:
  controllerName: "openshift.io/gateway-controller/v1"
EOF
```

Wait until the GatewayClass resource is accepted:

```shell
kubectl get gatewayclass openshift-default -w
```

```
NAME                CONTROLLER                           ACCEPTED   AGE
openshift-default   openshift.io/gateway-controller/v1   True       52s
```

Now install the Gateway API controller for your platform:

=== "Open Data Hub"

    Install Kuadrant using the OLM method. MaaS requires Kuadrant v1.4.2 or later.

    Create the `kuadrant-system` namespace and OperatorGroup:

    ```yaml
    kubectl create namespace kuadrant-system

    kubectl apply -f - <<EOF
    apiVersion: operators.coreos.com/v1
    kind: OperatorGroup
    metadata:
      name: kuadrant-operator-group
      namespace: kuadrant-system
    spec: {}
    EOF
    ```

    !!! note
        A single OperatorGroup should exist in any given namespace. Check for the
        existence of multiple OperatorGroups if Kuadrant operator is not deployed
        successfully.

    Configure Kuadrant's CatalogSource:

    ```yaml
    GH_LATEST_KUADRANT_ENTRY_URL="https://api.github.com/repos/Kuadrant/kuadrant-operator/releases/latest"
    LATEST_KUADRANT_VERSION=$(curl -sSf ${GH_LATEST_KUADRANT_ENTRY_URL} | jq -r '.tag_name')

    kubectl apply -f - <<EOF
    apiVersion: operators.coreos.com/v1alpha1
    kind: CatalogSource
    metadata:
      name: kuadrant-operator-catalog
      namespace: kuadrant-system
    spec:
      displayName: Kuadrant Operators
      image: quay.io/kuadrant/kuadrant-operator-catalog:${LATEST_KUADRANT_VERSION}
      sourceType: grpc
    EOF
    ```

    Deploy the Kuadrant operator, configuring it to work with OpenShift's Gateway API:

    ```yaml
    kubectl apply -f - <<EOF
    apiVersion: operators.coreos.com/v1alpha1
    kind: Subscription
    metadata:
      name: kuadrant-operator
      namespace: kuadrant-system
    spec:
      channel: stable
      installPlanApproval: Automatic
      name: kuadrant-operator
      source: kuadrant-operator-catalog
      sourceNamespace: kuadrant-system
      config:
        env:
        - name: "ISTIO_GATEWAY_CONTROLLER_NAMES"
          value: "openshift.io/gateway-controller/v1"
    EOF
    ```

    !!! note "Rate limiting when Limitador is unavailable"
        If the Limitador service is unavailable, rate limits may not be enforced unless the Kuadrant operator is configured to deny traffic on failure. Set `RATELIMIT_CHECK_SERVICE_FAILURE_MODE` and `RATELIMIT_REPORT_SERVICE_FAILURE_MODE` to `deny` on the operator Subscription (`spec.config.env`). Add them next to `ISTIO_GATEWAY_CONTROLLER_NAMES`, or apply the patch after install:

        ```shell
        kubectl patch subscription kuadrant-operator -n kuadrant-system --type='json' -p='[
          {"op":"add","path":"/spec/config/env/-","value":{"name":"RATELIMIT_CHECK_SERVICE_FAILURE_MODE","value":"deny"}},
          {"op":"add","path":"/spec/config/env/-","value":{"name":"RATELIMIT_REPORT_SERVICE_FAILURE_MODE","value":"deny"}}
        ]'
        ```

    Wait for the subscription to install successfully:

    ```shell
    kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown subscription/kuadrant-operator -n kuadrant-system --timeout=300s
    ```

    Wait for the operator webhook to be ready:

    ```shell
    kubectl wait --for=condition=Available --timeout=120s deployment/kuadrant-operator-controller-manager -n kuadrant-system
    ```

    Once the Kuadrant operator is ready, create a Kuadrant instance:

    ```yaml
    kubectl apply -f - <<EOF
    apiVersion: kuadrant.io/v1beta1
    kind: Kuadrant
    metadata:
      name: kuadrant
      namespace: kuadrant-system
    EOF
    ```

=== "Red Hat OpenShift AI"

    Install Red Hat Connectivity Link (RHCL) Operator from OpenShift's built-in OperatorHub.
    MaaS requires RHCL v1.3 or later:

    ```yaml
    kubectl apply -f - <<EOF
    apiVersion: v1
    kind: Namespace
    metadata:
      name: kuadrant-system
    ---
    apiVersion: operators.coreos.com/v1
    kind: OperatorGroup
    metadata:
      name: kuadrant-operator-group
      namespace: kuadrant-system
    ---
    apiVersion: operators.coreos.com/v1alpha1
    kind: Subscription
    metadata:
      name: kuadrant-operator
      namespace: kuadrant-system
    spec:
      channel: stable
      installPlanApproval: Automatic
      name: rhcl-operator
      source: redhat-operators
      sourceNamespace: openshift-marketplace
    EOF
    ```

    !!! note "Rate limiting when Limitador is unavailable"
        If the Limitador service is unavailable, rate limits may not be enforced unless the Kuadrant operator is configured to deny traffic on failure. Set `RATELIMIT_CHECK_SERVICE_FAILURE_MODE` and `RATELIMIT_REPORT_SERVICE_FAILURE_MODE` to `deny` on the operator Subscription (`spec.config.env`). Add a `config` section with those two variables before applying, or patch after install:

        ```shell
        kubectl patch subscription kuadrant-operator -n kuadrant-system --type='json' -p='[
          {"op":"add","path":"/spec/config","value":{"env":[
            {"name":"RATELIMIT_CHECK_SERVICE_FAILURE_MODE","value":"deny"},
            {"name":"RATELIMIT_REPORT_SERVICE_FAILURE_MODE","value":"deny"}
          ]}}
        ]'
        ```

    #### High-concurrency authentication timeout

    RHCL configures the Kuadrant WASM plugin with an authentication service timeout of `200ms` by default. Under high concurrent request load, this short timeout can appear as intermittent HTTP `500` or `503` responses from gateway policy evaluation even when the model backend is healthy. If you see timeout-related failures during concurrent inference, increase `AUTH_SERVICE_TIMEOUT` to `2s` (`2000ms`) on the RHCL operator Subscription (`spec.config.env`).

    Set the RHCL Subscription name and namespace for your installation before patching. The manual install example above uses `kuadrant-operator` in `kuadrant-system`; RHOAI-managed clusters commonly use `rhcl-operator` in `rh-connectivity-link`.

    ```shell
    RHCL_NAMESPACE=kuadrant-system
    RHCL_SUBSCRIPTION=kuadrant-operator

    # For RHOAI-managed RHCL, use:
    # RHCL_NAMESPACE=rh-connectivity-link
    # RHCL_SUBSCRIPTION=rhcl-operator
    ```

    To configure the timeout before installation, add the environment variable to the Subscription. If you already set other `spec.config.env` entries, add it alongside them:

    ```yaml
    spec:
      config:
        env:
          - name: AUTH_SERVICE_TIMEOUT
            value: "2s"
    ```

    To configure the timeout after installation, choose the patch that matches the current Subscription shape:

    If `spec.config.env` already exists, append the timeout value:

    ```shell
    kubectl patch subscription "${RHCL_SUBSCRIPTION}" -n "${RHCL_NAMESPACE}" --type='json' -p='[
      {"op":"add","path":"/spec/config/env/-","value":{"name":"AUTH_SERVICE_TIMEOUT","value":"2s"}}
    ]'
    ```

    If `spec.config` exists but `spec.config.env` does not, add the `env` array:

    ```shell
    kubectl patch subscription "${RHCL_SUBSCRIPTION}" -n "${RHCL_NAMESPACE}" --type='json' -p='[
      {"op":"add","path":"/spec/config/env","value":[
        {"name":"AUTH_SERVICE_TIMEOUT","value":"2s"}
      ]}
    ]'
    ```

    If the Subscription does not have a `spec.config` section yet, create it with the timeout value:

    ```shell
    kubectl patch subscription "${RHCL_SUBSCRIPTION}" -n "${RHCL_NAMESPACE}" --type='json' -p='[
      {"op":"add","path":"/spec/config","value":{"env":[
        {"name":"AUTH_SERVICE_TIMEOUT","value":"2s"}
      ]}}
    ]'
    ```

    After changing the operator Subscription, wait for RHCL-managed components to roll out and retest the concurrent workload. Keep the value as low as your workload allows: increasing it gives authentication calls more time under load, but failed auth service calls can also take longer before the gateway returns an error.

    Wait for the subscription to install successfully:

    ```shell
    kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown subscription/kuadrant-operator -n kuadrant-system --timeout=300s
    ```

    Wait for the operator webhook to be ready:

    ```shell
    kubectl wait --for=condition=Available --timeout=120s deployment/kuadrant-operator-controller-manager -n kuadrant-system
    ```

    Watch for the remaining RHCL components to be ready:

    ```shell
    kubectl get deployments -n kuadrant-system -w
    ```

    ```
    NAME                                    READY   UP-TO-DATE   AVAILABLE   AGE
    authorino-operator                      1/1     1            1           23m
    dns-operator-controller-manager         1/1     1            1           23m
    kuadrant-console-plugin                 1/1     1            1           23m
    kuadrant-operator-controller-manager    1/1     1            1           23m
    limitador-operator-controller-manager   1/1     1            1           23m
    ```

    Once the RHCL operator is ready, create a Kuadrant instance:

    ```yaml
    kubectl apply -f - <<EOF
    apiVersion: kuadrant.io/v1beta1
    kind: Kuadrant
    metadata:
      name: kuadrant
      namespace: kuadrant-system
    EOF
    ```

### Verification

Check that Gateway API controller deployments are ready:

```shell
kubectl get deployments -n kuadrant-system -w
```

```
NAME                                    READY   UP-TO-DATE   AVAILABLE   AGE
authorino-operator                      1/1     1            1           80s
dns-operator-controller-manager         1/1     1            1           77s
kuadrant-console-plugin                 1/1     1            1           58s
kuadrant-operator-controller-manager    1/1     1            1           69s
limitador-operator-controller-manager   1/1     1            1           73s
```

For RHOAI installations, you should also see:

```
authorino                               1/1     1            1           81s
limitador-limitador                     1/1     1            1           82s
```

## Install Platform Operator

Install the platform operator (ODH or RHOAI) and initialize the platform with DSCInitialization. The DataScienceCluster and Gateway setup are in [Install MaaS Components](maas-setup.md).

=== "Open Data Hub"

    Install the Open Data Hub Project (ODH) operator, which is available in OpenShift's
    preconfigured CatalogSource of community operators. MaaS requires ODH v3.0 or later:

    ```yaml
    kubectl apply -f - <<EOF
    apiVersion: operators.coreos.com/v1alpha1
    kind: Subscription
    metadata:
      name: opendatahub-operator
      namespace: openshift-operators
    spec:
      channel: fast-3
      installPlanApproval: Automatic
      name: opendatahub-operator
      source: community-operators
      sourceNamespace: openshift-marketplace
    EOF
    ```

    Wait for the subscription to install successfully:

    ```shell
    kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown subscription/opendatahub-operator -n openshift-operators --timeout=300s
    ```

    Wait for the operator webhook to be ready:

    ```shell
    kubectl wait --for=condition=Available --timeout=120s deployment/opendatahub-operator-controller-manager -n openshift-operators
    ```

    Initialize the ODH platform with DSCInitialization:

    ```yaml
    kubectl apply -f - <<EOF
    apiVersion: dscinitialization.opendatahub.io/v2
    kind: DSCInitialization
    metadata:
      name: default-dsci
    spec:
      applicationsNamespace: opendatahub
      monitoring:
        managementState: Managed
        namespace: opendatahub
        metrics: {}
      trustedCABundle:
        managementState: Managed
    EOF
    ```

=== "Red Hat OpenShift AI"

    Install Red Hat OpenShift AI (RHOAI) Operator from OpenShift's built-in OperatorHub.
    MaaS requires RHOAI v3.0 or later:

    ```yaml
    kubectl apply -f - <<EOF
    apiVersion: v1
    kind: Namespace
    metadata:
      name: redhat-ods-operator
    ---
    apiVersion: operators.coreos.com/v1
    kind: OperatorGroup
    metadata:
      name: rhoai3-operatorgroup
      namespace: redhat-ods-operator
    ---
    apiVersion: operators.coreos.com/v1alpha1
    kind: Subscription
    metadata:
      name: rhoai3-operator
      namespace: redhat-ods-operator
    spec:
      channel: stable-3.x
      installPlanApproval: Automatic
      name: rhods-operator
      source: redhat-operators
      sourceNamespace: openshift-marketplace
    EOF
    ```

    Wait for the subscription to install successfully:

    ```shell
    kubectl wait --for=jsonpath='{.status.state}'=AtLatestKnown subscription/rhoai3-operator -n redhat-ods-operator --timeout=300s
    ```

    Wait for the operator webhook to be ready:

    ```shell
    kubectl wait --for=condition=Available --timeout=120s deployment/rhods-operator -n redhat-ods-operator
    ```

    Once ready, the RHOAI Operator automatically creates a `DSCInitialization` resource.

## Next Steps

1. [Install MaaS Components](maas-setup.md) - Database, Gateways, and Configure DataScienceCluster
2. [Deploy a Model](../configuration-and-management/model-setup.md) - Deploy your first model
