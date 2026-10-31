"""Helpers for the AIGuardrail provider binding e2e.

Self-contained on purpose: the guardrail tests are the only consumer, they need
a NeMo Guardrails install that the rest of the suite does not, and nothing here
belongs in the shared conftest until something else wants it.

The binding verdicts themselves are also covered by envtest in
ai-gateway-controller. What only a cluster can show is the part envtest fakes:
NemoGuardrails publishing status.endpoint off the enable-auth annotation, and
the controller discovering the provider CRD through real RBAC.
"""
import json
import os
import subprocess
import time

import pytest

# Opt-in gate. The suite installs the NeMo Guardrails controller through
# test/e2e/scripts/deploy-trustyai-guardrails.sh, which most runs do not do.
RUN_GUARDRAILS = os.environ.get("E2E_RUN_GUARDRAILS", "").lower() in {"1", "true", "yes"}

NEMO_CRD = "nemoguardrails.trustyai.opendatahub.io"
NEMO_API_VERSION = "trustyai.opendatahub.io/v1alpha1"
AIGUARDRAIL_CRD = "aiguardrails.aigateway.opendatahub.io"
AIGUARDRAIL_API_VERSION = "aigateway.opendatahub.io/v1alpha1"

# status.endpoint is published only for an auth-protected server, and
# status.endpoint is the only field the binding controller reads for readiness.
ENABLE_AUTH_ANNOTATION = "security.opendatahub.io/enable-auth"

# The operator ships its default configurations into its own namespace. A
# provider's nemoConfigs[].configMaps must resolve in the provider namespace,
# so one is copied across per test namespace.
OPERATOR_NAMESPACE = os.environ.get("DEPLOYMENT_NAMESPACE", "opendatahub")
DEFAULT_CONFIG_ID = "default-all"

# The mcp-guardrails overlay applies namePrefix: trustyai-service-operator-, so
# the shipped default installs under a prefixed name. The copy drops the prefix.
SOURCE_CONFIGMAP = os.environ.get(
    "E2E_GUARDRAIL_SOURCE_CONFIGMAP",
    "trustyai-service-operator-nemo-guardrails-default-all",
)
DEFAULT_CONFIGMAP = "nemo-guardrails-default-all"

PROVIDER_READY_TIMEOUT = int(os.environ.get("E2E_GUARDRAIL_PROVIDER_TIMEOUT", "300"))
VERDICT_TIMEOUT = int(os.environ.get("E2E_GUARDRAIL_VERDICT_TIMEOUT", "120"))
POLL_INTERVAL = 3

# A polling loop cannot check its own deadline while oc is blocked, so a
# stalled request would outlast the two timeouts above and hold the worker.
# Matches the 30s the rest of the suite bounds its oc calls with.
OC_TIMEOUT = int(os.environ.get("E2E_GUARDRAIL_OC_TIMEOUT", "30"))


def oc(*args, check=True, input_text=None):
    """Run an oc command, capturing output."""
    return subprocess.run(
        ["oc", *args],
        capture_output=True,
        text=True,
        check=check,
        input=input_text,
        timeout=OC_TIMEOUT,
    )


def apply(manifest: dict):
    oc("apply", "-f", "-", input_text=json.dumps(manifest))


def delete(kind: str, name: str, namespace: str):
    """Delete a resource, tolerating one that is already gone.

    An absent resource is the only outcome cleanup is prepared for, and
    --ignore-not-found covers it. Anything else -- denied, API error -- is
    raised, so a teardown that leaves resources behind says why.
    """
    oc("delete", kind, name, "-n", namespace, "--ignore-not-found=true")


def get(kind: str, name: str, namespace: str) -> dict:
    result = oc("get", kind, name, "-n", namespace, "-o", "json")
    return json.loads(result.stdout)


def crd_installed(name: str) -> bool:
    """Report whether the CRD exists, raising on anything that is not absence.

    Only a not-found answers the question. Reading a denial or an API error as
    "absent" skips the guardrail suite on a cluster that has the platform, and
    a run that skipped everything reports the same green as one that passed.
    """
    result = oc("get", "crd", name, check=False)
    if result.returncode == 0:
        return True
    if "NotFound" in result.stderr or "not found" in result.stderr:
        return False
    raise AssertionError(f"cannot read CRD {name}: {result.stderr.strip()}")


