# MaaS Platform Documentation

Welcome to the Models-as-a-Service (MaaS) Platform documentation.

The MaaS Platform enhances the model serving capabilities of [Open Data Hub](https://github.com/opendatahub-io) by adding a management layer for self-service access control, rate limiting, and subscription-based entitlements.

Use this platform to streamline the deployment of your models, monitor usage, and effectively manage costs.

## 📚 Documentation Overview

### 🚀 Getting Started

- **[QuickStart Guide](quickstart.md)** - Complete platform deployment instructions
- **[Architecture](concepts/architecture.md)** - Overview of the MaaS Platform architecture

### 👤 User Guide

- **[API Key Management](user-guide/api-key-management.md)** - Creating and managing API keys
- **[Model Discovery](user-guide/model-discovery.md)** - Listing available models
- **[Inference](user-guide/inference.md)** - Making inference requests

### ⚙️ Administration

- **[Access and Quota Overview](concepts/subscription-overview.md)** - Configuring policies and subscriptions
- **[Model Setup](configuration-and-management/model-setup.md)** - Setting up models for MaaS
- **[API Key Administration](configuration-and-management/api-key-administration.md)** - Bulk revocation and cleanup
- **[Observability](advanced-administration/observability.md)** - Monitoring, metrics, and dashboards
- **[Limitador Persistence](advanced-administration/limitador-persistence.md)** - Redis backend for rate-limit counters
- **[TLS Configuration](configuration-and-management/tls-configuration.md)** - Configuring TLS

### 📋 Release Notes

- **[Release notes](release-notes/index.md)** - Version highlights and known limitations by release

### 📖 Installation Guide

- **[Prerequisites](install/prerequisites.md)** - Requirements and database setup
- **[Platform Setup](install/platform-setup.md)** - Install ODH/RHOAI with MaaS
- **[MaaS Setup](install/maas-setup.md)** - Gateway AuthPolicy and policies
- **[Validation](install/validation.md)** - Verify your deployment

### 🔄 Migration

- **[Tier to Subscription Migration](migration/tier-to-subscription.md)** - Migrate from tier-based to subscription-based access control
