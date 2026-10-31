"""E2E for the AIGuardrail provider binding against a real NeMo Guardrails install.

Covers the verdicts the ai-gateway-controller binding reconciler publishes when
the provider is a NemoGuardrails the TrustyAI operator actually reconciles.

Requirements:
  - test/e2e/scripts/deploy-trustyai-guardrails.sh has been run.
  - ai-gateway-controller is deployed and the AIGuardrail CRD is installed.
  - The controller registers its provider watch only when the NemoGuardrails CRD
    already resolves at startup. Restart it after installing TrustyAI, or
    provider edits land on the resync cadence instead of immediately.

Environment:
  - E2E_RUN_GUARDRAILS=true: opt in. The module installs no platform of its own.
  - E2E_GUARDRAIL_PROVIDER_TIMEOUT: how long a NeMo server may take to publish
    status.endpoint (default 300s).
"""
import uuid

import pytest

import guardrail_helpers as gr

pytestmark = [
    pytest.mark.xdist_group("guardrails"),
    pytest.mark.serial,
    pytest.mark.guardrails,
]


@pytest.fixture(scope="module", autouse=True)
def guardrail_platform():
    gr.require_guardrail_platform()


@pytest.fixture(scope="module")
def suffix():
    return uuid.uuid4().hex[:6]


@pytest.fixture
def namespace_factory():
    """Create namespaces that are removed when the test ends."""
    created = []

    def create(name: str) -> str:
        gr.create_namespace(name)
        created.append(name)
        return name

    yield create
    for name in reversed(created):
        gr.delete_namespace(name)


@pytest.fixture(scope="module")
def provider_namespace(suffix):
    """A namespace holding one NeMo server, shared by the scenarios that read it.

    Module-scoped because a NeMo server takes minutes to publish an endpoint and
    most scenarios only need it to exist, not to be theirs.
    """
    name = f"e2e-guardrails-provider-{suffix}"
    gr.create_namespace(name)
    # Setting up a NeMo server can fail, and waiting for its endpoint can time
    # out. Deleting the namespace takes the ConfigMap and the provider with it,
    # so the one finally covers every way this gets partway through.
    try:
        gr.copy_default_configmap(name)
        gr.apply(gr.provider_manifest(name))
        gr.wait_for_provider_endpoint(name)
        yield name
    finally:
        gr.delete_namespace(name)


@pytest.fixture
def reset_allowed_consumers(provider_namespace):
    """Restore the provider's default Same rule after a scenario rewrites it."""
    yield
    gr.apply(gr.provider_manifest(provider_namespace))


@pytest.fixture
def requires_allowed_consumers():
    if not gr.allowed_consumers_supported():
        pytest.skip(
            "the installed NemoGuardrails CRD has no spec.allowedConsumers; "
            "install from a TrustyAI ref that carries it"
        )


def test_policy_binding_a_ready_provider_is_accepted(provider_namespace):
    """The endpoint TrustyAI published is the endpoint the binding accepts."""
    gr.apply(gr.policy_manifest(provider_namespace))

    status = gr.wait_for_accepted_reason(provider_namespace, "PolicyAccepted")
    gr.assert_binding_published(status)

    endpoint = gr.wait_for_provider_endpoint(provider_namespace)
    assert status["providerEndpoint"] == endpoint

    policy = gr.get("aiguardrail", "policy", provider_namespace)
    assert gr.condition(policy, "Compatible")["reason"] == "ChecksSatisfiable"
    assert gr.condition(policy, "ProviderReady")["reason"] == "ProviderAvailable"

    gr.delete("aiguardrail", "policy", provider_namespace)


def test_consumer_in_another_namespace_is_refused_by_default(
    provider_namespace, namespace_factory, suffix,
):
    """The provider's default Same rule does not authorize a cross-namespace bind."""
    consumer = namespace_factory(f"e2e-guardrails-consumer-{suffix}")
    gr.apply(gr.policy_manifest(consumer, provider_namespace=provider_namespace))

    status = gr.wait_for_accepted_reason(consumer, "ConsumerNotAuthorized")
    gr.assert_binding_withdrawn(status)


def test_provider_opening_itself_to_all_accepts_a_remote_consumer(
    requires_allowed_consumers, provider_namespace, namespace_factory, suffix,
    reset_allowed_consumers,
):
    consumer = namespace_factory(f"e2e-guardrails-all-{suffix}")
    gr.apply(gr.policy_manifest(consumer, provider_namespace=provider_namespace))
    gr.wait_for_accepted_reason(consumer, "ConsumerNotAuthorized")

    gr.apply(gr.provider_manifest(
        provider_namespace, allowed_consumers={"namespaces": {"from": "All"}},
    ))

    gr.assert_binding_published(
        gr.wait_for_accepted_reason(consumer, "PolicyAccepted")
    )


