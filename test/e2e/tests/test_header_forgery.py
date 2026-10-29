"""E2E for the gateway's refusal of client-supplied trusted-context headers.

Authorino resolves identity, subscription and guardrail selection after
authentication, then injects them as headers upstream reads as settled facts.
A client able to set them itself picks its own identity, its own rate-limit
bucket, or which guardrail checks run on its prompt.

maas-controller already unit-tests the predicates it writes into the AuthPolicy
(TestBuildGatewayAuthPolicySpec_DenyClientIdentityHeaders). That shows the
policy was written, not that it is enforced: Envoy lowercases header names
before Authorino evaluates them, and the guardrail rule is a prefix match no
unit test drives with a real request.
"""
import pytest
import requests
from test_helper import _request_with_gateway_retry

# Its own group, not one of the mutable shared-state ones: these tests only
# read, so they need neither a per-worker tenant nor the serial pass.
pytestmark = [pytest.mark.xdist_group("header_forgery"), pytest.mark.security]

# Everything Authorino injects after auth. Upstream cannot tell an injected
# value from one the caller sent, so the request is refused rather than
# overwritten -- an overwrite only holds for headers someone remembered to list.
INJECTED_IDENTITY_HEADERS = [
    "x-maas-username",
    "x-maas-group",
    "x-maas-keyname",
    "x-maas-subscription-rate-limit-id",
    "x-maas-organization-id",
    "x-maas-cost-center",
    "x-maas-identity",
    "x-maas-admin-scope",
]

# Guardrail selection is one header per check, so the policy refuses the whole
# prefix: a check the catalog gains later must not arrive as a new hole. The
# mixed-case spelling rides on Envoy lowercasing names before CEL sees them,
# which is the half of the rule no unit test can reach.
FORGED_GUARDRAIL_HEADERS = [
    "x-aigateway-guardrail-content-safety",
    "x-aigateway-guardrail-no-such-check",
    "X-AIGateway-Guardrail-Content-Safety",
]


def _probe(base_url: str, headers: dict, forged: dict | None = None):
    """GET /v1/models, optionally carrying forged headers.

    /v1/models needs no model and no subscription of its own, and the deny rule
    carries no "when", so it applies to this request like any other.
    """
    sent = dict(headers)
    if forged:
        sent.update(forged)
    return _request_with_gateway_retry(requests.get, f"{base_url}/v1/models", headers=sent)


def _assert_refused(response):
    """A refusal has to come from the policy, not from a gateway still loading.

    An empty 401/403 is what Envoy returns before the AuthPolicy is in place.
    Asserting the error body keeps a gateway that is merely unready from
    reading as a policy that is working.
    """
    assert response.status_code == 403, (
        f"a forged header must be refused, got {response.status_code}: "
        f"{response.text[:300]}"
    )
    error = response.json().get("error", {})
    assert error.get("type") == "authorization_error", (
        f"the refusal did not come from the AuthPolicy: {response.text[:300]}"
    )


@pytest.fixture(scope="module", autouse=True)
def gateway_serves_the_unforged_request(maas_api_base_url, headers):
    """The control: the same request without a forged header is served.

    Without it every assertion below passes just as well against a gateway that
    is refusing everything.
    """
    response = _probe(maas_api_base_url, headers)
    assert response.status_code == 200, (
        f"the unforged request must be served: {response.status_code} "
        f"{response.text[:300]}"
    )


@pytest.mark.parametrize("header", INJECTED_IDENTITY_HEADERS)
def test_client_supplied_identity_header_is_refused(maas_api_base_url, headers, header):
    _assert_refused(_probe(maas_api_base_url, headers, {header: "forged"}))


@pytest.mark.parametrize("header", FORGED_GUARDRAIL_HEADERS)
def test_client_supplied_guardrail_selection_is_refused(maas_api_base_url, headers, header):
    """Choosing your own checks is choosing which rails run on your prompt."""
    _assert_refused(_probe(maas_api_base_url, headers, {header: "true"}))