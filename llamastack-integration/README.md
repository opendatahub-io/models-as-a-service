# Llamastack Integration with Models-as-a-Service

This integration provides direct compatibility between [Meta's Llamastack](https://llama-stack.readthedocs.io/) and the Models-as-a-Service (MaaS) platform. By configuring Llamastack to run natively with KServe patterns on port 8000 with HTTPS, it integrates seamlessly with MaaS without requiring wrapper services.

## Overview

The integration enables access to various LLM providers through Llamastack's unified interface while maintaining full compatibility with:

- ✅ **MaaS Gateway** - Authentication, authorization, and routing
- ✅ **Rate Limiting** - Tier-based usage controls
- ✅ **Model Discovery** - Automatic detection via `/v1/models` endpoint
- ✅ **Token Usage Tracking** - Monitoring and billing integration
- ✅ **Health Monitoring** - KServe-compatible health checks
- ✅ **TLS Security** - HTTPS with KServe certificates

## Supported Providers

This integration focuses on **external/cloud providers** that don't require local GPU resources:

| Provider | Models Available | Example |
|----------|-----------------|---------|
| **Google Gemini** | Gemini 1.5 Pro, Flash, 2.0 Flash | `examples/gemini-pro/` |
| **OpenAI** | GPT-4o, GPT-4o Mini, GPT-3.5 Turbo, o1 | `examples/openai-gpt4/` |
| **Anthropic** | Claude 3.5 Sonnet, Haiku, Claude 3 Opus | `examples/anthropic-claude/` |

## Architecture

```
┌─────────────────┐    ┌──────────────────┐    ┌─────────────────────┐
│   MaaS API      │    │   MaaS Gateway   │    │   Llamastack Pod   │
│   (Discovery)   │◄──►│   (Auth/Route)   │◄──►│   (Provider Proxy)  │
└─────────────────┘    └──────────────────┘    └─────────────────────┘
                                                          │
                                                          ▼
                                               ┌─────────────────────┐
                                               │  External Provider  │
                                               │  (Gemini/OpenAI/    │
                                               │   Anthropic)        │
                                               └─────────────────────┘
```

### Key Components

- **Base LLMInferenceService**: Standard KServe deployment template
- **Provider Overlays**: Kustomize configurations for each external provider
- **Configuration Management**: Llamastack YAML configs via ConfigMaps
- **Secret Management**: API keys stored as Kubernetes secrets
- **Health Monitoring**: `/v1/health` endpoint for liveness/readiness

## Quick Start

### Prerequisites

- Kubernetes cluster with MaaS installed
- `kubectl` configured to access your cluster
- API key for your chosen provider (Gemini, OpenAI, or Anthropic)

### Deploy with Google Gemini

1. **Set your API key environment variable:**
   ```bash
   export GEMINI_API_KEY="your-actual-gemini-api-key-here"
   ```

2. **Create API key secret:**
   ```bash
   kubectl create secret generic gemini-gemini-api-key \
     --from-literal=api-key="$GEMINI_API_KEY" \
     -n llm
   ```

3. **Deploy Llamastack:**
   ```bash
   cd llamastack-integration
   kubectl apply -k deploy/overlays/gemini
   ```

4. **Verify deployment:**
   ```bash
   ./scripts/validate-deployment.sh gemini
   ```

5. **Test chat completion:**
   ```bash
   ./scripts/test-chat-completion.sh gemini
   ```

### Deploy with OpenAI

Follow the same pattern with OpenAI:
```bash
export OPENAI_API_KEY="your-actual-openai-api-key-here"
kubectl create secret generic openai-openai-api-key \
  --from-literal=api-key="$OPENAI_API_KEY" \
  -n llm
kubectl apply -k deploy/overlays/openai
```

### Deploy with Anthropic

Follow the same pattern with Anthropic:
```bash
export ANTHROPIC_API_KEY="your-actual-anthropic-api-key-here"
kubectl create secret generic anthropic-anthropic-api-key \
  --from-literal=api-key="$ANTHROPIC_API_KEY" \
  -n llm
kubectl apply -k deploy/overlays/anthropic
```

## Directory Structure

```
llamastack-integration/
├── README.md                          # This file
├── deploy/
│   ├── base/                          # Base LLMInferenceService template
│   │   ├── llamastack.yaml           # Core deployment configuration
│   │   └── kustomization.yaml        # Base kustomization
│   └── overlays/                      # Provider-specific configurations
│       ├── gemini/                    # Google Gemini configuration
│       ├── openai/                    # OpenAI configuration
│       └── anthropic/                 # Anthropic configuration
├── examples/                          # Example deployments with docs
│   ├── gemini-pro/README.md          # Gemini deployment guide
│   ├── openai-gpt4/README.md         # OpenAI deployment guide
│   └── anthropic-claude/README.md    # Anthropic deployment guide
└── scripts/                           # Validation and testing scripts
    ├── validate-deployment.sh         # Deployment validation
    └── test-chat-completion.sh        # End-to-end testing
```

## Configuration

### Base Configuration

The base `llamastack.yaml` uses LlamaStack's "starter" distribution with runtime TLS configuration:

```yaml
apiVersion: serving.kserve.io/v1alpha1
kind: LLMInferenceService
metadata:
  name: llamastack
  annotations:
    alpha.maas.opendatahub.io/tiers: '[]'  # Available to all tiers
spec:
  router:
    gateway:
      refs:
        - name: maas-default-gateway       # Connect to MaaS gateway
          namespace: openshift-ingress
  template:
    containers:
      - name: llamastack
        image: "meta-llama/llamastack:latest"
        command: ["sh", "-c"]
        args:
        - |
          CONFIG=$(python3 -c "from llama_stack.core.utils.config_resolution import resolve_config_or_distro; print(resolve_config_or_distro('starter'))")
          python3 -c "
          import yaml
          with open('$CONFIG') as f:
              config = yaml.safe_load(f)
          config['server']['port'] = 8000
          config['server']['tls_certfile'] = '/var/run/kserve/tls/tls.crt'
          config['server']['tls_keyfile'] = '/var/run/kserve/tls/tls.key'
          with open('/tmp/run-config.yaml', 'w') as f:
              yaml.dump(config, f, default_flow_style=False)
          "
          exec llama stack run /tmp/run-config.yaml
        ports:
          - name: https
            containerPort: 8000            # KServe standard port
```

### Provider Configuration

Each provider overlay includes:

1. **API Key Secret**:
   ```yaml
   apiVersion: v1
   kind: Secret
   metadata:
     name: provider-provider-api-key
   type: Opaque
   stringData:
     api-key: "${PROVIDER_API_KEY}"
   ```

2. **Environment Variable Injection** (via JSON patch):
   ```yaml
   - op: add
     path: /spec/template/containers/0/env/-
     value:
       name: PROVIDER_API_KEY
       valueFrom:
         secretKeyRef:
           name: provider-provider-api-key
           key: api-key
   - op: replace
     path: /spec/model/name
     value: provider-model-name
   ```

The LlamaStack "starter" distribution automatically discovers and configures providers based on available API keys in the environment.

## Testing

### Validation Script

```bash
./scripts/validate-deployment.sh [PROVIDER] [NAMESPACE]
```

Performs comprehensive checks:
- LLMInferenceService status
- Pod health and readiness
- Health endpoint connectivity
- Model discovery through MaaS API

### Chat Completion Test

```bash
./scripts/test-chat-completion.sh [PROVIDER] [NAMESPACE] [MODEL]
```

Tests end-to-end functionality:
- Authentication with MaaS
- Chat completion requests
- Token usage tracking
- Response validation

## Integration with MaaS

### Model Discovery

Llamastack exposes OpenAI-compatible `/v1/models` endpoint that MaaS automatically discovers:

```bash
# MaaS calls this endpoint to discover models
GET https://llamastack-service:8000/v1/models
```

Response includes all configured models from the provider.

### Chat Completions

Chat requests are routed through the MaaS gateway to Llamastack:

```bash
# User request through MaaS gateway
POST https://maas-gateway/v1/chat/completions
Authorization: Bearer <maas-token>

# Routed to Llamastack
POST https://llamastack-service:8000/v1/chat/completions
```

### Authentication Flow

1. User authenticates with MaaS API → receives token
2. User sends requests to MaaS Gateway with token
3. Gateway validates token and routes to Llamastack
4. Llamastack forwards to external provider with API key

### Rate Limiting

Rate limits are enforced by MaaS based on user tier before reaching Llamastack.

## Monitoring and Observability

### Health Checks

- **Liveness**: `GET /v1/health` - Pod restart if failing
- **Readiness**: `GET /v1/health` - Traffic routing control

### Metrics

Llamastack provides token usage in responses:
```json
{
  "usage": {
    "prompt_tokens": 10,
    "completion_tokens": 20,
    "total_tokens": 30
  }
}
```

This integrates with MaaS billing and monitoring systems.

## Troubleshooting

### Common Issues

1. **Pod Not Starting**
   ```bash
   kubectl logs -n llm -l provider=<PROVIDER>
   kubectl describe pod -n llm -l provider=<PROVIDER>
   ```

2. **Models Not Appearing**
   - **Common cause**: API key secret contains placeholder values
   - **Check secret**: `kubectl get secret <provider>-<provider>-api-key -n llm -o jsonpath='{.data.api-key}' | base64 -d`
   - **Fix with env vars**:
     ```bash
     export GEMINI_API_KEY="your-actual-key-here"
     kubectl delete secret gemini-gemini-api-key -n llm
     kubectl create secret generic gemini-gemini-api-key --from-literal=api-key="$GEMINI_API_KEY" -n llm
     kubectl rollout restart deployment gemini-llamastack-kserve -n llm
     ```
   - Verify service health: `./scripts/validate-deployment.sh`

3. **Authentication Failures**
   - Verify API key in secret: `kubectl get secret <provider>-<provider>-api-key -n llm -o yaml`
   - Test API key directly with provider
   - **Ensure env var is set**: `echo $GEMINI_API_KEY` (should not be empty)
   - Check Llamastack logs for API errors: `kubectl logs -n llm -l app.kubernetes.io/name=<provider>-llamastack`

4. **Rate Limiting Issues**
   - Check user tier configuration
   - Verify MaaS gateway policies
   - Monitor token usage patterns

### Debug Commands

```bash
# Check all resources for a provider
kubectl get all -n llm -l provider=<PROVIDER>

# View pod logs
kubectl logs -n llm -l provider=<PROVIDER>

# Check API key secret
kubectl get secret <provider>-<provider>-api-key -n llm -o yaml

# Test health endpoint directly
kubectl port-forward -n llm service/<provider>-llamastack 8443:443
curl -k https://localhost:8443/v1/health
```

## Benefits

- **Zero Infrastructure Changes** - Uses existing MaaS patterns
- **No Wrapper Complexity** - Direct Llamastack integration
- **Provider Flexibility** - Easy to add new providers
- **Full Feature Compatibility** - All MaaS features work as-is
- **Minimal Resource Requirements** - No GPU needed for external providers
- **Standard Compliance** - Pure KServe LLMInferenceService

## Contributing

To add support for additional providers:

1. Create new overlay directory: `deploy/overlays/new-provider/`
2. Add Llamastack configuration with provider-specific settings
3. Create example documentation: `examples/new-provider/README.md`
4. Test with validation scripts

## Security Considerations

- **API Key Management**:
  - ✅ Use environment variables: `export GEMINI_API_KEY="..."`
  - ✅ Verify before deploy: `echo $GEMINI_API_KEY` (ensure not empty)
  - ❌ Never commit actual keys to git
  - ❌ Avoid placeholder values in YAML files
- **Infrastructure Security**:
  - API keys are stored as Kubernetes secrets
  - TLS encryption for all communications
  - MaaS authentication and authorization
  - Network policies for pod-to-pod communication
  - Regular secret rotation recommended

### Environment Variable Best Practices

```bash
# ✅ GOOD: Set environment variables first
export GEMINI_API_KEY="your-actual-key-here"
export OPENAI_API_KEY="your-actual-key-here"

# ✅ GOOD: Use in commands
kubectl create secret generic gemini-gemini-api-key --from-literal=api-key="$GEMINI_API_KEY" -n llm

# ❌ BAD: Hard-coded placeholders
kubectl create secret generic gemini-gemini-api-key --from-literal=api-key="YOUR_GEMINI_API_KEY_HERE" -n llm

# ✅ GOOD: Verify before deployment
if [ -z "$GEMINI_API_KEY" ]; then echo "Error: GEMINI_API_KEY not set"; exit 1; fi
```

## License

This integration follows the same license as the parent Models-as-a-Service project.