# Testing Guide for Kind Local Development

This guide explains how to test the MaaS platform running on Kind.

## Test Scripts

### 1. Automated Test Suite (`validationtest.sh`)

Comprehensive automated testing of all flows - authentication, rate limiting, and inference.

```bash
cd deployment/scripts/kind
./validationtest.sh
```

**Tests included:**

| # | Test Name | What it Checks |
|---|-----------|----------------|
| 1 | Basic Connectivity | Gateway is responding |
| 2 | Authentication | Unauthenticated requests rejected (401) |
| 3 | Authentication | Authenticated requests accepted (200) |
| 4 | Request Rate Limiting | Free tier limit enforced (5 req/2min → 429) |
| 5 | Token Rate Limiting | Free tier token limit enforced (100 tokens/min → 429) |
| 6 | LLM Inference | Premium user can make inference requests |

**Expected output:**
```
================================================
Test Summary
================================================
Tests Run:    6
Tests Passed: 6
Tests Failed: 0

✅ All tests passed!
```

### 2. Interactive Demo (`demo.sh`)

Menu-driven interactive demonstrations of different features.

```bash
cd deployment/scripts/kind
./demo.sh
```

**Menu options:**
1. **Quick Start** - Basic connectivity and inference
2. **Authentication & Authorization** - 3-tier access control (Free/Premium/Enterprise)
3. **Rate Limiting** - Request and token-based limits
4. **Full Demo** - All features walkthrough

**What it demonstrates:**
- Gateway accessibility via localhost:80
- Service account token authentication
- RBAC-based authorization
- Request rate limiting (5 req/2min for Free tier)
- Token rate limiting (100 tokens/min for Free tier)
- LLM inference with simulator models
- OpenAI API compatibility

## Manual Testing

### Test 1: Basic LLM Inference

First, create a test user token:
```bash
kubectl create sa demo-user -n maas-api
kubectl create clusterrole llm-model-access --verb=get,list,create --resource=llminferenceservices
kubectl create clusterrolebinding demo-user-llm-access --clusterrole=llm-model-access --serviceaccount=maas-api:demo-user
TOKEN=$(kubectl create token demo-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)
```

Then make a request:
```bash
curl -X POST http://localhost/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "llm-katan",
    "messages": [
      {"role": "user", "content": "Write a haiku about Kubernetes"}
    ],
    "max_tokens": 50
  }'
```

**Expected:** HTTP 200 with JSON response containing AI-generated haiku.

### Test 2: Authentication Failure

```bash
curl -X POST http://localhost/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "llm-katan",
    "messages": [{"role": "user", "content": "Hello"}],
    "max_tokens": 10
  }'
```

**Expected:** HTTP 401 Unauthorized (blocked by Authorino).

### Test 3: Rate Limiting

Create a free-tier user and send multiple requests:

```bash
kubectl create sa free-user -n maas-api
kubectl create clusterrolebinding free-user-llm-access --clusterrole=llm-model-access --serviceaccount=maas-api:free-user
FREE_TOKEN=$(kubectl create token free-user -n maas-api --duration=1h --audience=maas-default-gateway-sa)

# Send 7 requests (free tier limit is 5 per 2 minutes)
for i in {1..7}; do
  echo "Request $i:"
  curl -s -w "HTTP: %{http_code}\n" \
    -X POST http://localhost/v1/chat/completions \
    -H "Authorization: Bearer $FREE_TOKEN" \
    -H 'Content-Type: application/json' \
    -d '{"model":"llm-katan","messages":[{"role":"user","content":"Hi"}],"max_tokens":5}'
  echo "---"
  sleep 0.2
done
```

**Expected:** First 4-5 requests succeed (200), later requests get rate limited (429).

### Test 4: Token-Based Rate Limiting

```bash
# Request 150 tokens (free tier limit is 100 tokens per minute)
curl -s -w "\nHTTP: %{http_code}\n" \
  -X POST http://localhost/v1/chat/completions \
  -H "Authorization: Bearer $FREE_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "llm-katan",
    "messages": [{"role": "user", "content": "Write a long response"}],
    "max_tokens": 150
  }'
```

**Expected:** HTTP 429 Too Many Requests (token limit exceeded).

### Test 5: Multi-turn Conversation

```bash
curl -X POST http://localhost/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{
    "model": "llm-katan",
    "messages": [
      {"role": "user", "content": "What is 2+2?"},
      {"role": "assistant", "content": "2+2 equals 4."},
      {"role": "user", "content": "What about 3+3?"}
    ],
    "max_tokens": 30
  }'
```

## Testing Flows

### Flow 1: Complete Request Path

**What to test:** End-to-end request from localhost to AI response.

**Steps:**
1. Send request to `http://localhost/v1/chat/completions`
2. Verify Istio Gateway receives it
3. Verify Authorino validates token
4. Verify Limitador checks rate limits
5. Verify llm-katan processes request
6. Verify AI response is returned

**How to test:**
```bash
# Run the quick demo
cd deployment/scripts/kind
./demo.sh
# Select option 1 (Quick Start)

# Or watch logs in real-time
kubectl logs -n llm deployment/llm-katan -f &
curl -X POST http://localhost/v1/chat/completions \
  -H "Authorization: Bearer $TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"model":"llm-katan","messages":[{"role":"user","content":"Hello"}],"max_tokens":10}'
```

### Flow 2: Policy Enforcement

**What to test:** Policies correctly block/allow traffic.

**Steps:**
1. Test without token → Should be blocked
2. Test with valid token → Should succeed
3. Test rapid requests → Should hit rate limit
4. Test token limit → Should hit token limit

