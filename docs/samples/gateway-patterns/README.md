# Gateway Patterns

Curated Gateway API deployment patterns for MaaS on OpenShift. Each pattern
provides copy-pasteable manifests, prerequisites, verification steps, and
common failure modes.

These patterns were derived from or aligned with the reference gist at
<https://gist.github.com/lphiri/83ce1ecb17f7aa7efb76275560554d32>.
Product samples may differ after review and alignment with supported
OpenShift AI / ODH versions.

## Pattern index

| Pattern | Directory | Environment | Purpose |
|---------|-----------|-------------|---------|
| [ClusterIP + Route re-encrypt](clusterip-route-reencrypt/) | `clusterip-route-reencrypt/` | Dev / Lab / Production | ClusterIP Gateway Service fronted by an OpenShift Route with re-encrypt TLS; no external LoadBalancer required |

## Environment matrix

| Environment | Recommended pattern | TLS termination | Namespace expectations | Notes |
|-------------|--------------------|-----------------|-----------------------|-------|
| **Development / Lab** | ClusterIP + Route re-encrypt | Router terminates client TLS; re-encrypts to Gateway via service-ca | `openshift-ingress` for Gateway; application namespace for HTTPRoute | Self-signed or service-ca certs acceptable; use `router.openshift.io/service-ca-certificate` annotation |
| **Production** | ClusterIP + Route re-encrypt | Router terminates client TLS with a trusted certificate; re-encrypts to Gateway via service-ca | `openshift-ingress` for Gateway; application namespace for HTTPRoute | Replace the default Router wildcard cert with a CA-signed certificate; rotate Secrets before expiry |

## How to pick a pattern

1. **Do you need an external LoadBalancer?** If not (bare-metal, restricted cloud,
   or preference for Router-managed ingress), use **ClusterIP + Route re-encrypt**.
2. **Who terminates TLS?** All current patterns use the OpenShift Router for
   client-facing TLS and re-encrypt to the Gateway.
3. **Which labels and annotations?** Each pattern documents the required MaaS
   and Kuadrant labels. See the per-pattern README for details.

## MaaS integration

After deploying a gateway pattern, complete the MaaS-specific steps:

- **Attach models**: Set `spec.router.gateway.refs` on your LLMInferenceService
  to point at the deployed Gateway. See [Model Gateway and Serving](../../content/configuration-and-management/model-gateway-and-serving.md).
- **TLS (Authorino, maas-api)**: Configure end-to-end TLS for authentication and
  API traffic. See [TLS Configuration](../../content/configuration-and-management/tls-configuration.md).
- **Install guide**: For the full MaaS installation flow (database, DSC, validation),
  see [Install MaaS Components](../../content/install/maas-setup.md).
- **Policies**: The `kuadrant.io/gateway: "true"` label enables Kuadrant policy
  attachment (AuthPolicy, RateLimitPolicy). The `opendatahub.io/managed: "false"`
  annotation lets maas-controller manage AuthPolicies on the Gateway.

## Adding a new pattern

1. Create a directory under `docs/samples/gateway-patterns/` named after the pattern.
2. Include all YAML manifests with placeholder values (never ship production
   certificates, hostnames, or customer-specific identifiers).
3. Add a `kustomization.yaml` for resources that can be applied together.
4. Write a `README.md` covering: when to use, how it works, prerequisites, apply
   steps, verification, common failure modes, and customization notes.
5. Update the pattern index table above and the environment matrix.
6. Update the gateway patterns documentation page in `docs/content/`.
