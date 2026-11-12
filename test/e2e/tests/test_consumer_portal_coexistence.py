"""
Consumer Portal Coexistence E2E Tests — RHAI-10609, RHAI-10614, RHAI-10615
=========================================================================

Verify the consumer portal stack (deployed by ai-gateway-controller) end to
end, and that it does not change authentication on the MaaS model routes:

  - The portal route: no session cookie → 302 to /oauth2/start; /oauth2/start
    sends the browser to the identity provider.
  - A real browser sign-in (Keycloak login form → /oauth2/callback) yields a
    session that passes the portal AuthPolicy, reports the user's identity on
    /oauth2/userinfo, and is ended by /oauth2/sign_out.
  - The portal client's tokens carry the flattened MS-0007 identity contract
    (org_id / org_name / roles, plain groups) that the portal AuthPolicy reads.
  - On model paths a portal session grants nothing: the session alone is
    denied, a session plus an API key behaves exactly like the API key, and
    forged x-maas-* identity headers are rejected.

The other auth paths (API keys, OIDC JWTs) keep their own suites; in the OIDC
lane those run with the portal enabled, so they double as regression tests.

Prerequisites: same as the Grid OIDC tests (EXTERNAL_OIDC=true, OIDC_* env
vars, Keycloak test realms), plus the portal enablement from
test/e2e/scripts/deploy-maas-portal.sh (on by default when EXTERNAL_OIDC=true;
MAAS_PORTAL=false turns it off and skips this module).
"""

from __future__ import annotations

import base64
import json
import os
import re
import uuid
from html import unescape
from urllib.parse import parse_qs, unquote, urljoin, urlparse

import pytest
import requests

from conftest import TLS_VERIFY
from grid_inventory_fixtures import ORGANIZATION_A
from grid_oidc_helpers import required_grid_env
from multitenancy_helpers import wait_for_json
from test_helper import (
    MAAS_API_DEPLOYMENT_NAMESPACE,
    MODEL_NAME,
    MODEL_PATH,
    TIMEOUT,
    _create_api_key,
    _gateway_url,
    _get_cluster_token,
    _inference,
)

PORTAL_PATH = "/maas-portal"
PORTAL_ROUTE_NAME = "maas-consumer-portal"
OAUTH2_START_PATH = "/oauth2/start"
OAUTH2_USERINFO_PATH = "/oauth2/userinfo"
OAUTH2_SIGN_OUT_PATH = "/oauth2/sign_out"
PROXY_COOKIE_NAME = "_maas_consumer_proxy"
# Must match the tenant-a realm's maas-consumer-portal client and
# test/e2e/scripts/deploy-maas-portal.sh.
PORTAL_CLIENT_ID = os.environ.get("MAAS_PORTAL_CLIENT_ID", "maas-consumer-portal")
PORTAL_CLIENT_SECRET = os.environ.get("MAAS_PORTAL_CLIENT_SECRET", "maas-e2e-portal-client-secret")
# The auth proxy rejects sign-ins without a verified email, so the browser
# flow uses the realm's portal_user (the other test users have no email).
PORTAL_USER = "portal_user"
PORTAL_USER_EMAIL = "portal_user@example.com"
TEST_USER_PASSWORD = "letmein"
FORGED_IDENTITY_HEADERS = {
    "x-maas-username": "evil_hacker",
    "x-maas-group": '["system:authenticated"]',
    "x-maas-organization-id": "42c30000000000000000000000000000",
    "x-maas-roles": '["org-admin"]',
    "x-maas-identity": '{"source":"oidc","claims":{"sub":"evil_hacker","groups":["org-admin"]}}',
    "x-forwarded-id-token": "forged.id.token",
}
ORGANIZATION_CLAIMS = {"org_id", "org_name", "roles"}

