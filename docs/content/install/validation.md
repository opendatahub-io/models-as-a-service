# Validation Guide

This guide provides instructions for validating and testing your MaaS Platform deployment.

## Namespace Reference

| Component | RHOAI | ODH |
|-----------|-------|-----|
| MaaS API | redhat-ods-applications | opendatahub |
| Kuadrant/RHCL | kuadrant-system | kuadrant-system |
| Gateway | openshift-ingress | openshift-ingress |

## Manual Validation (Recommended)

Follow these steps to validate your deployment and understand each component:

### 1. Get Gateway Endpoint

```bash
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}') && \
HOST="https://maas.${CLUSTER_DOMAIN}" && \
echo "Gateway endpoint: $HOST"
```

!!! note
    If you haven't created the `maas-default-gateway` yet, you can use the fallback:
    ```bash
    HOST="https://gateway.${CLUSTER_DOMAIN}" && \
    echo "Using fallback gateway endpoint: $HOST"
    ```

### 2. Get Authentication Token

For OpenShift:

```bash
TOKEN_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{"expiration": "10m"}' \
  "${HOST}/maas-api/v1/tokens") && \
TOKEN=$(echo $TOKEN_RESPONSE | jq -r .token) && \
echo "Token obtained: ${TOKEN:0:20}..."
```

!!! note
    For more information about how tokens work, see [Understanding Token Management](../configuration-and-management/token-management.md).

### 3. List Available Models

```bash
MODELS=$(curl -sSk ${HOST}/maas-api/v1/models \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $TOKEN" | jq -r .) && \
echo $MODELS | jq . && \
MODEL_NAME=$(echo $MODELS | jq -r '.data[0].id') && \
MODEL_URL=$(echo $MODELS | jq -r '.data[0].url') && \
echo "Model URL: $MODEL_URL"
```

### 4. Test Model Inference Endpoint

Send a request to the model endpoint (should get a 200 OK response):

```bash
curl -sSk -H "Authorization: Bearer $TOKEN" \
  -H "Content-Type: application/json" \
  -d "{\"model\": \"${MODEL_NAME}\", \"prompt\": \"Hello\", \"max_tokens\": 50}" \
  "${MODEL_URL}/v1/completions" | jq
```

### 5. Test Authorization Enforcement

Send a request to the model endpoint without a token (should get a 401 Unauthorized response):

```bash
curl -sSk -H "Content-Type: application/json" \
  -d "{\"model\": \"${MODEL_NAME}\", \"prompt\": \"Hello\", \"max_tokens\": 50}" \
  "${MODEL_URL}/v1/completions" -v
```

### 6. Test Rate Limiting

Send multiple requests to trigger rate limit (should get 200 OK followed by 429 Rate Limit Exceeded after about 4 requests):

```bash
for i in {1..16}; do
  curl -sSk -o /dev/null -w "%{http_code}\n" \
    -H "Authorization: Bearer $TOKEN" \
    -H "Content-Type: application/json" \
    -d "{\"model\": \"${MODEL_NAME}\", \"prompt\": \"Hello\", \"max_tokens\": 50}" \
    "${MODEL_URL}/v1/completions"
done
```

### 7. Verify Component Status

Check that all components are running:

```bash
kubectl get pods -n maas-api && \
kubectl get pods -n kuadrant-system && \
kubectl get pods -n kserve && \
kubectl get pods -n llm
```

Check Gateway status:

```bash
kubectl get gateway -n openshift-ingress maas-default-gateway
```

Check that policies are enforced:

```bash
kubectl get authpolicy -A && \
kubectl get tokenratelimitpolicy -A && \
kubectl get llminferenceservices -n llm
```

See the deployment scripts documentation at `scripts/README.md` for more information about validation and troubleshooting.

### 8. Verify Observability Metrics

!!! warning "Lazy metric population"
    Many metrics are **lazily registered** — they only appear after the first event triggers them. You **must generate traffic** (steps 4-6 above) before validating metrics. Dashboard panels will show "No Data" until traffic has been sent. This is normal Prometheus client behavior, not a configuration issue.

Check that observability resources are deployed:

```bash
# TelemetryPolicy (adds user/tier/model labels to Limitador metrics)
kubectl get telemetrypolicy -n openshift-ingress

# Istio Telemetry (adds tier label to gateway latency)
kubectl get telemetry latency-per-tier -n openshift-ingress

# ServiceMonitors (configure Prometheus scraping)
kubectl get servicemonitor -n kuadrant-system
kubectl get servicemonitor -n llm
```

Verify Limitador metrics are available (after sending traffic):

```bash
# Port-forward to Limitador metrics endpoint
kubectl port-forward -n kuadrant-system svc/limitador-limitador 8080:8080 &
sleep 2

# Check for usage metrics (require traffic to populate)
curl -s http://localhost:8080/metrics | grep -E '^(authorized_hits|authorized_calls|limited_calls)'

# Clean up
kill %1
```

Verify vLLM metrics from model pods (after sending inference requests):

```bash
# Check metrics directly from the model pod
MODEL_POD=$(kubectl get pods -n llm -l kserve.io/component=workload -o name | head -1)
kubectl exec -n llm "$MODEL_POD" -- curl -sk https://localhost:8000/metrics | \
  grep -E '^vllm:' | sed 's/{.*//' | sort -u
```