def require_opt_in():
    """Skip unless the run declares the guardrail platform is installed."""
    if not RUN_GUARDRAILS:
        pytest.skip(
            "guardrail e2e is opt-in: run "
            "test/e2e/scripts/deploy-trustyai-guardrails.sh and set "
            "E2E_RUN_GUARDRAILS=true"
        )


def require_guardrail_platform():
    """Skip unless both CRDs the binding needs are installed."""
    require_opt_in()
    if not crd_installed(NEMO_CRD):
        pytest.skip(f"{NEMO_CRD} is not installed")
    if not crd_installed(AIGUARDRAIL_CRD):
        pytest.skip(f"{AIGUARDRAIL_CRD} is not installed")


def allowed_consumers_supported() -> bool:
    """Report whether the installed NemoGuardrails CRD accepts allowedConsumers.

    Against a build that predates the field the API server prunes it, the
    controller reads nothing, and every provider behaves as from: Same — so the
    scenarios that drive it would assert a rule that was never stored.
    """
    result = oc("get", "crd", NEMO_CRD, "-o", "json", check=False)
    if result.returncode != 0:
        # The caller turns a False into a skip, so a denial or an API error
        # read as "unsupported" drops these scenarios on a cluster that does
        # support them.
        if "NotFound" in result.stderr or "not found" in result.stderr:
            return False
        raise AssertionError(f"cannot read CRD {NEMO_CRD}: {result.stderr.strip()}")
    crd = json.loads(result.stdout)
    for version in crd.get("spec", {}).get("versions", []):
        schema = version.get("schema", {}).get("openAPIV3Schema", {})
        spec = schema.get("properties", {}).get("spec", {})
        if "allowedConsumers" in spec.get("properties", {}):
            return True
    return False


def create_namespace(name: str):
    manifest = {
        "apiVersion": "v1",
        "kind": "Namespace",
        "metadata": {"name": name},
    }
    apply(manifest)


def enable_tenant_namespace(namespace: str, name: str = "default-tenant"):
    """Allow MaaS tenant resources to be created in namespace.

    The MaaSSubscription webhook refuses a namespace that holds no
    MaasTenantConfig, so a throwaway namespace needs one before it can carry an
    attachment. The webhook reads through the API rather than a cache, so the
    permission holds as soon as this returns.
    """
    apply({
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "MaasTenantConfig",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {},
    })


def delete_namespace(name: str):
    """Request namespace deletion without waiting for it to finish.

    --ignore-not-found covers a namespace a scenario already removed, and
    --wait=false covers one still terminating, so a failure here is real.
    """
    oc("delete", "namespace", name, "--ignore-not-found=true", "--wait=false")


def label_namespace(name: str, key: str, value: str | None):
    """Set a namespace label, or remove it when value is None."""
    label = f"{key}-" if value is None else f"{key}={value}"
    oc("label", "namespace", name, label, "--overwrite")


def copy_default_configmap(namespace: str, name: str = DEFAULT_CONFIGMAP) -> str:
    """Copy the operator's default NeMo configuration into namespace.

    The default name is safe only in a namespace the caller created. Writing it
    into a namespace that already holds one overwrites the configuration some
    other provider is serving, so a caller working in a namespace it does not
    own passes a name of its own. Returns the name written.
    """
    source = get("configmap", SOURCE_CONFIGMAP, OPERATOR_NAMESPACE)
    apply({
        "apiVersion": "v1",
        "kind": "ConfigMap",
        "metadata": {"name": name, "namespace": namespace},
        "data": source.get("data", {}),
    })
    return name


