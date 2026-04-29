# Validation Guide

This guide provides instructions for validating and testing your MaaS Platform deployment.

!!! note "Prerequisite"
    At least one model must be deployed to validate the installation. See [Model Setup (On Cluster)](model-setup.md) to deploy sample models.

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

!!! note "Optional"
    List MaaSSubscriptions you can access (authenticate with your OpenShift token; requires `HOST` from above):
    ```bash
    curl -sSk -H "Authorization: Bearer $(oc whoami -t)" \
      "${HOST}/maas-api/v1/subscriptions" | jq .
    ```

### 2. Get API Key

For OpenShift, create an API key (authenticate with your OpenShift token):

```bash
API_KEY_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{"name": "validation-key", "description": "Key for validation", "expiresIn": "1h", "subscription": "simulator-subscription"}' \
  "${HOST}/maas-api/v1/api-keys") && \
API_KEY=$(echo $API_KEY_RESPONSE | jq -r .key) && \
echo "API key obtained: ${API_KEY:0:20}..."
```

!!! note "Optional"
    List your API keys (metadata only; plaintext secrets are never returned):
    ```bash
    curl -sSk \
      -H "Authorization: Bearer $(oc whoami -t)" \
      -H "Content-Type: application/json" \
      -X POST \
      -d '{}' \
      "${HOST}/maas-api/v1/api-keys/search" | jq .
    ```

!!! warning "API key shown only once"
    The plaintext API key is returned **only at creation time**. We do not store the API key, so there is no way to retrieve it again. Store it securely when it is displayed. If you run into errors, see [Troubleshooting](troubleshooting.md).

!!! note
    `subscription` is the MaaSSubscription metadata name to bind (here `simulator-subscription` matches the [maas-system](https://github.com/opendatahub-io/models-as-a-service/tree/main/docs/samples/maas-system) free sample). Use your own name or omit the field to auto-select by `spec.priority`. For details, see [API Key Management](../user-guide/api-key-management.md).

### 3. List Available Models

Each API key is bound to one MaaSSubscription at creation time. `GET /v1/models` with an API key does not require `X-MaaS-Subscription`—the list is scoped to that subscription. (With an OpenShift user token instead of an API key, you can optionally send `X-MaaS-Subscription` to filter when you have access to multiple subscriptions.)

```bash
MODELS=$(curl -sSk ${HOST}/maas-api/v1/models \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $API_KEY" | jq -r .) && \
echo $MODELS | jq . && \
MODEL_NAME=$(echo $MODELS | jq -r '.data[0].id') && \
MODEL_URL=$(echo $MODELS | jq -r '.data[0].url') && \
echo "Model URL: $MODEL_URL"
```

### 4. Test Model Inference Endpoint

Send a request to the model’s OpenAI-compatible **chat completions** API (expect **200 OK**). This example uses **`POST /v1/chat/completions`** with a `messages` array. If your backend only implements **`/v1/completions`** (prompt-based) or another route, adjust the path and JSON body accordingly.

```bash
curl -sSk -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d "{\"model\": \"${MODEL_NAME}\", \"messages\": [{\"role\": \"user\", \"content\": \"Hello\"}], \"max_tokens\": 50}" \
  "${MODEL_URL}/v1/chat/completions" | jq
```

### 6. Test Authorization Enforcement

Send a request to the model endpoint without a token (should get a 401 Unauthorized response):

```bash
curl -sSk -H "Content-Type: application/json" \
  -d "{\"model\": \"${MODEL_NAME}\", \"messages\": [{\"role\": \"user\", \"content\": \"Hello\"}], \"max_tokens\": 50}" \
  "${MODEL_URL}/v1/chat/completions" -v
```

### 7. Test Rate Limiting

Send multiple requests to trigger rate limit (should get 200 OK followed by 429 Rate Limit Exceeded after about 4 requests):

```bash
for i in {1..16}; do
  curl -sSk -o /dev/null -w "%{http_code}\n" \
    -H "Authorization: Bearer $API_KEY" \
    -H "Content-Type: application/json" \
    -d "{\"model\": \"${MODEL_NAME}\", \"messages\": [{\"role\": \"user\", \"content\": \"Hello\"}], \"max_tokens\": 50}" \
    "${MODEL_URL}/v1/chat/completions"
done
```

See the deployment scripts documentation at `scripts/README.md` and the [Troubleshooting](troubleshooting.md) guide for more information.

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

For troubleshooting common issues, see [Troubleshooting](troubleshooting.md).
