# Troubleshooting

This guide helps you diagnose and resolve common issues with MaaS Platform deployments.

## Common Issues

1. **Getting `501` Not Implemented errors**: Traffic is not making it to the Gateway.
      - [ ] Verify Gateway status and HTTPRoute configuration
2. **Getting `401` Unauthorized errors when trying to create an API key**: Authentication to maas-api is not working.
      - [ ] Verify `maas-api-auth-policy` AuthPolicy is applied
      - [ ] Check if your cluster uses a custom token review audience:

      ```bash
      # Detect your cluster's audience
      AUD="$(kubectl create token default --duration=10m 2>/dev/null | \
        cut -d. -f2 | jq -Rr '@base64d | fromjson | .aud[0]' 2>/dev/null)"
      echo "Cluster audience: ${AUD}"
      ```

      If the audience is NOT `https://kubernetes.default.svc`, patch the AuthPolicy:

      ```bash
      kubectl patch authpolicy maas-api-auth-policy -n opendatahub \
        --type=merge --patch "
      spec:
        rules:
          authentication:
            openshift-identities:
              kubernetesTokenReview:
                audiences:
                  - ${AUD}
                  - maas-default-gateway-sa"
      ```

3. **Getting `401` errors when trying to get models**: Authentication is not working for the models endpoint.
      - [ ] Create a new API key and use it in the Authorization header
      - [ ] Verify `gateway-auth-policy` AuthPolicy is applied
      - [ ] Validate that the service account has `post` access to the `llminferenceservices` resource per MaaSAuthPolicy
        - Note: this should be automated by the ODH Controller
