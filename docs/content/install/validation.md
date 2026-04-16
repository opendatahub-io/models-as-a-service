# Validation Guide

After deploying MaaS, you need to verify three things:

1. That the infrastructure is healthy: pods are running, the gateway is accepting traffic, and auth/rate-limit policies are enforced.
2. That you can deploy a model and talk to it through the gateway: create an API key, list available models, and send an inference request.
3. That security is active: requests without credentials are rejected and rate limits kick in after a few calls.

!!! note "Prerequisite"
    At least one model must be deployed to validate the installation. See [Model Setup (On Cluster)](model-setup.md) to deploy sample models.

## Step-by-Step Validation

Use these steps to test individual components, debug specific failures, or understand how the system works.

!!! info "OC token vs API key"
    Two types of credentials work with the MaaS gateway. Use your OpenShift token (`oc whoami -t`) for admin operations like creating API keys. Use an API key (`sk-oai-...`) for model operations like inference and listing models. The steps below use both.

### 1. Get Gateway Endpoint

```bash
CLUSTER_DOMAIN=$(kubectl get ingresses.config.openshift.io cluster -o jsonpath='{.spec.domain}') && \
HOST="https://maas.${CLUSTER_DOMAIN}" && \
echo "Gateway endpoint: $HOST"
```

Extract the cluster CA certificate so that `curl` can verify TLS without the `-k` flag:

```bash
CA_CERT="/tmp/cluster-ca.crt"
oc get configmap kube-root-ca.crt -n openshift-config \
  -o jsonpath='{.data.ca-bundle\.crt}' > "$CA_CERT"
echo "CA certificate saved to $CA_CERT"
```

??? success "Expected output"
    ```text
    Gateway endpoint: https://maas.apps.your-cluster.example.com
    CA certificate saved to /tmp/cluster-ca.crt
    ```

!!! note
    If you don't have cluster-reader permissions, set the gateway URL directly:
    ```bash
    HOST="https://maas.apps.your-cluster.example.com"
    ```

!!! tip "If CA extraction fails"
    If you cannot extract the CA certificate (e.g., missing permissions or non-OpenShift cluster), you can fall back to `curl -k` to skip TLS verification. This is acceptable for one-off debugging but should not be used in automation or shared environments, as it disables certificate validation.

**If this fails:**

- Empty `CLUSTER_DOMAIN`: you don't have permission to read the ingress config. Set `HOST` manually as shown above.

### 2. Get API Key

Create an API key using your OpenShift token:

```bash
API_KEY_RESPONSE=$(curl -sS --cacert "$CA_CERT" \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{"name": "validation-key", "description": "Key for validation", "expiresIn": "1h", "subscription": "simulator-subscription"}' \
  "${HOST}/maas-api/v1/api-keys")
API_KEY=$(echo "$API_KEY_RESPONSE" | jq -r '.key')
echo "API key obtained successfully."
```

??? success "Expected output"
    ```text
    API key obtained successfully.
    ```

!!! tip "If jq fails"
    If you see a jq parse error, the curl request likely returned a non-JSON error. Run `echo "$API_KEY_RESPONSE"` to inspect the raw response.

!!! warning "API key shown only once"
    The plaintext API key is returned **only at creation time**. We do not store the API key, so there is no way to retrieve it again. Store it securely when it is displayed. If you run into errors, see [Troubleshooting](troubleshooting.md).

