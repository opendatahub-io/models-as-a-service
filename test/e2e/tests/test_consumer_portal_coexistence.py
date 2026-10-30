"""
Consumer Portal Coexistence E2E Tests — RHAI-10609
===================================================

Verify that the consumer portal routing + AuthPolicy (RHAI-10608, deployed by
ai-gateway-controller) does not change authentication on every other route:

  - API key (sk-oai-*) on a model path still works; invalid keys still fail.
  - External OIDC JWT against the API still works.
  - Kubernetes service-account tokens (TokenReview) still work.
  - Portal session cookies and forged x-maas-* identity headers on model
    paths are ignored — the portal policy must not leak into the MaaS
    gateway chain.
  - The portal itself: no session cookie → 302 to /oauth2/start;
    /oauth2/* stays public.

All tests skip when the portal HTTPRoute is absent (older controller image,
or consumerPortal not enabled), so the file is safe in every existing lane.

Prerequisites: same as the Grid OIDC tests (EXTERNAL_OIDC=true, OIDC_* env
vars, Keycloak test realms) plus a controller image carrying RHAI-10608.
"""

from __future__ import annotations

import os
import uuid
from urllib.parse import parse_qs, unquote, urlparse

import pytest
import requests

from conftest import TLS_VERIFY
from grid_oidc_helpers import request_grid_token
from test_helper import (
    _create_api_key,
    _gateway_url,
    _get_cluster_token,
    _inference,
    _request_with_gateway_retry,
)

PORTAL_PATH = "/maas-portal"
OAUTH2_START_PATH = "/oauth2/start"
PROXY_COOKIE_NAME = "_maas_consumer_proxy"
FORGED_IDENTITY_HEADERS = {
    "x-maas-username": "evil_hacker",
    "x-maas-group": '["system:authenticated"]',
    "x-maas-orgid": "42c30000000000000000000000000000",
    "x-maas-roles": '["org-admin"]',
    "x-forwarded-access-token": "forged.access.token",
}


@pytest.fixture(scope="module", autouse=True)
def require_consumer_portal():
    """Skip the module unless the portal route answers.

    The portal path answers 302 → /oauth2/start once RHAI-10608 is deployed.
    A 404 means no HTTPRoute matches /maas-portal (portal disabled or an
    older controller image), which is a skip, not a failure. Any other
    outcome — including connection errors — must fail, not skip: silently
    skipping these auth-regression tests on a transient blip would hide
    exactly the regressions they exist to catch.
    """
    if os.environ.get("EXTERNAL_OIDC", "").lower() != "true":
        pytest.skip("EXTERNAL_OIDC is not true")
    try:
        response = requests.get(
            f"{_gateway_url()}{PORTAL_PATH}",
            allow_redirects=False,
            timeout=30,
            verify=TLS_VERIFY,
        )
    except requests.exceptions.RequestException as exc:
        pytest.fail(f"consumer portal probe failed: {exc}")
    if response.status_code == 404:
        pytest.skip("consumer portal route is not deployed")


pytestmark = [
    pytest.mark.nightly,
    pytest.mark.grid_oidc,
    pytest.mark.skipif(
        os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
        reason="EXTERNAL_OIDC is not true",
    ),
    pytest.mark.xdist_group("external"),
]


@pytest.fixture(scope="module")
def api_key() -> str:
    """Mint a plain cluster-token API key (the pre-portal baseline flow)."""
    return _create_api_key(_get_cluster_token(), name=f"e2e-portal-coexistence-{uuid.uuid4().hex[:8]}")