pytestmark = [
    pytest.mark.nightly,
    pytest.mark.grid_oidc,
    pytest.mark.skipif(
        os.environ.get("EXTERNAL_OIDC", "").lower() != "true",
        reason="EXTERNAL_OIDC is not true",
    ),
    pytest.mark.skipif(
        os.environ.get("MAAS_PORTAL", os.environ.get("EXTERNAL_OIDC", "")).lower() != "true",
        reason="MAAS_PORTAL is not true (consumer portal enablement is off)",
    ),
    pytest.mark.xdist_group("external"),
]


@pytest.fixture(scope="module", autouse=True)
def require_consumer_portal():
    """Wait for the controller to publish the portal HTTPRoute.

    deploy-maas-portal.sh only provides the prerequisites; ai-gateway-controller
    creates the route on its next reconcile. The portal is enabled in this
    lane, so a route that never appears is a deployment failure, not a skip.
    """
    wait_for_json("httproute", PORTAL_ROUTE_NAME, MAAS_API_DEPLOYMENT_NAMESPACE, timeout=300)


def _decode_jwt_payload(token: str) -> dict:
    parts = token.split(".")
    assert len(parts) == 3, f"Invalid JWT format: expected 3 parts, got {len(parts)}"
    padding = "=" * (-len(parts[1]) % 4)
    return json.loads(base64.urlsafe_b64decode(parts[1] + padding))


def _audiences(claims: dict) -> list[str]:
    aud = claims.get("aud", [])
    return [aud] if isinstance(aud, str) else aud


def _request_portal_client_tokens(username: str) -> dict:
    """Password-grant the portal client and return the token response."""
    response = requests.post(
        required_grid_env("OIDC_TOKEN_URL"),
        data={
            "grant_type": "password",
            "client_id": PORTAL_CLIENT_ID,
            "client_secret": PORTAL_CLIENT_SECRET,
            "username": username,
            "password": TEST_USER_PASSWORD,
            "scope": "openid",
        },
        timeout=30,
        verify=TLS_VERIFY,
    )
    assert response.status_code == 200, (
        f"Keycloak token request failed for {username} on {PORTAL_CLIENT_ID}: "
        f"{response.status_code} {response.text}"
    )
    tokens = response.json()
    assert tokens.get("access_token") and tokens.get("id_token"), (
        f"Keycloak token response is missing access_token or id_token: {sorted(tokens)}"
    )
    return tokens


def _sign_in(username: str) -> requests.Session:
    """Drive the browser sign-in flow and return a session holding the portal cookie.

    /maas-portal/ → 302 /oauth2/start → 302 Keycloak login form → POST the
    credentials → 302 /oauth2/callback, which sets the proxy session cookie.
    """
    session = requests.Session()
    session.verify = TLS_VERIFY
    login_page = session.get(f"{_gateway_url()}{PORTAL_PATH}/", allow_redirects=True, timeout=30)
    form = re.search(r'<form[^>]*id="kc-form-login"[^>]*action="([^"]+)"', login_page.text)
    assert form, (
        f"Expected the Keycloak login form, landed on {login_page.url} "
        f"with status {login_page.status_code}"
    )
    callback = session.post(
        urljoin(login_page.url, unescape(form.group(1))),
        data={"username": username, "password": TEST_USER_PASSWORD, "credentialId": ""},
        allow_redirects=True,
        timeout=30,
    )
    assert any(cookie.name.startswith(PROXY_COOKIE_NAME) for cookie in session.cookies), (
        f"Sign-in did not set a {PROXY_COOKIE_NAME} cookie for {username}: "
        f"landed on {callback.url} with status {callback.status_code}"
    )
    return session


def _portal_cookie_header(session: requests.Session) -> str:
    return "; ".join(
        f"{cookie.name}={cookie.value}"
        for cookie in session.cookies
        if cookie.name.startswith(PROXY_COOKIE_NAME)
    )


@pytest.fixture(scope="module")
def portal_session() -> requests.Session:
    return _sign_in(PORTAL_USER)


