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

* LeaderWorkerSet
* Kuadrant (or RHCL)
* Platform operator (ODH or RHOAI)

!!! note "Documentation References"
    This guide is provided for convenience. In case of any issues or more advanced setups:

    - **ODH**: Refer to [Kuadrant documentation](https://docs.kuadrant.io)
    - **RHOAI**: Refer to [Red Hat documentation](https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed)

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

    Install Kuadrant using the OLM method. MaaS requires Kuadrant v1.3.1 or later.

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
    MaaS requires RHCL v1.2 or later:

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

    Wait for the operator webhook to be ready:

    ```shell
    
    
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
      channel: fast-3.x
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
    kubectl wait --for=condition=Available --timeout=120s deployment/rhods-operator-controller-manager -n redhat-ods-operator
    ```

    Once ready, the RHOAI Operator automatically creates a `DSCInitialization` resource.

## Next Steps

1. [Install MaaS Components](maas-setup.md) - Database, Gateways, and Configure DataScienceCluster
2. [Deploy a Model](../configuration-and-management/model-setup.md) - Deploy your first model
