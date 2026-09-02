# Enforcement oracle fixtures

Reference Kuadrant output used as generation oracles: the GatewayEnforcement
compiler must reproduce these. Captured from Kuadrant (Red Hat Connectivity Link
1.3.3), which compiles an AuthPolicy and a TokenRateLimitPolicy into them.

| File | Kuadrant artifact |
|------|-------------------|
| `oracle-pluginconfig-site-a.json` | WasmPlugin `spec.pluginConfig` |
| `oracle-actionset-chat-completions.json` | one WasmPlugin action set |
| `oracle-authconfig-site-a.json` | Authorino `AuthConfig` `spec` |
| `oracle-limitador-site-a.json` | Limitador `spec.limits` |

## Regenerating

Regenerate when a new Kuadrant version changes the compiled output: apply the
MaaS AuthPolicy and TokenRateLimitPolicy, export the generated WasmPlugin,
AuthConfig, and Limitador, update these files, and bump the version above. The
compiler tests compare route conditions and rate-limit bindings, not the
internal name hashes, so hash differences across versions are expected.
