# MaaS Kind Deployment Testing Scripts

Collection of test scripts for validating the Models as a Service (MaaS) platform functionality in Kind-based local development environment.

## Prerequisites

- MaaS platform running on Kind cluster
- `kubectl` access to the cluster
- `jq` installed for JSON processing
- Port forwarding to `localhost:80` active

## Test Scripts

### 1. Simple Models Test (`test-models-simple.sh`) ⭐ **NEW**

Quick verification that both models are working correctly.

```bash
./deployment/scripts/kind/tests/test-models-simple.sh
```

**What it tests:**
- Creates premium user token
- Sends test messages to both model-a and model-b
- Displays responses with token usage statistics  
- Tests model discovery endpoint

**Expected output:**
- ✅ Both models respond with generated content
- ✅ Token usage statistics displayed
- ✅ Model discovery lists both models as ready

---

### 2. Authentication & Authorization Test (`test-auth-tiers.sh`) ⭐ **NEW**

Comprehensive validation of tier-based access control.

```bash
./deployment/scripts/kind/tests/test-auth-tiers.sh
```

**What it tests:**
- **Free User Access**: Can access model-a (free), blocked from model-b (premium)
- **Premium User Access**: Can access all models (model-a and model-b)
- **Model Discovery**: Both user types can list available models

**Expected output:**
- ✅ Free users blocked from premium models (401/403)
- ✅ Premium users can access all models (200)  
- ✅ Proper HTTP status codes for each scenario

---

### 3. List Models Test (`test-list-models.sh`)

Tests the models discovery endpoint with premium user.

```bash
./deployment/scripts/kind/tests/test-list-models.sh
```

**What it tests:**
- Token creation and authentication
- Models endpoint accessibility 
- JSON response formatting
- Available models and their permissions

**Expected output:**
- List of available models (model-a, model-b)
- Model metadata and access permissions
- Properly formatted JSON response

---

### 4. Rate Limiting Test (`test-rate-limits.sh`)

Tests request-based rate limiting (5 requests per 2 minutes for free tier).

```bash
./deployment/scripts/kind/tests/test-rate-limits.sh
```

**What it tests:**
- Rate limit policy enforcement
- Request counting accuracy
- 429 responses when limit exceeded
- Rate limit reset behavior

**Test sequence:**
1. Prompts to wait 130 seconds for clean slate (optional, default: no wait)
2. Sends 8 requests rapidly
3. Expects ~5 success + ~3 rate limited

**Expected behavior:**
- First 5 requests: HTTP 200 (success)
- Remaining requests: HTTP 429 (rate limited)

---

### 5. Token Rate Limiting Test (`test-token-rate-limits.sh`)

Tests token-based rate limiting (100 tokens per minute for free tier).

```bash
./deployment/scripts/kind/tests/test-token-rate-limits.sh
```

**What it tests:**
- Token counting accuracy
- Token limit enforcement
- Progressive token accumulation
- Token rate limit reset behavior

**Test sequence:**
1. Prompts to wait 70 seconds for clean slate (optional, default: no wait)
2. Sends requests with progressively longer prompts
3. Monitors token usage accumulation
4. Expects rate limiting around 100 tokens

**Expected behavior:**
- Initial requests: HTTP 200 with token counts
- Later requests: HTTP 429 when ~100 tokens exceeded

---

## Current Rate Limit Policies

### Free Tier Limits
- **Requests**: 5 per 2 minutes
- **Tokens**: 100 per minute
- **User**: `free-user` service account

### Premium Tier Limits  
- **Requests**: 20 per 2 minutes
- **Tokens**: 50,000 per minute

### Enterprise Tier Limits
- **Requests**: 50 per 2 minutes  
- **Tokens**: 100,000 per minute

## Recommended Test Order

For comprehensive testing, run scripts in this order:

1. **Start with basic functionality**:
   ```bash
   ./test-models-simple.sh      # Quick model verification
   ```

2. **Test security and access control**:
   ```bash
   ./test-auth-tiers.sh         # Authentication & authorization
   ./test-list-models.sh        # Model discovery
   ```

3. **Test rate limiting** (optional, has delays):
   ```bash
   ./test-rate-limits.sh        # Request rate limits
   ./test-token-rate-limits.sh  # Token rate limits
   ```

## Usage Tips

1. **No Wait Required**: New scripts (test-models-simple.sh and test-auth-tiers.sh) run immediately
2. **Interactive Rate Limit Tests**: Rate limit tests now prompt before waiting (default: no wait for first-time runs)
3. **Multiple Runs**: Scripts can be run multiple times with appropriate gaps
4. **Check Policies**: If results are unexpected, verify current rate limit policies:
   ```bash
   kubectl describe ratelimitpolicy gateway-rate-limits -n istio-system
   kubectl describe tokenratelimitpolicy gateway-token-rate-limits -n istio-system
   ```

5. **Monitor Logs**: Check Limitador logs if needed:
   ```bash
   kubectl logs -n kuadrant-system -l app=limitador --tail=20
   ```

## Troubleshooting

- **Authentication failures**: Ensure MaaS API and gateway are running
- **Connection refused**: Check port forwarding to localhost:80
- **Unexpected responses**: Verify model simulators are deployed and ready
- **Policy mismatches**: Confirm current rate limit policies match expectations

## Test Environment

These scripts are designed for the Kind-based local development environment with:
- Fast inference simulators (not real LLM models)
- Standard MaaS rate limiting policies
- Free tier testing by default