**How to test:**
```bash
cd deployment/scripts/kind

# Run test suite
./validationtest.sh

# Or run interactive demo
./demo.sh
# Select option 2 (Authentication & Authorization)
# Select option 3 (Rate Limiting)

# Check policy status
kubectl get authpolicies,ratelimitpolicies -n istio-system
```

### Flow 3: Gateway Routing

**What to test:** Multiple paths route to correct services.

**Steps:**
1. Request to `/v1/*` → Should route to llm-katan
2. Request to `/maas-api/*` → Should route to maas-api (if deployed)

**How to test:**
```bash
# Test LLM route
curl -I -H "Authorization: Bearer $TOKEN" http://localhost/v1/models

# Check routes
kubectl get httproutes -A
```

## Debugging Failed Tests

### Test fails: Gateway not responding

**Check:**
```bash
kubectl get pods -n istio-system
kubectl get gateway maas-gateway -n istio-system
docker ps | grep maas-local
```

**Fix:**
- Ensure Docker Desktop is running
- Check port 80 is not in use: `lsof -ti:80`
- Restart Kind cluster: `./deployment/scripts/kind/setup-kind.sh`

### Test fails: Authentication not working

**Check:**
```bash
kubectl get pods -n kuadrant-system | grep authorino
kubectl logs -n kuadrant-system -l app.kubernetes.io/name=authorino --tail=50
kubectl get authpolicy gateway-auth-policy -n istio-system -o yaml
```

**Fix:**
- Wait for Authorino to be ready: `kubectl wait --for=condition=ready pod -l app.kubernetes.io/name=authorino -n kuadrant-system`
- Check AuthPolicy is enforced: `kubectl get authpolicy -n istio-system -o jsonpath='{.items[0].status.conditions[?(@.type=="Enforced")].status}'` (should be "True")
- Ensure token has correct audience: `--audience=maas-default-gateway-sa`

### Test fails: LLM not responding

**Check:**
```bash
kubectl get pods -n llm
kubectl logs -n llm deployment/llm-katan --tail=100
kubectl describe pod -n llm -l app=llm-katan
```

**Fix:**
- llm-katan may be downloading model (wait ~60 seconds)
- Check resources: Increase Docker Desktop memory to 8GB
- Check logs for errors

### Test fails: Rate limiting not working

**Check:**
```bash
kubectl get pods -n kuadrant-system | grep limitador
kubectl logs -n kuadrant-system -l app=limitador --tail=50
kubectl get ratelimitpolicies -n istio-system
kubectl get kuadrant -n kuadrant-system
```

**Fix:**
- Ensure Kuadrant instance exists: `kubectl get kuadrant -n kuadrant-system`
- Ensure Limitador is running: `kubectl get pods -n kuadrant-system | grep limitador`
- Check RateLimitPolicy is enforced: `kubectl get ratelimitpolicy -n istio-system -o jsonpath='{.items[0].status.conditions[?(@.type=="Enforced")].status}'` (should be "True")

## Monitoring During Tests

### Watch pod status
```bash
watch kubectl get pods -A
```

### Watch logs in multiple terminals
```bash
# Terminal 1: llm-katan
kubectl logs -n llm deployment/llm-katan -f

# Terminal 2: Authorino
kubectl logs -n kuadrant-system -l app.kubernetes.io/name=authorino -f

# Terminal 3: Limitador
kubectl logs -n kuadrant-system -l app=limitador -f

# Terminal 4: Run tests
cd deployment/scripts/kind
./validationtest.sh
```

### Monitor resource usage
```bash
kubectl top pods -A
kubectl top nodes
```

## CI/CD Integration

To use these tests in CI/CD:

```bash
# In your CI pipeline
cd deployment/scripts/kind
./install.sh  # Includes test models by default
./validationtest.sh

# Exit code 0 = all tests passed
# Exit code 1 = one or more tests failed
```

## Test Coverage

Current test coverage:

- ✅ Gateway connectivity
- ✅ Gateway routing to llm-katan
- ✅ Kubernetes resource health
- ✅ Authentication (Authorino + K8s ServiceAccount tokens)
- ✅ Authorization (RBAC via SubjectAccessReview)
- ✅ Request-based rate limiting (Limitador)
- ✅ Token-based rate limiting (Limitador)
- ✅ LLM inference (llm-katan with vLLM)
- ✅ OpenAI API compatibility
- ✅ Error handling (401, 403, 429)
- ✅ 3-tier access control (Free/Premium/Enterprise)

## Tier Configuration

The Kind deployment includes 3-tier access control:

| Tier | Request Limit | Token Limit | Use Case |
|------|--------------|-------------|----------|
| Free | 5 req/2min | 100 tokens/min | Testing, personal projects |
| Premium | 20 req/2min | 50,000 tokens/min | Small teams, development |
| Enterprise | 50 req/2min | 100,000 tokens/min | Production, large teams |

Rate limits are configured in:
- Request limits: [deployment/base/policies/gateway-rate-limits.yaml](../../base/policies/gateway-rate-limits.yaml)
- Token limits: [deployment/base/policies/gateway-token-rate-limits.yaml](../../base/policies/gateway-token-rate-limits.yaml)

## Next Steps

After successful testing:

1. **Explore the architecture**: Review `ARCHITECTURE.md` for detailed diagrams
2. **Modify policies**: Edit AuthPolicy or RateLimitPolicy and apply with `kubectl apply -k deployment/overlays/kind`
3. **Deploy your own model**: Use `test-models/llm-katan/` as a template
4. **Integrate with frontend**: Start the React frontend and connect to `http://localhost`
5. **Experiment with tiers**: Create users with different RBAC permissions
6. **Contribute**: Create a PR with improvements or new features
