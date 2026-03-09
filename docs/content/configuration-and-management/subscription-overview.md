# Subscription Management Overview

This guide explains how to configure and manage **subscriptions** for the MaaS Platform. The subscription system provides flexible, per-model access control and rate limiting using Kubernetes-native custom resources.

## Overview

The subscription architecture separates **access control** from **commercial entitlements**:

| Concern | Handled By | Examples |
|---------|------------|----------|
| **Access Control** | MaaSAuthPolicy | Who (groups/users) can access which models |
| **Commercial Limits** | MaaSSubscription | Per-model token rate limits, quotas |
| **Model Catalog** | MaaSModelRef | Which models are available and their endpoints |

This separation enables:

- **Multiple subscriptions per user** — Users can belong to multiple groups, each with different subscription plans
- **Per-model rate limits** — Each model can have different token limits within a subscription
- **Explicit subscription selection** — When a user has multiple subscriptions, they specify which one to use via the `X-MaaS-Subscription` header
- **GitOps-friendly configuration** — CRDs with schema validation, standard Kustomize patching

## Documentation Structure

This subscription management documentation is organized into three sections:

1. **[Subscription Overview](subscription-overview.md)** (this document) — High-level overview of the subscription system
2. **[Subscription Configuration](subscription-configuration.md)** — Step-by-step configuration guide
3. **[Subscription Concepts](subscription-concepts.md)** — Reference material explaining how the system works

## Quick Start

To get started with subscription management:

1. Ensure the **MaaS controller** is installed (reconciles MaaSModelRef, MaaSAuthPolicy, MaaSSubscription)
2. Create **MaaSModelRef** CRs for each model you want to expose
3. Create **MaaSAuthPolicy** CRs to define who can access which models
4. Create **MaaSSubscription** CRs to define per-model token rate limits

See the [Subscription Configuration](subscription-configuration.md) guide for detailed steps.

## Dual-Gate Model

Both gates must pass for a request to succeed:

| Gate | CRD | Question | Failure |
|------|-----|----------|---------|
| **Access** | MaaSAuthPolicy | Is this user allowed to access this model? | 401/403 |
| **Commercial** | MaaSSubscription | Does this user have a subscription covering this model? | 429 |

- A user can have **access** (via MaaSAuthPolicy) but **no subscription** → **429** (rate limited)
- A user can have a **subscription** but **no access** → **403** (forbidden)

## Key Concepts

- **MaaSModelRef** — Registers a model with MaaS; the controller sets `status.endpoint` and `status.phase`
- **MaaSAuthPolicy** — Maps subjects (groups/users) to models; creates Kuadrant AuthPolicies
- **MaaSSubscription** — Maps owner groups to models with per-model token rate limits; creates Kuadrant TokenRateLimitPolicies
- **X-MaaS-Subscription header** — Required when a user has multiple subscriptions to specify which one to use for the request

## Related Documentation

- [Subscription Architecture](https://github.com/opendatahub-io/models-as-a-service/blob/main/archdiagrams/SubscriptionArch.md) — Design document for the subscription model
- [MaaS Controller old-vs-new flow](https://github.com/opendatahub-io/models-as-a-service/blob/main/maas-controller/docs/old-vs-new-flow.md) — Comparison of subscription-based flows
