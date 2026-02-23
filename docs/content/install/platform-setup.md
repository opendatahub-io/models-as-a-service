# Install Open Data Hub or Red Hat OpenShift AI

This guide covers the installation of either Open Data Hub (ODH) or Red Hat OpenShift AI (RHOAI),
with the required configuration to enable the Models-as-a-Service capability (MaaS).

!!! note "Choose Your Platform"
    You should choose either **Open Data Hub** or **Red Hat OpenShift AI** - do not install both.
    The installation steps are similar with a few platform-specific differences noted throughout.

## Prerequisites

You need a Red Hat OpenShift cluster version 4.19.9 or later. Older OpenShift versions are not suitable.

MaaS requires the Model Serving component configured for deploying models with `LLMInferenceService`
resources. The prerequisites for this setup are a Gateway API controller (Kuadrant or RHCL) and the
LeaderWorkerSet API (LWS).

**Tools you will need:**

* `kubectl` or `oc` client (this guide uses `kubectl`)

**For ODH installations only:**

* `curl`
* `jq`

!!! note "Documentation References"
    This guide is provided for convenience. In case of any issues or more advanced setups:

    - **ODH**: Refer to [Kuadrant documentation](https://docs.kuadrant.io)
    - **RHOAI**: Refer to [Red Hat documentation](https://docs.redhat.com/en/documentation/red_hat_openshift_ai_self-managed)

## Install LeaderWorkerSet API

=== "Red Hat OpenShift AI"

    Install Red Hat LeaderWorkerSet API (LWS) Operator from OpenShift's built-in OperatorHub:

    ```yaml
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
    ```

    Once the LWS operator is ready, set up the LWS API:

    ```yaml
    apiVersion: operator.openshift.io/v1
    kind: LeaderWorkerSetOperator
    metadata:
      name: cluster
      namespace: openshift-lws-operator
    spec:
      managementState: Managed
    ```

    Check [Red Hat LWS documentation](https://docs.redhat.com/en/documentation/openshift_container_platform/latest/html/ai_workloads/leader-worker-set-operator)
    if you need further guidance.

=== "Open Data Hub"

    Install the latest version of LWS by using the _kubectl_ method from
    [LWS official documentation](https://lws.sigs.k8s.io/docs/installation/#install-by-kubectl):

    ```shell
    GH_LATEST_LWS_ENTRY_URL="https://api.github.com/repos/kubernetes-sigs/lws/releases"
    LATEST_LWS_VERSION=$(curl -sSf ${GH_LATEST_LWS_ENTRY_URL} | jq -r 'sort_by(.tag_name|ltrimstr("v")|split(".")|map(tonumber)) | last | .tag_name')

    kubectl apply --server-side -f https://github.com/kubernetes-sigs/lws/releases/download/${LATEST_LWS_VERSION}/manifests.yaml
    ```

### Verification

Check that LWS deployments are ready:

=== "Red Hat OpenShift AI"

    ```shell
    kubectl get deployments --namespace openshift-lws-operator

    NAME                     READY   UP-TO-DATE   AVAILABLE   AGE
    lws-controller-manager   2/2     2            2           61s
    openshift-lws-operator   1/1     1            1           4m26s
    ```

=== "Open Data Hub"

    ```shell
    kubectl get deployments --namespace lws-system

    NAME                     READY   UP-TO-DATE   AVAILABLE   AGE
    lws-controller-manager   2/2     2            2           35s
    ```

## Install Gateway API Controller

Initialize OpenShift's provided Gateway API implementation:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: GatewayClass
metadata:
  name: openshift-default
spec:
  controllerName: "openshift.io/gateway-controller/v1"
```

Wait until the GatewayClass resource is accepted:

```shell
kubectl get gatewayclass openshift-default

NAME                CONTROLLER                           ACCEPTED   AGE
openshift-default   openshift.io/gateway-controller/v1   True       52s
```

Now install the Gateway API controller for your platform:

=== "Red Hat OpenShift AI"

    Install Red Hat Connectivity Link (RHCL) Operator from OpenShift's built-in OperatorHub.
    MaaS requires RHCL v1.2 or later:

    ```yaml
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
    ```

    Once the RHCL operator is ready, create a Connectivity Link instance:

    ```yaml
    apiVersion: kuadrant.io/v1beta1
    kind: Kuadrant
    metadata:
      name: kuadrant
      namespace: kuadrant-system
    ```

=== "Open Data Hub"

    Install Kuadrant using the OLM method. MaaS requires Kuadrant v1.3.1 or later.

    Create the `kuadrant-system` namespace:

    ```shell
    kubectl create namespace kuadrant-system
    ```

    Create an OperatorGroup:

    ```shell
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

    ```shell
    # Find latest Kuadrant operator version:
    GH_LATEST_KUADRANT_ENTRY_URL="https://api.github.com/repos/Kuadrant/kuadrant-operator/releases/latest"
    LATEST_KUADRANT_VERSION=$(curl -sSf ${GH_LATEST_KUADRANT_ENTRY_URL} | jq -r '.tag_name')

    # Install the CatalogSource
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

    ```shell
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

    Once the Kuadrant operator is ready, create a Kuadrant instance:

    ```shell
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
kubectl get deployments -n kuadrant-system

NAME                                    READY   UP-TO-DATE   AVAILABLE   AGE
authorino-operator                      1/1     1            1           80s
dns-operator-controller-manager         1/1     1            1           77s
kuadrant-console-plugin                 1/1     1            1           58s
kuadrant-operator-controller-manager    1/1     1            1           69s
limitador-operator-controller-manager   1/1     1            1           73s
```

For RHOAI installations, you should also see:

```shell
authorino                               1/1     1            1           81s
limitador-limitador                     1/1     1            1           82s
```

## Install Platform with Model Serving

First, set up the inference Gateway required by Model Serving:

```yaml
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: openshift-ai-inference
  namespace: openshift-ingress
spec:
  gatewayClassName: openshift-default
  listeners:
  - name: http
    port: 80
    protocol: HTTP
    allowedRoutes:
      namespaces:
        from: All
  infrastructure:
    labels:
      serving.kserve.io/gateway: kserve-ingress-gateway
```

!!! info "Gateway Architecture"
    MaaS uses a segregated gateway approach where models explicitly opt-in to MaaS capabilities. The `openshift-ai-inference` gateway above is for standard KServe inference, while `maas-default-gateway` (created later) enables token authentication and rate limiting. For details, see [Model Setup - Gateway Architecture](../configuration-and-management/model-setup.md#gateway-architecture).

Now install the platform operator for your environment:

=== "Red Hat OpenShift AI"

    Install Red Hat OpenShift AI (RHOAI) Operator from OpenShift's built-in OperatorHub.
    MaaS requires RHOAI v3.0 or later:

    ```yaml
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
    ```

    Once ready, the RHOAI Operator should automatically create a `DSCInitialization` resource.
    Install the Model Serving component by creating the following `DataScienceCluster` resource:

    ```yaml
    apiVersion: datasciencecluster.opendatahub.io/v2
    kind: DataScienceCluster
    metadata:
      name: default-dsc
    spec:
      components:
        # Components required for MaaS:
        kserve:
          managementState: Managed
          rawDeploymentServiceConfig: Headed
          # Enable Models-as-a-Service via operator
          modelsAsService:
            managementState: Managed

        # Components recommended for MaaS:
        dashboard:
          managementState: Managed
    ```

=== "Open Data Hub"

    Install the Open Data Hub Project (ODH) operator, which is available in OpenShift's
    preconfigured CatalogSource of community operators. MaaS requires ODH v3.0 or later:

    ```shell
    kubectl apply -f - <<EOF
    apiVersion: operators.coreos.com/v1alpha1
    kind: Subscription
    metadata:
      name: opendatahub-operator
      namespace: openshift-operators
    spec:
      channel: fast-3
      name: opendatahub-operator
      source: community-operators
      sourceNamespace: openshift-marketplace
    EOF
    ```

    Install the ODH Model Serving component by creating two resources:

    1. A `DSCInitialization` resource to initialize the ODH platform
    2. A `DataScienceCluster` resource to install ODH components

    ```shell
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
    ---
    apiVersion: datasciencecluster.opendatahub.io/v2
    kind: DataScienceCluster
    metadata:
      name: default-dsc
    spec:
      components:
        # Components required for MaaS:
        kserve:
          managementState: Managed
          rawDeploymentServiceConfig: Headed
          # Enable Models-as-a-Service via operator
          modelsAsService:
            managementState: Managed

        # Components recommended for MaaS:
        dashboard:
          managementState: Managed
    EOF
    ```

!!! note "MaaS via Operator"
    When `modelsAsService.managementState` is set to `Managed`, the operator will deploy
    the MaaS API, MaaS API AuthPolicy, and NetworkPolicy automatically. However, the **Gateway**,
    **Gateway AuthPolicy**, **TokenRateLimitPolicy**, and **RateLimitPolicy** must still be
    installed manually following the instructions below and in [maas-setup.md](maas-setup.md).

## Create MaaS Gateway

A Gateway with the name `maas-default-gateway` is **required** for MaaS to function. The configuration
below provides an example Gateway you can use:

!!! warning "Example Gateway Configuration"
    The Gateway configuration below is provided as an example. Depending on your cluster setup,
    you may need additional configuration such as TLS certificates, specific listener settings,
    or custom infrastructure labels. Consult your cluster administrator if you're unsure about
    Gateway requirements for your environment.

```shell
# Get your cluster's domain
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}')

kubectl apply -f - <<EOF
apiVersion: gateway.networking.k8s.io/v1
kind: Gateway
metadata:
  name: maas-default-gateway
  namespace: openshift-ingress
spec:
  gatewayClassName: openshift-default
  listeners:
   - name: http
     hostname: maas.${CLUSTER_DOMAIN}
     port: 80
     protocol: HTTP
     allowedRoutes:
       namespaces:
         from: All
EOF
```

Wait for the Gateway to be programmed:

```shell
kubectl wait --for=condition=Programmed gateway/maas-default-gateway -n openshift-ingress --timeout=60s
```

!!! note
    The `maas-default-gateway` created above satisfies the Gateway requirement for MaaS. When
    following the next steps, you can skip the Gateway creation and proceed directly to installing
    the Gateway AuthPolicy and usage policies in [maas-setup.md](maas-setup.md).

## Verification

Check that all MaaS components are running:

=== "Red Hat OpenShift AI"

    ```shell
    # Check RHOAI Model Serving deployments
    kubectl get deployments -n redhat-ods-applications

    NAME                        READY   UP-TO-DATE   AVAILABLE   AGE
    kserve-controller-manager   1/1     1            1           73s
    odh-model-controller        1/1     1            1           79s
    rhods-dashboard             2/2     2            2           78s
    maas-api                    1/1     1            1           60s  # Only if MaaS enabled
    ```

=== "Open Data Hub"

    ```shell
    # Check MaaS API deployment
    kubectl get deployment maas-api -n opendatahub

    # Check HTTPRoute
    kubectl get httproute maas-api-route -n opendatahub

    # Check AuthPolicy
    kubectl get authpolicy maas-api-auth-policy -n opendatahub

    # Check NetworkPolicy (allows Authorino to reach MaaS API)
    kubectl get networkpolicy maas-authorino-allow -n opendatahub
    ```

    All resources should exist and the MaaS API deployment should show `READY 1/1`.

## Test MaaS API Connectivity

Verify that Authorino can communicate with the MaaS API:

=== "Red Hat OpenShift AI"

    ```shell
    # Get Authorino pod
    AUTHORINO_POD=$(kubectl get pods -n kuadrant-system -l authorino-resource=authorino -o jsonpath='{.items[0].metadata.name}')

    # Test connectivity
    kubectl exec -n kuadrant-system $AUTHORINO_POD -- curl -s \
      http://maas-api.redhat-ods-applications.svc.cluster.local:8080/health
    ```

=== "Open Data Hub"

    ```shell
    # Get Authorino pod
    AUTHORINO_POD=$(kubectl get pods -n kuadrant-system -l authorino-resource=authorino -o jsonpath='{.items[0].metadata.name}')

    # Test connectivity
    kubectl exec -n kuadrant-system $AUTHORINO_POD -- curl -s \
      http://maas-api.opendatahub.svc.cluster.local:8080/health
    ```

Expected output:

```json
{"status":"healthy"}
```

For end-to-end validation and troubleshooting, see the [Validation Guide](validation.md).

## Next Steps

Once your platform with MaaS is installed:

1. [Install MaaS Components](maas-setup.md) - Install Gateway AuthPolicy and usage policies
2. [Deploy a Model](../configuration-and-management/model-setup.md) - Deploy your first LLMInferenceService