class TestPortalAuthBoundary:
    """The portal route itself behaves per the AuthPolicy from RHAI-10608."""

    def test_portal_redirects_to_sign_in_without_cookie(self):
        """No session cookie → 302 to /oauth2/start with rd=<path>, no-store."""
        response = requests.get(
            f"{_gateway_url()}{PORTAL_PATH}",
            allow_redirects=False,
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 302, (
            f"Expected 302 to sign-in, got {response.status_code} body_bytes={len(response.content)}"
        )
        location = urlparse(response.headers.get("Location", ""))
        assert location.path == OAUTH2_START_PATH, (
            f"Expected redirect to {OAUTH2_START_PATH}, got Location={location.geturl()!r}"
        )
        # The sign-in flow must return the user to the path they requested.
        assert unquote(parse_qs(location.query).get("rd", [""])[0]) == PORTAL_PATH, (
            f"Expected rd={PORTAL_PATH} in redirect, got Location={location.geturl()!r}"
        )
        assert response.headers.get("Cache-Control") == "no-store"

    def test_oauth2_start_is_public(self):
        """/oauth2/* must stay anonymous — no session required to start sign-in."""
        response = requests.get(
            f"{_gateway_url()}{OAUTH2_START_PATH}",
            allow_redirects=False,
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code not in (401, 403), (
            f"/oauth2/start must be public, got {response.status_code} "
            f"body_bytes={len(response.content)}"
        )


class TestNonPortalAuthUnchanged:
    """Non-portal paths keep the MaaS gateway chain: API key → OIDC → TokenReview."""

    def test_api_key_model_access_unchanged(self, api_key: str):
        response = _inference(api_key)
        assert response.status_code == 200, (
            f"API key inference on model path failed after portal deployment: "
            f"{response.status_code} {response.text}"
        )

    def test_invalid_api_key_still_rejected(self):
        response = _inference("sk-oai-invalid-invalid-invalid")
        # The gateway answers 403 for an unknown key on the model path; both
        # 401 and 403 satisfy the coexistence contract (still rejected).
        assert response.status_code in (401, 403), (
            f"Invalid API key must be rejected, got {response.status_code}"
        )

    def test_external_oidc_jwt_still_accepted(self, maas_api_base_url: str):
        """A Keycloak JWT keeps authenticating against the API endpoints."""
        token = request_grid_token("alice_lead")
        response = _request_with_gateway_retry(
            requests.get,
            f"{maas_api_base_url}/v1/models",
            headers={"Authorization": f"Bearer {token}"},
            timeout=30,
        )
        assert response.status_code == 200, (
            f"OIDC JWT on /v1/models failed after portal deployment: "
            f"{response.status_code} {response.text}"
        )

    def test_serviceaccount_token_still_accepted(self, maas_api_base_url: str):
        """A Kubernetes SA token (TokenReview path) keeps minting API keys."""
        suffix = uuid.uuid4().hex[:8]
        sa_token = _get_cluster_token()
        key = _create_api_key(sa_token, name=f"e2e-portal-coexistence-sa-{suffix}")
        response = _inference(key)
        assert response.status_code == 200, (
            f"SA-token-minted API key inference failed: {response.status_code} {response.text}"
        )

    def test_proxy_cookie_ignored_on_model_path(self, api_key: str):
        """A portal session cookie must not change auth on non-portal paths."""
        control = _inference(api_key)
        assert control.status_code == 200, "control inference failed"
        response = _inference(
            api_key,
            extra_headers={"Cookie": f"{PROXY_COOKIE_NAME}=forged-session-value"},
        )
        assert response.status_code == 200, (
            f"Portal cookie on a model path changed the outcome: "
            f"{response.status_code} {response.text}"
        )

    def test_forged_identity_headers_ignored_on_model_path(self, api_key: str):
        """Client-supplied x-maas-* identity headers never grant identity.

        The gateway denies client-supplied identity headers on model paths
        (deny-client-identity-headers), the same contract TestOIDCHeaderInjection
        asserts on /v1/models. The control request proves the key itself works,
        so a 401/403 on the forged request means the headers were rejected,
        not that the key lost access.
        """
        control = _inference(api_key)
        assert control.status_code == 200, (
            f"Control inference failed: {control.status_code} {control.text}"
        )
        response = _inference(api_key, extra_headers=FORGED_IDENTITY_HEADERS)
        assert response.status_code in (401, 403), (
            f"Forged x-maas-* headers must be denied on a model path, "
            f"got {response.status_code}"
        )
