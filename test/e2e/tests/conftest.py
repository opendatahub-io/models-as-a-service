from __future__ import annotations

import json
from typing import Any, Dict, Optional

import pytest
import subprocess
import requests
from kubernetes.dynamic import DynamicClient
from ocp_resources.resource import get_client
from simple_logger.logger import get_logger

from test.e2e.tests.utils import (
    MaasEnv,
    collect_infrastructure_diagnostics,
    detect_scheme_via_llmisvc,
    env_bool,
    env_int,
    env_str,
    host_from_ingress_domain,
    is_openshift_cluster,
    mint_maas_token,
    normalize_url,
    TestSummary
)

LOGGER = get_logger(name=__name__)


def pytest_addoption(parser: pytest.Parser) -> None:
    parser.addoption(
        "--model-path",
        action="store",
        default=None,
        help="Model inference path appended to model base URL (default: /v1/chat/completions).",
    )
    parser.addoption(
        "--request-payload",
        action="store",
        default=None,
        help="Custom JSON payload template for inference; supports ${MODEL_NAME}/${MODEL_ID} substitution.",
    )


@pytest.fixture(scope="session")
def admin_client() -> DynamicClient:
    return get_client()


@pytest.fixture(scope="session")
def maas_env(admin_client: DynamicClient) -> MaasEnv:
    if not is_openshift_cluster(admin_client=admin_client):
        raise AssertionError("This suite requires OpenShift (route.openshift.io API must exist).")

    insecure_http = env_bool("INSECURE_HTTP", default_value=False)

    explicit_host = env_str("HOST")
    host_value = explicit_host or host_from_ingress_domain(admin_client=admin_client)

    explicit_base_url = env_str("MAAS_API_BASE_URL")
    if explicit_base_url:
        maas_api_base_url = normalize_url(explicit_base_url)
    else:
        default_scheme = "http" if insecure_http else "https"
        inferred_scheme = detect_scheme_via_llmisvc(admin_client=admin_client)
        scheme = default_scheme if insecure_http else (inferred_scheme or default_scheme)
        maas_api_base_url = f"{scheme}://{host_value}/maas-api"

    env_obj = MaasEnv(
        insecure_http=insecure_http,
        host=host_value,
        maas_api_base_url=maas_api_base_url,
        maas_api_namespace=env_str("MAAS_API_NAMESPACE", "opendatahub") or "opendatahub",
        gateway_namespace=env_str("MAAS_GATEWAY_NAMESPACE", "openshift-ingress") or "openshift-ingress",
        gateway_name=env_str("MAAS_GATEWAY_NAME", "maas-default-gateway") or "maas-default-gateway",
        maas_api_route_name=env_str("MAAS_API_ROUTE_NAME", "maas-api-route") or "maas-api-route",
        authpolicy_namespace=env_str("MAAS_AUTHPOLICY_NAMESPACE", "openshift-ingress") or "openshift-ingress",
        authpolicy_name=env_str("MAAS_AUTHPOLICY_NAME", "gateway-auth-policy") or "gateway-auth-policy",
        tokenratelimitpolicy_namespace=env_str("MAAS_TOKENRLP_NAMESPACE", "openshift-ingress") or "openshift-ingress",
        tokenratelimitpolicy_name=env_str("MAAS_TOKENRLP_NAME", "gateway-token-rate-limits") or "gateway-token-rate-limits",
        request_ratelimitpolicy_namespace=env_str("MAAS_RLP_NAMESPACE", "openshift-ingress") or "openshift-ingress",
        request_ratelimitpolicy_name=env_str("MAAS_RLP_NAME", "gateway-rate-limits") or "gateway-rate-limits",
        rate_limit_request_count=env_int("RATE_LIMIT_TEST_COUNT", default_value=10),
        retry_wait_seconds=env_int("RETRY_WAIT_SECONDS", default_value=60),
        metrics_url=env_str("MAAS_METRICS_URL", None),
    )

    LOGGER.info(f"MAAS_API_BASE_URL={env_obj.maas_api_base_url}")
    return env_obj


@pytest.fixture(scope="session")
def request_session_http() -> requests.Session:
    http_session = requests.Session()
    http_session.verify = False
    http_session.headers.update({"Content-Type": "application/json"})
    return http_session


@pytest.fixture(scope="session")
def ocp_bearer_token() -> str:
    token_value = env_str("OCP_BEARER_TOKEN")
    if token_value:
        return token_value

    try:
        token_value = subprocess.check_output(["oc", "whoami", "-t"], text=True).strip()
    except Exception as exc:
        raise AssertionError(
            "Missing OCP_BEARER_TOKEN and failed to run `oc whoami -t`. "
            "Login first (`oc login ...`) or export OCP_BEARER_TOKEN."
        ) from exc

    if not token_value:
        raise AssertionError("`oc whoami -t` returned empty token")
    return token_value


@pytest.fixture(scope="session")
def maas_token(request_session_http: requests.Session, maas_env: MaasEnv, ocp_bearer_token: str) -> str:
    existing_token = env_str("TOKEN")
    if existing_token:
        return existing_token

    return mint_maas_token(
        http_session=request_session_http,
        maas_api_base_url=maas_env.maas_api_base_url,
        ocp_bearer_token=ocp_bearer_token,
        retry_wait_seconds=maas_env.retry_wait_seconds,
    )


@pytest.fixture(scope="session")
def model_path(pytestconfig: pytest.Config) -> str:
    return pytestconfig.getoption("--model-path") or "/v1/chat/completions"


@pytest.fixture(scope="session")
def request_payload_template(pytestconfig: pytest.Config) -> Optional[Dict[str, Any]]:
    raw_payload = pytestconfig.getoption("--request-payload")
    if not raw_payload:
        return None
    try:
        parsed = json.loads(raw_payload)
    except Exception as exc:
        raise AssertionError(f"--request-payload must be valid JSON. Got: {raw_payload!r}") from exc
    if not isinstance(parsed, dict):
        raise AssertionError("--request-payload must be a JSON object")
    return parsed


@pytest.fixture(scope="session")
def infrastructure_diagnostics(admin_client: DynamicClient, maas_env: MaasEnv) -> Dict[str, Any]:
    return collect_infrastructure_diagnostics(admin_client=admin_client, maas_env=maas_env)

@pytest.fixture(scope="session")
def test_summary(pytestconfig: pytest.Config) -> TestSummary:
    summary = TestSummary()
    pytestconfig._test_summary = summary  # type: ignore[attr-defined]
    return summary

def pytest_terminal_summary(terminalreporter, exitstatus: int, config: pytest.Config) -> None:
    if getattr(config, "_summary_printed", False):
        return
    config._summary_printed = True  # type: ignore[attr-defined]

    summary = getattr(config, "_test_summary", None)
    if not summary:
        return

    terminalreporter.write("\n\n=== Test Summary ===\n")
    terminalreporter.write(summary.render())
    terminalreporter.write("\n")
