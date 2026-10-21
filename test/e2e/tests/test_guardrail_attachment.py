"""E2E for the guardrail attachment API as the cluster actually serves it.

maas-controller already unit-tests these rules against a schema it builds
in-process (TestMaaSSubscriptionGuardrailsAttachmentsAccepted and
TestMaaSSubscriptionGuardrailRefNamespaceIsPruned). What those cannot show is
that the CRDs installed on a cluster carry the field — generation, vendoring
and the deployment overlay all sit between the Go type and the live schema.

Only the attachment is covered here. Nothing yet reads these fields, so there
is no effective-policy behaviour to assert.
"""
import json
import uuid

import pytest

import guardrail_helpers as gr

pytestmark = [
    pytest.mark.xdist_group("guardrails"),
    pytest.mark.guardrails,
]

AIGUARDRAIL_CRD = gr.AIGUARDRAIL_CRD

# Every scope the attachment API spans, with the path its list lives on.
ATTACHMENT_SCOPES = [
    ("maassubscriptions.maas.opendatahub.io", "guardrails"),
    ("maastenantconfigs.maas.opendatahub.io", "guardrails"),
    ("aitenants.maas.opendatahub.io", "guardrails"),
    ("maasmodelrefs.maas.opendatahub.io", "guardrails"),
]


@pytest.fixture(scope="module", autouse=True)
def guardrail_opt_in():
    """Opt-in only: the attachment API needs no NeMo server.

    Whether the AIGuardrail CRD is installed is what the first test asserts, so
    it is deliberately not part of the gate.
    """
    gr.require_opt_in()


@pytest.fixture(scope="module")
def namespace():
    """A throwaway namespace that may hold MaaS tenant resources.

    Writing a subscription is what the round-trip scenarios assert, and the
    webhook only permits that in a namespace enabled for tenant resources.
    """
    name = f"e2e-guardrail-attach-{uuid.uuid4().hex[:6]}"
    gr.create_namespace(name)
    gr.enable_tenant_namespace(name)
    yield name
    gr.delete_namespace(name)


def subscription(name: str, namespace: str, guardrails=None, model_guardrails=None) -> dict:
    """A minimal subscription that satisfies the existing CEL rules."""
    model_ref = {"name": "granite-7b", "namespace": "model-ns", "unlimited": True}
    if model_guardrails is not None:
        model_ref["guardrails"] = model_guardrails
    spec = {
        "owner": {"groups": [{"name": "team"}]},
        "modelRefs": [model_ref],
    }
    if guardrails is not None:
        spec["guardrails"] = guardrails
    return {
        "apiVersion": "maas.opendatahub.io/v1alpha1",
        "kind": "MaaSSubscription",
        "metadata": {"name": name, "namespace": namespace},
        "spec": spec,
    }


def spec_properties(crd_name: str) -> dict:
    """Return spec.properties from the CRD's storage version, as installed."""
    result = gr.oc("get", "crd", crd_name, "-o", "json", check=False)
    if result.returncode != 0:
        # A fail, not a skip: an absent CRD is the thing the caller is here to
        # catch, and for aitenants and maasmodelrefs nothing else in this
        # module would notice it.
        raise AssertionError(f"cannot read CRD {crd_name}: {result.stderr.strip()}")
    crd = json.loads(result.stdout)
    for version in crd["spec"]["versions"]:
        if not version.get("storage"):
            continue
        schema = version["schema"]["openAPIV3Schema"]
        return schema["properties"]["spec"].get("properties", {})
    raise AssertionError(f"{crd_name} declares no storage version")


def test_aiguardrail_crd_is_installed():
    """The attachment references a policy kind the cluster has to serve."""
    assert gr.crd_installed(AIGUARDRAIL_CRD), (
        f"{AIGUARDRAIL_CRD} is missing; MaaS vendors it so an attachment can "
        "resolve to a real policy"
    )


@pytest.mark.parametrize("crd_name,field", ATTACHMENT_SCOPES)
def test_installed_crd_carries_the_attachment_field(crd_name, field):
    properties = spec_properties(crd_name)
    assert field in properties, (
        f"{crd_name} spec.{field} is missing from the installed schema — "
        "the API server will prune every attachment written to it"
    )


def test_subscription_accepts_attachments_at_both_scopes(namespace):
    """Subscription-wide and per-model attachments survive a real round trip."""
    name = "guardrail-attachment"
    gr.apply(subscription(
        name, namespace,
        guardrails=[{"ref": {"name": "application-safety-v1"}, "checks": ["subscription-check"]}],
        model_guardrails=[{"ref": {"name": "application-safety-v1"}, "checks": ["application-check"]}],
    ))

    stored = gr.get("maassubscription", name, namespace)["spec"]
    assert stored["guardrails"][0]["ref"]["name"] == "application-safety-v1"
    assert stored["guardrails"][0]["checks"] == ["subscription-check"]
    assert stored["modelRefs"][0]["guardrails"][0]["checks"] == ["application-check"]

    gr.delete("maassubscription", name, namespace)


def test_reference_namespace_is_pruned(namespace):
    """A policy always resolves in the tenant target namespace.

    The reference carries no namespace field, so a caller that supplies one must
    not end up with it stored — that would read as a cross-namespace bind the
    API never granted.
    """
    name = "guardrail-ref-namespace"
    gr.apply(subscription(name, namespace, guardrails=[{
        "ref": {"name": "application-safety-v1", "namespace": "somewhere-else"},
    }]))

    ref = gr.get("maassubscription", name, namespace)["spec"]["guardrails"][0]["ref"]
    assert "namespace" not in ref, f"ref.namespace should be pruned, got {ref}"

    gr.delete("maassubscription", name, namespace)


def test_attachment_list_is_bounded(namespace):
    """MaxItems=64 keeps one subscription from attaching an unbounded policy set."""
    too_many = [{"ref": {"name": f"policy-{i}"}} for i in range(65)]
    result = gr.oc(
        "apply", "-f", "-",
        input_text=json.dumps(subscription("guardrail-too-many", namespace, guardrails=too_many)),
        check=False,
    )

    assert result.returncode != 0, "65 attachments were accepted; MaxItems=64 is not enforced"
    assert "64" in result.stderr, f"unexpected rejection: {result.stderr}"


def test_check_names_must_be_dns_labels(namespace):
    """Check names index into AIGuardrail.spec.checks, which are DNS labels."""
    result = gr.oc(
        "apply", "-f", "-",
        input_text=json.dumps(subscription("guardrail-bad-check", namespace, guardrails=[{
            "ref": {"name": "application-safety-v1"},
            "checks": ["Not A Label"],
        }])),
        check=False,
    )

    assert result.returncode != 0, "an invalid check name was accepted"