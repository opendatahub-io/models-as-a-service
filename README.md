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

- **Openshift cluster** (4.19.9+) with kubectl/oc access

## 🚀 Quick Start

### Deploy Infrastructure

Use the unified deployment script for all deployment scenarios:

```bash
# Deploy RHOAI (default)
./scripts/deploy.sh

# Deploy ODH
./scripts/deploy.sh --operator-type odh

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
| `--operator-type` | `rhoai`, `odh` | `rhoai` | Which operator to install |
| `--rate-limiter` | `rhcl`, `kuadrant` | auto | Rate limiting component (rhcl for operators, kuadrant for kustomize) |
| `--enable-tls-backend` | flag | enabled | TLS for Authorino ↔ MaaS API |
| `--skip-certmanager` | flag | auto-detect | Skip cert-manager installation |
| `--skip-lws` | flag | auto-detect | Skip LeaderWorkerSet installation |
| `--namespace` | string | auto | Target namespace |
| `--timeout` | seconds | `300` | Operation timeout |
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
| `OPERATOR_CATALOG` | Custom operator catalog | `quay.io/opendatahub/catalog:pr-456` |
| `OPERATOR_IMAGE` | Custom operator image | `quay.io/opendatahub/operator:pr-456` |
| `OPERATOR_TYPE` | Operator type (rhoai/odh) | `odh` |
| `RATE_LIMITER` | Rate limiter (rhcl/kuadrant) | `kuadrant` |
| `LOG_LEVEL` | Logging verbosity | `DEBUG`, `INFO`, `WARN`, `ERROR` |

**Note:** TLS backend is enabled by default. Use `--disable-tls-backend` to disable.

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
# Skip optional operators (if already installed)
./scripts/deploy.sh --skip-certmanager --skip-lws

# Deploy without TLS backend (HTTP tier lookup)
./scripts/deploy.sh --disable-tls-backend
```


## 📚 Documentation

- [Deployment Guide](docs/content/quickstart.md) - Complete deployment instructions
- [MaaS API Documentation](maas-api/README.md) - Go API for key management

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
