# Reminder: openshift-ai-inference Gateway Removed from Docs

**Date:** 2026-03-08

The `openshift-ai-inference` Gateway was removed from the [MaaS setup documentation](content/install/maas-setup.md) because it was believed to be unnecessary for the current version.

**What was removed:**
- The YAML block that created the `openshift-ai-inference` Gateway (in `openshift-ingress` namespace)
- The Gateway Architecture info note that described the segregated gateway approach

**If you find out later that openshift-ai-inference IS needed**, restore it by:

1. Re-adding the Gateway YAML to `docs/content/install/maas-setup.md` in the "Create Gateway" section, before the maas-default-gateway block
2. Re-adding the Gateway Architecture info note

The original content was:
- Gateway name: `openshift-ai-inference`
- Namespace: `openshift-ingress`
- Infrastructure label: `serving.kserve.io/gateway: kserve-ingress-gateway`
- Purpose: Standard KServe inference (vs maas-default-gateway for token auth and rate limiting)

**Reference:** The Gateway is still defined in `deployment/base/networking/odh/odh-gateway-api.yaml` if needed for kustomize deployments.