def provider_manifest(
    namespace: str,
    name: str = "nemo",
    config_id: str = DEFAULT_CONFIG_ID,
    config_maps=None,
    enable_auth: bool = True,
    allowed_consumers: dict | None = None,
) -> dict:
    """Build a NemoGuardrails provider.

    allowed_consumers is written verbatim so a scenario can plant a rule the
    binding controller must refuse.
    """
    metadata = {"name": name, "namespace": namespace}
    if enable_auth:
        metadata["annotations"] = {ENABLE_AUTH_ANNOTATION: "true"}
    spec = {
        "nemoConfigs": [{
            "name": config_id,
            "default": True,
            "configMaps": config_maps or [DEFAULT_CONFIGMAP],
        }],
    }
    if allowed_consumers is not None:
        spec["allowedConsumers"] = allowed_consumers
    return {
        "apiVersion": NEMO_API_VERSION,
        "kind": "NemoGuardrails",
        "metadata": metadata,
        "spec": spec,
    }


def policy_manifest(
    namespace: str,
    provider_name: str = "nemo",
    provider_namespace: str | None = None,
    config_id: str = DEFAULT_CONFIG_ID,
    name: str = "policy",
) -> dict:
    """Build an AIGuardrail.

    provider_namespace is omitted for a same-namespace reference, which is the
    case the provider's default Same rule authorizes.
    """
    ref = {"name": provider_name}
    if provider_namespace:
        ref["namespace"] = provider_namespace
    return {
        "apiVersion": AIGUARDRAIL_API_VERSION,
        "kind": "AIGuardrail",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {
            "provider": {"timeout": "5s", "nemo": {"ref": ref}},
            "checks": [{
                "name": "content-safety",
                "configId": config_id,
                "phases": ["Input"],
            }],
        },
    }


def wait_for_provider_endpoint(namespace: str, name: str = "nemo", timeout=None) -> str:
    """Wait for the NeMo server to publish status.endpoint, and return it."""
    timeout = timeout or PROVIDER_READY_TIMEOUT
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        provider = get("nemoguardrails", name, namespace)
        endpoint = provider.get("status", {}).get("endpoint")
        if endpoint:
            return endpoint
        last = provider.get("status")
        time.sleep(POLL_INTERVAL)
    raise AssertionError(
        f"NemoGuardrails {namespace}/{name} published no status.endpoint "
        f"within {timeout}s; last status: {last}"
    )


def condition(policy: dict, condition_type: str) -> dict | None:
    for item in policy.get("status", {}).get("conditions", []):
        if item.get("type") == condition_type:
            return item
    return None


def wait_for_accepted_reason(
    namespace: str, reason: str, name: str = "policy", timeout=None,
) -> dict:
    """Wait for Accepted to carry reason for the policy's current generation.

    The generation gate is what makes this an assertion rather than a race: a
    verdict left over from an earlier spec says nothing about the edit the
    scenario just made.
    """
    timeout = timeout or VERDICT_TIMEOUT
    deadline = time.time() + timeout
    last = None
    while time.time() < deadline:
        policy = get("aiguardrail", name, namespace)
        accepted = condition(policy, "Accepted")
        if accepted is not None:
            current = policy.get("metadata", {}).get("generation")
            if accepted.get("observedGeneration") == current:
                if accepted.get("reason") == reason:
                    return policy.get("status", {})
                last = f"{accepted.get('reason')}: {accepted.get('message')}"
            else:
                last = f"stale generation {accepted.get('observedGeneration')} != {current}"
        time.sleep(POLL_INTERVAL)
    raise AssertionError(
        f"AIGuardrail {namespace}/{name} never reported Accepted={reason} "
        f"within {timeout}s; last: {last}"
    )


def assert_binding_published(status: dict):
    """A binding is enforceable only while both fields are set."""
    assert status.get("providerEndpoint"), \
        f"an accepted binding must publish the endpoint it accepted: {status}"
    assert status.get("bindingRevision"), \
        f"an accepted binding must publish its revision: {status}"


def assert_binding_withdrawn(status: dict):
    """A refusal must leave nothing for a consumer to keep gating on."""
    assert not status.get("providerEndpoint"), \
        f"a refused policy must not publish an endpoint: {status}"
    assert not status.get("bindingRevision"), \
        f"a refused policy must not publish a revision: {status}"