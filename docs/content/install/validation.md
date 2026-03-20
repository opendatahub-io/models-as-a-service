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

### 2. Get API Key

For OpenShift, create an API key (authenticate with your OpenShift token):

```bash
API_KEY_RESPONSE=$(curl -sSk \
  -H "Authorization: Bearer $(oc whoami -t)" \
  -H "Content-Type: application/json" \
  -X POST \
  -d '{"name": "validation-key", "description": "Key for validation", "expiresIn": "1h"}' \
  "${HOST}/maas-api/v1/api-keys") && \
API_KEY=$(echo $API_KEY_RESPONSE | jq -r .key) && \
echo "API key obtained: ${API_KEY:0:20}..."
```

!!! warning "API key shown only once"
    The plaintext API key is returned **only at creation time**. We do not store the API key, so there is no way to retrieve it again. Store it securely when it is displayed. If you run into errors, see [Troubleshooting](troubleshooting.md).

!!! note
    For more information about API keys, see [Understanding Token Management](../configuration-and-management/token-management.md).

### 3. List Available Models

Set the subscription name (required when your API key matches multiple subscriptions; use the name from your MaaSSubscription CR):

```bash
export MaaS_SUBSCRIPTION="simulator-subscription"  # or your subscription name
```

```bash
MODELS=$(curl -sSk ${HOST}/maas-api/v1/models \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer $API_KEY" \
    -H "X-MaaS-Subscription: ${MaaS_SUBSCRIPTION}" | jq -r .) && \
echo $MODELS | jq . && \
MODEL_NAME=$(echo $MODELS | jq -r '.data[0].id') && \
MODEL_URL=$(echo $MODELS | jq -r '.data[0].url') && \
echo "Model URL: $MODEL_URL"
```

### 4. Test Model Inference Endpoint

Send a request to the model endpoint (should get a 200 OK response):

```bash
curl -sSk -H "Authorization: Bearer $API_KEY" \
  -H "X-MaaS-Subscription: ${MaaS_SUBSCRIPTION}" \
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
    -H "Authorization: Bearer $API_KEY" \
    -H "X-MaaS-Subscription: ${MaaS_SUBSCRIPTION}" \
    -H "Content-Type: application/json" \
    -d "{\"model\": \"${MODEL_NAME}\", \"prompt\": \"Hello\", \"max_tokens\": 50}" \
    "${MODEL_URL}/v1/completions"
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
