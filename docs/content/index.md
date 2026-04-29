# MaaS Platform Documentation

Welcome to the Models-as-a-Service (MaaS) Platform documentation.

The MaaS Platform enhances the model serving capabilities of [Open Data Hub](https://github.com/opendatahub-io) by adding a management layer for self-service access control, rate limiting, and subscription-based entitlements.

Use this platform to streamline the deployment of your models, monitor usage, and effectively manage costs.

## 📚 Documentation Overview

### 🚀 Getting Started

- **[QuickStart Guide](quickstart.md)** - Complete platform deployment instructions
- **[Architecture](concepts/architecture.md)** - Overview of the MaaS Platform architecture

### ⚙️ Configuration & Management

- **[Access and Quota Overview](concepts/subscription-overview.md)** - Policies (access) and subscriptions (quota) for model access
- **[Subscription limitations and known issues](configuration-and-management/subscription-known-issues.md)** - Rate limits on shared routes, API keys, and other planning notes
- **[Model Setup (On Cluster)](configuration-and-management/model-setup.md)** - Setting up models for MaaS
- **[Self-Service Model Access](user-guide/self-service-model-access.md)** - Managing model access and policies

### 📋 Release Notes

- **[Release notes](release-notes/index.md)** - Version highlights and known limitations by release

### 🔧 Advanced Administration

- **[Observability](advanced-administration/observability.md)** - Monitoring, metrics, and dashboards
- **[Limitador Persistence](advanced-administration/limitador-persistence.md)** - Redis backend for persistent rate-limit counters
- **[TLS Configuration](configuration-and-management/tls-configuration.md)** - Configuring TLS for MaaS API, Authorino, and Gateway
- **[Token Management](configuration-and-management/token-management.md)** - Token authentication system and lifecycle

### 📖 Installation Guide

- **[Prerequisites](install/prerequisites.md)** - Requirements and database setup
- **[Platform Setup](install/platform-setup.md)** - Install ODH/RHOAI with MaaS
- **[MaaS Setup](install/maas-setup.md)** - Gateway AuthPolicy and policies
- **[Validation](install/validation.md)** - Verify your deployment

### 🔄 Migration

- **[Tier to Subscription Migration](migration/tier-to-subscription.md)** - Migrate from tier-based to subscription-based access control