def test_selector_rule_follows_the_consumer_namespace_labels(
    requires_allowed_consumers, provider_namespace, namespace_factory, suffix,
    reset_allowed_consumers,
):
    """Revoking the label touches neither CR, so only the Namespace watch can carry it."""
    consumer = namespace_factory(f"e2e-guardrails-sel-{suffix}")
    gr.apply(gr.provider_manifest(provider_namespace, allowed_consumers={
        "namespaces": {
            "from": "Selector",
            "selector": {"matchLabels": {"guardrails": "allowed"}},
        },
    }))
    gr.apply(gr.policy_manifest(consumer, provider_namespace=provider_namespace))
    gr.wait_for_accepted_reason(consumer, "ConsumerNotAuthorized")

    gr.label_namespace(consumer, "guardrails", "allowed")
    gr.assert_binding_published(
        gr.wait_for_accepted_reason(consumer, "PolicyAccepted")
    )

    gr.label_namespace(consumer, "guardrails", None)
    gr.assert_binding_withdrawn(
        gr.wait_for_accepted_reason(consumer, "ConsumerNotAuthorized")
    )


def test_unreadable_permission_rule_fails_closed(
    requires_allowed_consumers, provider_namespace, namespace_factory, suffix,
    reset_allowed_consumers,
):
    """A rule the controller cannot read is not evidence that the bind is permitted."""
    consumer = namespace_factory(f"e2e-guardrails-bad-{suffix}")
    gr.apply(gr.provider_manifest(
        provider_namespace, allowed_consumers={"namespaces": {"from": "Nope"}},
    ))
    gr.apply(gr.policy_manifest(consumer, provider_namespace=provider_namespace))

    gr.assert_binding_withdrawn(
        gr.wait_for_accepted_reason(consumer, "InvalidAllowedConsumers")
    )


def test_check_selecting_an_undeclared_configuration_is_refused(provider_namespace):
    gr.apply(gr.policy_manifest(
        provider_namespace, config_id="no-such-config", name="unknown-config",
    ))

    status = gr.wait_for_accepted_reason(
        provider_namespace, "UnknownCheckConfig", name="unknown-config",
    )
    gr.assert_binding_withdrawn(status)

    policy = gr.get("aiguardrail", "unknown-config", provider_namespace)
    compatible = gr.condition(policy, "Compatible")
    assert compatible["status"] == "False"
    assert compatible["reason"] == "UnknownCheckConfig"
    assert "no-such-config" in compatible["message"], \
        "the message must name the configId to fix, not just report a mismatch"

    gr.delete("aiguardrail", "unknown-config", provider_namespace)


def test_provider_without_auth_publishes_no_endpoint_and_is_not_ready(
    namespace_factory, suffix,
):
    """Without the annotation TrustyAI publishes no endpoint, which is all readiness reads."""
    namespace = namespace_factory(f"e2e-guardrails-noauth-{suffix}")
    gr.copy_default_configmap(namespace)
    gr.apply(gr.provider_manifest(namespace, enable_auth=False))
    gr.apply(gr.policy_manifest(namespace))

    status = gr.wait_for_accepted_reason(namespace, "ProviderNotReady")
    gr.assert_binding_withdrawn(status)

    policy = gr.get("aiguardrail", "policy", namespace)
    ready = gr.condition(policy, "ProviderReady")
    assert ready["status"] == "False"
    assert ready["reason"] == "EndpointDiscoveryUnavailable"


def test_deleting_the_provider_withdraws_the_accepted_binding(
    namespace_factory, suffix,
):
    """Its own provider: the binding only withdraws once that provider is gone."""
    namespace = namespace_factory(f"e2e-guardrails-deleted-{suffix}")
    gr.copy_default_configmap(namespace)
    gr.apply(gr.provider_manifest(namespace))
    gr.wait_for_provider_endpoint(namespace)

    gr.apply(gr.policy_manifest(namespace))
    gr.assert_binding_published(
        gr.wait_for_accepted_reason(namespace, "PolicyAccepted")
    )

    gr.delete("nemoguardrails", "nemo", namespace)

    # A consumer gating on bindingRevision would otherwise keep compiling
    # against a provider that no longer exists.
    gr.assert_binding_withdrawn(
        gr.wait_for_accepted_reason(namespace, "ProviderNotFound")
    )