!!! note "curl availability"
    The `kubectl exec ... curl` command assumes `curl` is available in the model container.
    The simulator image includes `curl`, but production model images (e.g., stock vLLM) may not.
    As an alternative, use `kubectl port-forward` with a local `curl`:

    ```bash
    kubectl port-forward -n llm "$MODEL_POD" 8000:8000 &
    curl -sk https://localhost:8000/metrics | grep -E '^vllm:' | sed 's/{.*//' | sort -u
    kill %1
    ```

!!! tip "Expected vLLM metrics after traffic"
    After sending at least one inference request, you should see metrics including:
    `vllm:e2e_request_latency_seconds`, `vllm:time_to_first_token_seconds`,
    `vllm:inter_token_latency_seconds`, `vllm:request_prompt_tokens`,
    `vllm:request_generation_tokens`, `vllm:num_requests_running`,
    `vllm:kv_cache_usage_perc`, and `vllm:request_success_total`.
    If these are missing, ensure traffic has been sent and the ServiceMonitor
    `kserve-llm-models` exists in the `llm` namespace.

Verify metrics are being scraped into Prometheus:

```bash
# Check Prometheus targets (requires cluster-admin for platform Prometheus)
kubectl exec -n openshift-user-workload-monitoring prometheus-user-workload-0 \
  -c prometheus -- curl -s http://localhost:9090/api/v1/targets | \
  jq '.data.activeTargets[] | select(.labels.namespace=="kuadrant-system" or .labels.namespace=="llm") | {job: .labels.job, health: .health}'
```

For detailed observability configuration and metric reference, see [Observability](../advanced-administration/observability.md).

## Automated Validation

For faster validation, you can use the automated validation script to run the manual validation steps more quickly:

```bash
./scripts/validate-deployment.sh
```

The script automates the manual validation steps above and provides detailed feedback with specific suggestions for fixing any issues found. This is useful when you need to quickly verify deployment status, but understanding the manual steps above helps with troubleshooting.

## TLS Verification

TLS is enabled by default when deploying via the automated script or ODH overlay.

### Check Certificate

```bash
# View certificate details (RHOAI)
kubectl get secret maas-api-serving-cert -n redhat-ods-applications \
  -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -text -noout

# Check expiry
kubectl get secret maas-api-serving-cert -n redhat-ods-applications \
  -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -enddate -noout
```

### Test HTTPS Endpoint

```bash
kubectl run curl --rm -it --image=curlimages/curl -- \
  curl -vk https://maas-api.redhat-ods-applications.svc:8443/health
```

For detailed TLS configuration options, see [TLS Configuration](../configuration-and-management/tls-configuration.md).

## Troubleshooting

### Common Issues

1. **Getting `501` Not Implemented errors**: Traffic is not making it to the Gateway.
      - [ ] Verify Gateway status and HTTPRoute configuration
2. **Getting `401` Unauthorized errors when trying to get a token**: Authentication maas-api is not working.
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
      # For RHOAI:
      kubectl patch authpolicy maas-api-auth-policy -n redhat-ods-applications \
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

      For ODH, use namespace `opendatahub` instead of `redhat-ods-applications`.
3. **Getting `401` errors when trying to get models**: Authentication is not working for the models endpoint.
      - [ ] Create a new token (default expiration is 10 minutes)
      - [ ] Verify `gateway-auth-policy` AuthPolicy is applied
      - [ ] Validate that `system:serviceaccounts:maas-default-gateway-tier-{TIER}` has `post` access to the `llminferenceservices` resource
        - Note: this should be automated by the ODH Controller
4. **Getting `404` errors when trying to get models**: The models endpoint is not working.
      - [ ] Verify `model-route` HTTPRoute exist and is applied
      - [ ] Verify the model is deployed and the `LLMInferenceService` has the `maas-default-gateway` gateway specified
      - [ ] Verify that the model is recognized by maas-api by checking the `maas-api/v1/models` endpoint (see [List Available Models](#3-list-available-models))
5. **Rate limiting not working**: Verify AuthPolicy and TokenRateLimitPolicy are applied
      - [ ] Verify `gateway-rate-limits` RateLimitPolicy is applied
      - [ ] Verify `gateway-token-rate-limits` TokenRateLimitPolicy is applied
      - [ ] Verify the model is deployed and the `LLMInferenceService` has the `maas-default-gateway` gateway specified
      - [ ] Verify that the model is rate limited by checking the inference endpoint (see [Test Rate Limiting](#6-test-rate-limiting))
      - [ ] Verify that the model is token rate limited by checking the inference endpoint (see [Test Rate Limiting](#6-test-rate-limiting))
6. **Routes not accessible (503 errors)**: Check MaaS Default Gateway status and HTTPRoute configuration
      - [ ] Verify Gateway is in `Programmed` state: `kubectl get gateway -n openshift-ingress maas-default-gateway`
      - [ ] Check HTTPRoute configuration and status
7. **Metrics missing or dashboards show "No Data"**: Most metrics are lazily populated
      - [ ] **Send traffic first** — metrics like `authorized_hits`, `authorized_calls`, vLLM histograms, and Authorino auth metrics only appear after the first request triggers them (see [Verify Observability Metrics](#8-verify-observability-metrics))
      - [ ] Verify ServiceMonitors exist: `kubectl get servicemonitor -n kuadrant-system` and `kubectl get servicemonitor -n llm`
      - [ ] Check Prometheus targets are healthy: port-forward to Prometheus and check `/api/v1/targets`
      - [ ] For Authorino auth metrics, verify the `authorino-server-metrics` ServiceMonitor is deployed (run `./scripts/install-observability.sh` if missing)