@pytest.fixture(scope="module")
def api_key() -> str:
    """Mint a plain cluster-token API key (the pre-portal baseline flow)."""
    return _create_api_key(_get_cluster_token(), name=f"e2e-portal-coexistence-{uuid.uuid4().hex[:8]}")


class TestPortalAuthBoundary:
    """The portal route itself behaves per the portal AuthPolicy."""

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

    def test_oauth2_start_redirects_to_identity_provider(self):
        """/oauth2/start is public and hands the browser to the OIDC issuer."""
        response = requests.get(
            f"{_gateway_url()}{OAUTH2_START_PATH}",
            allow_redirects=False,
            timeout=30,
            verify=TLS_VERIFY,
        )
        assert response.status_code == 302, (
            f"/oauth2/start must redirect to the identity provider, got "
            f"{response.status_code} body_bytes={len(response.content)}"
        )
        location = response.headers.get("Location", "")
        issuer = required_grid_env("OIDC_ISSUER_URL").rstrip("/")
        assert location.startswith(f"{issuer}/protocol/openid-connect/auth"), (
            f"Expected a redirect to {issuer}'s authorization endpoint, got Location={location!r}"
        )
        assert parse_qs(urlparse(location).query).get("client_id") == [PORTAL_CLIENT_ID], (
            f"Expected client_id={PORTAL_CLIENT_ID} in the authorization request, got Location={location!r}"
        )


class TestPortalSignedIn:
    """A real browser sign-in produces a working portal session."""

    def test_signed_in_request_passes_the_portal_auth_policy(self, portal_session: requests.Session):
        """With a session cookie the request is forwarded instead of redirected.

        The e2e maas-portal backend is a placeholder that serves nothing, so a
        forwarded request ends in a gateway upstream error; what matters is
        that the AuthPolicy no longer redirects or denies it.
        """
        response = portal_session.get(f"{_gateway_url()}{PORTAL_PATH}/", allow_redirects=False, timeout=30)
        assert response.status_code not in (302, 401, 403), (
            f"A signed-in request must pass the portal AuthPolicy, got {response.status_code} "
            f"Location={response.headers.get('Location')!r}"
        )

    def test_userinfo_reports_the_signed_in_identity(self, portal_session: requests.Session):
        """The session carries the user's email and plain groups from the portal client's tokens."""
        response = portal_session.get(f"{_gateway_url()}{OAUTH2_USERINFO_PATH}", allow_redirects=False, timeout=30)
        assert response.status_code == 200, (
            f"{OAUTH2_USERINFO_PATH} failed for a signed-in session: {response.status_code} {response.text}"
        )
        userinfo = response.json()
        assert userinfo.get("email") == PORTAL_USER_EMAIL, f"Unexpected userinfo: {userinfo}"
        assert "Engineering" in userinfo.get("groups", []), (
            f"Expected the plain Engineering group in userinfo, got {userinfo}"
        )

    def test_sign_out_ends_the_session(self):
        """/oauth2/sign_out clears the session cookie; the portal redirects to sign-in again."""
        session = _sign_in(PORTAL_USER)
        response = session.get(f"{_gateway_url()}{OAUTH2_SIGN_OUT_PATH}", allow_redirects=False, timeout=30)
        assert response.status_code == 302, (
            f"Expected {OAUTH2_SIGN_OUT_PATH} to redirect, got {response.status_code} {response.text}"
        )
        assert not _portal_cookie_header(session), (
            f"{OAUTH2_SIGN_OUT_PATH} must expire the {PROXY_COOKIE_NAME} cookie, "
            f"Set-Cookie={response.headers.get('Set-Cookie')!r}"
        )
        portal = session.get(f"{_gateway_url()}{PORTAL_PATH}", allow_redirects=False, timeout=30)
        assert portal.status_code == 302, (
            f"After sign-out the portal must redirect to sign-in, got {portal.status_code}"
        )
        assert urlparse(portal.headers.get("Location", "")).path == OAUTH2_START_PATH


