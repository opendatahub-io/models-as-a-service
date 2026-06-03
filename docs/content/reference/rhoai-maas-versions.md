# RHOAI ↔ MaaS Release Mapping

This table maps each supported Red Hat OpenShift AI (RHOAI) release to the
corresponding Models-as-a-Service (MaaS) component version. Use it for support
ticket triage, CI reproduction, compatibility checks, and upgrade planning.

## Version mapping

| RHOAI Version | MaaS Version | RHOAI Image Tag | Status | Notes |
|---------------|--------------|-----------------|--------|-------|
| 3.4           | v0.1.0       | `v3.4`          | GA     | Subscription-driven access model; tier system removed. See [upgrade guide](../migration/upgrade-to-3.4.md). |
| 3.3           | v0.0.2       | `v3.3`          | TP     | Same MaaS version as 3.2; no MaaS-specific changes. |
| 3.2           | v0.0.2       | `v3.2`          | TP     | TLS, deploy-script updates. MaaS deployed manually (RHOAI 3.2 DSC does not support `modelsAsService`). |
| 3.1           | v0.0.1       | `v3.1`          | TP     | Same MaaS version as 3.0; no MaaS-specific changes. |
| 3.0           | v0.0.1       | —               | TP     | Initial MaaS release; tier-based access model, manual deployment only. |

**Status key:** GA = Generally Available, TP = Tech Preview.

## Image references

Production images are published to the Red Hat registry. The image tag
corresponds to the RHOAI version, not the MaaS version.

| Component        | Registry path                                             |
|------------------|-----------------------------------------------------------|
| maas-api         | `registry.redhat.io/rhoai/odh-maas-api-rhel9:<tag>`      |
| maas-controller  | `registry.redhat.io/rhoai/odh-maas-controller-rhel9:<tag>`|

Upstream development images use the MaaS version tag on `quay.io/opendatahub/`.

## Branch → RHOAI mapping

See [Release Strategy](../contributing/release-strategy.md) for the full
promotion flow. In short:

| Branch   | Feeds into    |
|----------|---------------|
| `main`   | Next release  |
| `stable` | ODH builds    |
| `rhoai`  | Current RHOAI |

## Operator channel

RHOAI 3.x operators use the `stable-3.x` channel from `redhat-operators`.
See [Platform Setup](../install/platform-setup.md) for installation details.

## Updating this table

When a new RHOAI GA ships that bumps the bundled MaaS version, add a row to the
version mapping table in the same release PR (or an explicitly linked follow-up).