!!! note
    `subscription` is the MaaSSubscription metadata name to bind (here `simulator-subscription` matches the [maas-system](https://github.com/opendatahub-io/models-as-a-service/tree/main/docs/samples/maas-system) free sample). Use your own name or omit the field to auto-select by `spec.priority`. For details, see [Understanding Token Management](../configuration-and-management/token-management.md).

**If this fails:**

- **401 Unauthorized**: your OC token is expired or invalid. Run `oc login` again.
- **400 "invalid subscription"**: no MaaSSubscription exists with that name. Deploy a model with MaaS CRs first (see [Model Setup](model-setup.md)), or omit the `subscription` field to auto-select.
- **503 Service Unavailable**: the gateway can't reach the MaaS API backend. Check that the `maas-api` pod is running: `kubectl get pods -l app.kubernetes.io/name=maas-api -A`

### 3. List Available Models

Each API key is bound to one MaaSSubscription at creation time. `GET /v1/models` with an API key returns only models from that subscription. With an OpenShift token instead of an API key, you can send `X-MaaS-Subscription` to filter when you have access to multiple subscriptions.

```bash
MODELS=$(curl -sS --cacert "$CA_CERT" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $API_KEY" \
    "${HOST}/maas-api/v1/models")
echo "$MODELS" | jq .
MODEL_NAME=$(echo "$MODELS" | jq -r '.data[0].id')
MODEL_URL=$(echo "$MODELS" | jq -r '.data[0].url')
echo "Model URL: $MODEL_URL"
```

??? success "Expected output"
    ```json
    {
      "data": [
        {
          "id": "facebook/opt-125m",
          "url": "https://maas.apps.your-cluster.example.com/llm/facebook-opt-125m-simulated",
          "object": "model"
        }
      ],
      "object": "list"
    }
    ```
    ```text
    Model URL: https://maas.apps.your-cluster.example.com/llm/facebook-opt-125m-simulated
    ```

**If this fails:**

- **403 Forbidden**: the API key is invalid or the AuthPolicy is not enforced. Create a new API key and try again.
- **Empty list** (`{"data":[], "object":"list"}`): a model is deployed but the MaaSModelRef is missing or not in Ready state. Check: `kubectl get maasmodelref -A`
- **401 Unauthorized**: the API key format is wrong or expired. Make sure you're using the `sk-oai-...` key from step 2.

### 4. Test Model Inference

Send a chat completion request to the model. You should get a 200 OK response with generated text:

```bash
RESPONSE=$(curl -sS --cacert "$CA_CERT" \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"model\": \"${MODEL_NAME}\", \"messages\": [{\"role\": \"user\", \"content\": \"Hello\"}], \"max_tokens\": 50}" \
  "${MODEL_URL}/v1/chat/completions")
echo "$RESPONSE" | jq .
```

??? success "Expected output"
    ```json
    {
      "id": "chatcmpl-abc123",
      "created": 1776000000,
      "model": "facebook/opt-125m",
      "usage": {
        "prompt_tokens": 1,
        "completion_tokens": 25,
        "total_tokens": 26
      },
      "object": "chat.completion",
      "choices": [
        {
          "index": 0,
          "message": {
            "role": "assistant",
            "content": "Hello! How can I help you today?"
          },
          "finish_reason": "stop"
        }
      ]
    }
    ```

!!! note
    Some models only support `/v1/completions` (prompt-based) instead of `/v1/chat/completions`. If you get a 404 or 400, try the completions endpoint:
    ```bash
    RESPONSE=$(curl -sS --cacert "$CA_CERT" \
      -H "Authorization: Bearer $API_KEY" \
      -H "Content-Type: application/json" \
      -d "{\"model\": \"${MODEL_NAME}\", \"prompt\": \"Hello\", \"max_tokens\": 50}" \
      "${MODEL_URL}/v1/completions")
    echo "$RESPONSE" | jq .
    ```

**If this fails:**

- **401 Unauthorized**: the API key is not recognized. It may be expired or in the wrong format. Create a new one.
- **403 Forbidden**: Authorino can't validate the API key against the MaaS API. This usually means a TLS configuration issue between Authorino and maas-api. Check the Authorino logs: `kubectl logs deployment/authorino -n kuadrant-system`
- **503 Service Unavailable**: the gateway can't reach the model backend. Check that the model pod is running: `kubectl get pods -n llm`
- **404 Not Found**: the model URL is wrong or the HTTPRoute is not configured. Check: `kubectl get httproute -A`

### 5. Test Authorization Enforcement

Send a request without any credentials. You should get a 401 Unauthorized response:

```bash
curl -sS --cacert "$CA_CERT" -o /dev/null -w "HTTP status: %{http_code}\n" \
  -H "Content-Type: application/json" \
  -d "{\"model\": \"${MODEL_NAME}\", \"messages\": [{\"role\": \"user\", \"content\": \"Hello\"}], \"max_tokens\": 50}" \
  "${MODEL_URL}/v1/chat/completions"
```

??? success "Expected output"
    ```text
    HTTP status: 401
    ```

**If this fails:**

- **200 OK instead of 401**: the AuthPolicy is not enforced on this route. Check: `kubectl get authpolicy -A` and verify the ENFORCED column is True.

### 6. Test Rate Limiting

Send multiple requests to trigger the rate limit. After a few successful requests, you should start getting 429 responses:

```bash
for i in {1..16}; do
  curl -sS --cacert "$CA_CERT" -o /dev/null -w "%{http_code} " \
    -H "Authorization: Bearer $API_KEY" \
    -H "Content-Type: application/json" \
    -d "{\"model\": \"${MODEL_NAME}\", \"messages\": [{\"role\": \"user\", \"content\": \"Hello\"}], \"max_tokens\": 50}" \
    "${MODEL_URL}/v1/chat/completions"
done
echo ""
```

??? success "Expected output"
    ```text
    200 200 200 429 429 429 429 429 429 429 429 429 429 429 429 429
    ```
    The exact number of 200s before rate limiting depends on your TokenRateLimitPolicy configuration. With the default simulator subscription (100 tokens/min), you should see 429s after 3-5 requests.

**If this fails:**

- **No 429s after 16 requests**: the TokenRateLimitPolicy is missing or not enforced. Check: `kubectl get tokenratelimitpolicy -A`. If multiple TokenRateLimitPolicies target the same HTTPRoute, see [Subscription limitations](../configuration-and-management/subscription-known-issues.md#token-rate-limits-when-multiple-model-references-share-one-httproute).

For more troubleshooting, see [Troubleshooting](troubleshooting.md).

## Automated Validation

Run the validation script to check all components in one pass:

```bash
./scripts/validate-deployment.sh
```

To validate a specific model:

```bash
./scripts/validate-deployment.sh <model-name>
```

!!! note "Non-admin users"
    The script reads the cluster ingress config to find the gateway URL. If you don't have cluster-reader permissions, set the URL manually:
    ```bash
    export MAAS_GATEWAY_HOST="https://maas.apps.your-cluster.example.com"
    ./scripts/validate-deployment.sh
    ```

!!! note "OpenShift required"
    The validation script requires an OpenShift cluster. For non-OpenShift environments, use the step-by-step validation above.

### What the script checks

The script runs four groups of checks:

1. **Component status** -- MaaS API pods, policy engine pods, RHOAI/KServe pods, and deployed models
2. **Gateway status** -- gateway is accepted and programmed, HTTPRoute is configured, hostname is reachable
3. **Policy status** -- AuthPolicy is enforced, TokenRateLimitPolicy is configured
4. **API endpoint tests** -- authentication token works, API key creation works, models endpoint responds, model inference returns a result, rate limiting kicks in, unauthorized requests are rejected

### Reading the output

Each check shows PASS, FAIL, or WARNING. At the end, the script prints a summary:

```text
Validation Summary

Results:
  Passed: 15
  Failed: 0
  Warnings: 0

All critical checks passed!
```

When a check fails, the script prints the reason and a suggestion. For example:

```text
FAIL: Models endpoint failed (HTTP 403)
  Reason: Response empty
  Suggestion: Check MaaS API service and logs
```

For the full list of script options and flags, see [scripts/README.md](https://github.com/opendatahub-io/models-as-a-service/blob/main/scripts/README.md#validate-deploymentsh).

### If checks fail

Use the step-by-step validation above to isolate which component is broken. For common error patterns, see [Troubleshooting](troubleshooting.md).

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

For troubleshooting common issues, see [Troubleshooting](troubleshooting.md).
