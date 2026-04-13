# ODH - Models as a Service with Policy Management

Our goal is to create a comprehensive platform for **Models as a Service** with real-time policy management.

> [!IMPORTANT]
> This project is a work in progress and is not yet ready for production.

## 📦 Technology Stack

- **OpenShift**: Kubernetes platform
- **Gateway API**: Traffic routing and management (OpenShift native implementation)
- **Kuadrant/Authorino/Limitador**: API gateway and policy engine
- **KServe**: Model serving platform
- **React**: Frontend framework
- **Go**: Backend frameworks

## 📋 Prerequisites

- **OpenShift cluster** (4.19.9+) with kubectl/oc access
- **Kuadrant v1.4.2+** (ODH) or **RHCL v1.3+** (RHOAI) - **Required for MaaS v0.2.0+**
- **PostgreSQL database** (for production ODH/RHOAI deployments)

### ⚠️ Important Version Requirements

#### Kuadrant 1.4.2+ Required (MaaS v0.2.0+)

**MaaS v0.2.0 and later requires Kuadrant 1.4.2+ (ODH) or RHCL 1.3+ (RHOAI).**

**Why Kuadrant 1.4.2+ is required:**

MaaS v0.2.0 requires the authorization header stripping capability added in Authorino v0.23.1 (shipped with Kuadrant 1.4.2) to protect user credentials from potential exfiltration to model backends.

**Security Context:**

When a user makes an inference request with their OpenShift token or API key, that credential must be validated by Authorino but should NOT be forwarded to model backends (whether internal KServe models or external providers). Kuadrant 1.4.2+ allows Authorino to:

1. Validate the incoming user credential (OpenShift token or MaaS API key)
2. Strip/replace the Authorization header before forwarding to model backends
3. Optionally inject model-specific credentials from Kubernetes Secrets (credentialRef) for ExternalModel resources

This prevents credential exfiltration where a malicious or compromised model service could capture and misuse user tokens.

**Migration Notes:**

- The deployment script (`scripts/deploy.sh`) automatically installs Kuadrant 1.4.2 for new deployments
- For existing deployments, upgrade Kuadrant/RHCL before upgrading to MaaS v0.2.0+

For detailed version compatibility, see [Version Compatibility](docs/content/install/prerequisites.md#version-compatibility).

#### Database Required for Production

MaaS requires a PostgreSQL database for API key management. For production ODH/RHOAI deployments, you must create a Secret with the database connection URL **before** enabling modelsAsService.

See [Database Prerequisites](docs/content/install/prerequisites.md#database-prerequisite) for details.

Note: The `scripts/deploy.sh` script creates a development PostgreSQL instance automatically.

## 🚀 Quick Start

### Deploy Infrastructure

Use the unified deployment script for all deployment scenarios:

```bash
# Deploy ODH (default)
./scripts/deploy.sh

# Deploy RHOAI
./scripts/deploy.sh --operator-type rhoai

# Deploy via Kustomize
./scripts/deploy.sh --deployment-mode kustomize
```

For detailed instructions, see the [Deployment Guide](docs/content/quickstart.md) or the [Deployment Options](#-deployment-options) section below.

## 🛠️ Deployment Options

### Basic Deployment

```bash
./scripts/deploy.sh [OPTIONS]
```

### Key Options

| Flag | Values | Default | Description |
|------|--------|---------|-------------|
| `--deployment-mode` | `operator`, `kustomize` | `operator` | Deployment method |
| `--operator-type` | `odh`, `rhoai` | `odh` | Which operator to install |
| `--enable-tls-backend` | flag | enabled | TLS for Authorino ↔ MaaS API |
| `--disable-tls-backend` | flag | `false` | Disable TLS backend |
| `--namespace` | string | auto | Target namespace |
| `--verbose` | flag | false | Enable debug logging |
| `--dry-run` | flag | false | Show plan without executing |
| `--help` | flag | - | Display full help |

### Advanced Options (PR Testing)

| Flag | Description | Example |
|------|-------------|---------|
| `--operator-catalog` | Custom operator catalog/index image | `quay.io/opendatahub/catalog:pr-456` |
| `--operator-image` | Custom operator image (patches CSV) | `quay.io/opendatahub/operator:pr-456` |
| `--channel` | Operator channel override | `fast`, `fast-3` |

### Environment Variables

| Variable | Description | Example |
|----------|-------------|---------|
| `MAAS_API_IMAGE` | Custom MaaS API container image (works in both operator and kustomize modes) | `quay.io/user/maas-api:pr-123` |
| `MAAS_CONTROLLER_IMAGE` | Custom MaaS controller container image | `quay.io/user/maas-controller:pr-123` |
| `METADATA_CACHE_TTL` | TTL in seconds for Authorino metadata HTTP caching | `60` (default), `300` |
| `AUTHZ_CACHE_TTL` | TTL in seconds for Authorino OPA authorization caching | `60` (default), `30` |
| `OPERATOR_CATALOG` | Custom operator catalog | `quay.io/opendatahub/catalog:pr-456` |
| `OPERATOR_IMAGE` | Custom operator image | `quay.io/opendatahub/operator:pr-456` |
| `OPERATOR_TYPE` | Operator type (rhoai/odh) | `odh` |
| `LOG_LEVEL` | Logging verbosity | `DEBUG`, `INFO`, `WARN`, `ERROR` |

**Note:** TLS backend is enabled by default. Use `--disable-tls-backend` to disable.

**Note:** The policy engine is auto-determined based on operator type (`rhcl` for RHOAI, `kuadrant` for ODH/kustomize) and does not need to be set manually.

### Deployment Examples

#### Standard Deployments

```bash
# Deploy RHOAI
./scripts/deploy.sh --operator-type rhoai

# Deploy ODH
./scripts/deploy.sh --operator-type odh
```

#### Testing PRs

```bash
# Test MaaS API PR #123
MAAS_API_IMAGE=quay.io/myuser/maas-api:pr-123 \
  ./scripts/deploy.sh --operator-type odh

# Test ODH operator PR #456 with custom manifests
./scripts/deploy.sh \
  --operator-type odh \
  --operator-catalog quay.io/opendatahub/opendatahub-operator-catalog:pr-456 \
  --operator-image quay.io/opendatahub/opendatahub-operator:pr-456
```

#### Minimal Deployments

```bash
# Deploy without TLS backend (HTTP for Authorino to maas-api)
./scripts/deploy.sh --disable-tls-backend
```


## 📚 Documentation

- [Deployment Guide](docs/content/quickstart.md) - Complete deployment instructions
- [MaaS API Documentation](maas-api/README.md) - Go API for key management
- [Authorino Caching Configuration](docs/content/configuration-and-management/authorino-caching.md) - Cache tuning for metadata and authorization

Online Documentation: [https://opendatahub-io.github.io/models-as-a-service/](https://opendatahub-io.github.io/models-as-a-service/)

## 🤝 Contributing

We welcome contributions! Please:
1. Fork the repository
2. Create a feature branch
3. Make your changes
4. Submit a pull request

## 📝 License

This project is licensed under the Apache 2.0 License.

## 📞 Support

For questions or issues:
- Open an issue on GitHub
- Check the [deployment guide](docs/content/quickstart.md) for troubleshooting
- Review the [samples](docs/samples/models) for examples
