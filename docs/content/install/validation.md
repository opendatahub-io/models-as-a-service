# Validation Guide

This guide provides instructions for validating and testing your MaaS Platform deployment.

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

## Automated Validation

For faster validation, you can use the automated validation script to run the manual validation steps more quickly:

```bash
./scripts/validate-deployment.sh
```

The script automates the manual validation steps above and provides detailed feedback with specific suggestions for fixing any issues found. This is useful when you need to quickly verify deployment status, but understanding the manual steps above helps with troubleshooting.

## Troubleshooting

### Common Issues

1. **Getting `501` Not Implemented errors**: Traffic is not making it to the Gateway.
      - [ ] Verify Gateway status and HTTPRoute configuration
2. **Getting `401` Unauthorized errors when trying to get a token**: Authentication maas-api is not working.
      - [ ] Verify `maas-api-auth-policy` and `maas-api-token-issuance-auth-policy` AuthPolicies are applied
      - [ ] Validate the AuthPolicy audiences are configured correctly:
        - `maas-api-auth-policy`: should accept both audiences (e.g., `["https://kubernetes.default.svc", "maas-default-gateway-sa"]`)
        - `maas-api-token-issuance-auth-policy`: should accept only OpenShift tokens (e.g., `["https://kubernetes.default.svc"]`)
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