4. **Getting `404` errors when trying to get models**: The models endpoint is not working.
      - [ ] Verify `model-route` HTTPRoute exist and is applied
      - [ ] Verify the model is deployed and the `LLMInferenceService` has the `maas-default-gateway` gateway specified
      - [ ] Verify that the model is recognized by maas-api by checking the `maas-api/v1/models` endpoint (see [Validation Guide - List Available Models](validation.md#3-list-available-models))
5. **Rate limiting not working**: Verify AuthPolicy and TokenRateLimitPolicy are applied
      - [ ] Verify `gateway-rate-limits` RateLimitPolicy is applied
      - [ ] Verify TokenRateLimitPolicy is applied (e.g. gateway-default-deny or per-route policies)
      - [ ] If **multiple** TokenRateLimitPolicies target the **same** HTTPRoute, see [Quota and Access Configuration](../configuration-and-management/quota-and-access-configuration.md)
      - [ ] Verify the model is deployed and the `LLMInferenceService` has the `maas-default-gateway` gateway specified
      - [ ] Verify that the model is rate limited for request bursts (request-rate limiting) — see [Validation Guide - Test Rate Limiting](validation.md#7-test-rate-limiting)
      - [ ] Verify that the model returns 429 for token-heavy prompts (token-rate limiting) — see [Validation Guide - Test Rate Limiting](validation.md#7-test-rate-limiting)
6. **Routes not accessible (503 errors)**: Check MaaS Default Gateway status and HTTPRoute configuration
      - [ ] Verify Gateway is in `Programmed` state: `kubectl get gateway -n openshift-ingress maas-default-gateway`
      - [ ] Check HTTPRoute configuration and status

7. **Metrics not appearing in dashboards**: Prometheus is not scraping MaaS components.
      - [ ] Verify User Workload Monitoring is enabled — see [Observability Prerequisites](../advanced-administration/observability.md#user-workload-monitoring)
      - [ ] Verify Kuadrant observability is enabled — see [Observability Prerequisites](../advanced-administration/observability.md#kuadrant-observability)
      - [ ] Check prometheus-user-workload pods are running:

      ```bash
      kubectl get pods -n openshift-user-workload-monitoring
      ```

      - [ ] Verify ServiceMonitors/PodMonitors exist:

      ```bash
      kubectl get servicemonitor,podmonitor -A | grep -E "(maas|kuadrant|limitador)"
      ```

8. **Rate limiting metrics missing (authorized_calls, limited_calls)**: Kuadrant observability is not enabled.
      - [ ] Enable observability on Kuadrant CR:

      ```bash
      kubectl patch kuadrant kuadrant -n kuadrant-system --type=merge \
        -p '{"spec":{"observability":{"enable":true}}}'
      ```

      - [ ] Verify the PodMonitor was created:

      ```bash
      kubectl get podmonitor -n kuadrant-system
      ```

9. **RHOAI Dashboard Observability tab returns `503 Service Unavailable`**: The Dashboard cannot reach the Perses backend.

      The error typically appears as `{"statusCode": 503, "code": "FST_REPLY_FROM_SERVICE_UNAVAILABLE", ...}`.
      This is a Fastify/Dashboard-level error (not a gateway 503) indicating the monitoring stack
      is not deployed or Perses is not running. The most common causes are missing operators (COO,
      OpenTelemetry) or DSCI `monitoring.metrics` not being configured.

      See [RHOAI Dashboard Observability Tab](../advanced-administration/observability.md#rhoai-dashboard-observability-tab) for the full prerequisites and verification checklist.

10. **GenAI Studio tab not visible in Dashboard**: Requires `llamastackoperator` set to `Managed` in the DSC and the `genAiStudio` feature flag enabled on `OdhDashboardConfig`.

      See [OdhDashboardConfig Feature Flags](maas-setup.md#odhdashboardconfig-feature-flags) for setup.

11. **TLS certificate errors (`curl: (60) SSL certificate problem`)**: Your cluster uses self-signed or internal CA certificates that are not in your system trust store. See [TLS Certificate Validation](#tls-certificate-validation) below.

## Conflicting AuthPolicy Detection

MaaS automatically detects non-MaaS AuthPolicies (e.g., from KServe or other controllers) that target the same HTTPRoutes used by MaaS-governed models. When a conflict is detected, MaaS sets a `ConflictingAuthPolicy` condition on the affected MaaSAuthPolicy resource and emits a Kubernetes warning event.

### Symptoms

- MaaSAuthPolicy has condition `ConflictingAuthPolicy=True`
- Warning events on MaaSAuthPolicy resources referencing non-MaaS AuthPolicies
- Unexpected authentication behavior (wrong policy may win when multiple AuthPolicies target the same HTTPRoute)

### Diagnosis

Check for conflicting AuthPolicy conditions:

```bash
# Check MaaSAuthPolicy conditions
kubectl get maasauthpolicy -n models-as-a-service -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{range .status.conditions[?(@.type=="ConflictingAuthPolicy")]}{.status}{"\t"}{.message}{end}{"\n"}{end}'

# List all AuthPolicies targeting a specific model's HTTPRoute
MODEL_NS="<model-namespace>"
MODEL_NAME="<model-name>"
kubectl get authpolicy -n "$MODEL_NS" -o json | \
  jq -r ".items[] | select(.spec.targetRef.name == \"$MODEL_NAME\" and .spec.targetRef.kind == \"HTTPRoute\") | .metadata.name + \" (managed-by: \" + (.metadata.labels[\"app.kubernetes.io/managed-by\"] // \"unknown\") + \")\""

# Check for warning events
kubectl get events -n models-as-a-service --field-selector reason=ConflictingAuthPolicy
```

### Remediation

1. **KServe anonymous auth policies** (`*-kserve-route-authn`): These are typically created when `security.opendatahub.io/enable-auth` is misconfigured on the InferenceService. Set the annotation to `"true"` on MaaS-governed InferenceServices to prevent KServe from creating conflicting anonymous AuthPolicies:

    ```bash
    kubectl annotate inferenceservice <name> -n <namespace> \
      security.opendatahub.io/enable-auth="true" --overwrite
    ```

2. **Custom AuthPolicies**: If another team deployed a custom AuthPolicy targeting the same HTTPRoute, coordinate to determine which controller should own authentication for that route. Either:
      - Remove the conflicting AuthPolicy if MaaS is the intended auth authority
      - Or remove the model from the MaaSAuthPolicy if MaaS should not govern that route

3. **Verify resolution**: After applying the fix, the `ConflictingAuthPolicy` condition should transition to `False` on the next reconciliation:

    ```bash
    kubectl get maasauthpolicy -n models-as-a-service -o jsonpath='{range .items[*]}{.metadata.name}{": "}{range .status.conditions[?(@.type=="ConflictingAuthPolicy")]}{.status}{end}{"\n"}{end}'
    ```

## TLS Certificate Validation

By default, `curl` validates TLS certificates against your system CA bundle. If you encounter certificate verification errors (e.g., `curl: (60) SSL certificate problem: self-signed certificate`), use one of the approaches below.

### Recommended: Use the Ingress CA/Certificate Chain

For OpenShift clusters with self-signed or internal certificates, use the ingress trust chain (or your platform-provided CA bundle) with `curl --cacert`:

```bash
# Get ingress certificate chain/trust bundle (platform-specific source)
oc get secret -n openshift-ingress router-certs-default \
  -o jsonpath='{.data.tls\.crt}' | base64 -d > /tmp/ingress-cert-chain.crt

# Use --cacert in curl commands
curl -sS --cacert /tmp/ingress-cert-chain.crt \
  -H "Authorization: Bearer $API_KEY" \
  "https://maas.${CLUSTER_DOMAIN}/maas-api/v1/models" | jq .
```

Alternatively, if your cluster uses the OpenShift service CA:

```bash
# Extract the service CA bundle
oc get configmap -n openshift-config-managed service-ca-bundle \
  -o jsonpath='{.data.service-ca\.crt}' > /tmp/service-ca.crt

# Use --cacert in curl commands
curl -sS --cacert /tmp/service-ca.crt \
  -H "Authorization: Bearer $API_KEY" \
  "https://maas.${CLUSTER_DOMAIN}/maas-api/v1/models" | jq .
```

### Development/Testing Only: Disable Verification

!!! danger "Security Warning"
    The `-k` flag disables all TLS certificate validation. An attacker on the network path can present a forged certificate and intercept your API key, token, or other credentials. **Never use `-k` in production or when sending credentials over untrusted networks.**

For **isolated development or test environments only**, you can add the `-k` flag:

```bash
# INSECURE: Only for isolated dev/test environments
curl -sS -k -H "Authorization: Bearer $API_KEY" \
  "https://maas.${CLUSTER_DOMAIN}/maas-api/v1/models" | jq .
```

### Adding the CA to Your System Trust Store

For a permanent solution, add the ingress certificate chain to your system trust store so that all tools (curl, Python, browsers) trust it automatically:

```bash
# Linux (Fedora/RHEL)
sudo cp /tmp/ingress-cert-chain.crt /etc/pki/ca-trust/source/anchors/
sudo update-ca-trust

# Linux (Debian/Ubuntu)
sudo cp /tmp/ingress-cert-chain.crt /usr/local/share/ca-certificates/ingress-cert-chain.crt
sudo update-ca-certificates

# macOS
sudo security add-trusted-cert -d -r trustRoot \
  -k /Library/Keychains/System.keychain /tmp/ingress-cert-chain.crt
```

For detailed TLS configuration options, see [TLS Configuration](../configuration-and-management/tls-configuration.md).

## Additional Resources

- [Validation Guide](validation.md) — Manual validation steps
- [Observability Guide](../advanced-administration/observability.md) — Metrics, monitoring, and dashboards
- [scripts/README.md](https://github.com/opendatahub-io/models-as-a-service/blob/main/scripts/README.md) — Deployment scripts documentation