class TestPortalIdentityContract:
    """The portal client's tokens carry the flattened MS-0007 identity contract."""

    def test_id_token_carries_flat_organization_claims(self):
        """The portal AuthPolicy reads org_id / roles from the ID token on the session path."""
        claims = _decode_jwt_payload(_request_portal_client_tokens(PORTAL_USER)["id_token"])
        assert claims.get("azp") == PORTAL_CLIENT_ID, f"Unexpected azp: {claims.get('azp')!r}"
        assert PORTAL_CLIENT_ID in _audiences(claims), f"Unexpected aud: {claims.get('aud')!r}"
        assert claims.get("org_id") == ORGANIZATION_A
        assert claims.get("org_name") == "coke"
        assert claims.get("roles") == ["users"]
        assert claims.get("groups") == ["Engineering"]
        assert claims.get("email") == PORTAL_USER_EMAIL

    def test_access_token_meets_the_maas_api_contract(self):
        """The access token forwarded to the dashboard must satisfy maas-api's strict contract."""
        claims = _decode_jwt_payload(_request_portal_client_tokens(PORTAL_USER)["access_token"])
        assert claims.get("azp") == PORTAL_CLIENT_ID, f"Unexpected azp: {claims.get('azp')!r}"
        assert "maas-api" in _audiences(claims), f"Unexpected aud: {claims.get('aud')!r}"
        assert claims.get("token_use") == "access"
        assert claims.get("org_id") == ORGANIZATION_A
        assert claims.get("roles") == ["users"]
        assert claims.get("groups") == ["Engineering"]

    def test_tokens_omit_organization_claims_without_an_organization(self):
        """Absent organization attributes produce no org claims (the policy then sends ""/[])."""
        tokens = _request_portal_client_tokens("grid_no_org")
        for kind in ("id_token", "access_token"):
            claims = _decode_jwt_payload(tokens[kind])
            assert ORGANIZATION_CLAIMS.isdisjoint(claims), (
                f"{kind} for grid_no_org must not carry {sorted(ORGANIZATION_CLAIMS & claims.keys())}"
            )


class TestModelPathIgnoresPortalSession:
    """A portal session never authorizes or changes a request on a model path."""

    def test_portal_session_alone_is_denied_on_model_path(self, portal_session: requests.Session):
        response = requests.post(
            f"{_gateway_url()}{MODEL_PATH}/v1/completions",
            headers={"Cookie": _portal_cookie_header(portal_session), "Content-Type": "application/json"},
            json={"model": MODEL_NAME, "prompt": "Hello", "max_tokens": 3},
            timeout=TIMEOUT,
            verify=TLS_VERIFY,
        )
        assert response.status_code in (401, 403), (
            f"A portal session alone must not authorize model inference, got {response.status_code}"
        )

    def test_portal_session_with_api_key_behaves_like_the_api_key(
        self, portal_session: requests.Session, api_key: str
    ):
        """SSO then API key (MS-0007 §API access): the API key decides the outcome."""
        control = _inference(api_key)
        assert control.status_code == 200, (
            f"Control inference failed: {control.status_code} {control.text}"
        )
        response = _inference(api_key, extra_headers={"Cookie": _portal_cookie_header(portal_session)})
        assert response.status_code == 200, (
            f"A portal session cookie changed the API key outcome on a model path: "
            f"{response.status_code} {response.text}"
        )

    def test_forged_identity_headers_rejected_on_model_path(self, api_key: str):
        """Client-supplied x-maas-* identity headers are rejected, never trusted.

        The gateway denies client-supplied identity headers on model paths
        (deny-client-identity-headers). The control request proves the key
        itself works, so a 401/403 on the forged request means the headers
        were rejected, not that the key lost access.
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
