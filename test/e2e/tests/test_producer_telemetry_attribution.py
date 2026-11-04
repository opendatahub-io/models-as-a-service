"""Producer attribution E2E for MaaS Limitador telemetry."""

from __future__ import annotations

import json
import re
import subprocess
import time
import uuid
from typing import Any

import pytest

from multitenancy_helpers import (
    TENANT_CR_NAME,
    require_aitenant_crd,
    require_tenant_namespace_discovery,
)
from test_helper import (
    GATEWAY_NAMESPACE,
    _apply_cr,
    _create_api_key,
    _get_cluster_token,
    _get_cr,
    _inference,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
    _wait_for_subscription_inference_ready,
)
from worker_tenant_fixtures import (
    WorkerTenantContext,
    activate_worker_tenant,
    bootstrap_worker_tenant,
    build_worker_tenant_case,
    teardown_worker_tenant,
)

pytestmark = [pytest.mark.serial]

_LABEL_RE = re.compile(r'([a-zA-Z_][a-zA-Z0-9_]*)="((?:\\.|[^"])*)"')


def _patch_metrics_capture_user(namespace: str) -> None:
    result = subprocess.run(
        [
            "oc",
            "patch",
            "maastenantconfig",
            TENANT_CR_NAME,
            "-n",
            namespace,
            "--type=merge",
            "-p",
            json.dumps(
                {"spec": {"telemetry": {"metrics": {"captureUser": True}}}}
            ),
        ],
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )
    assert result.returncode == 0, result.stderr or result.stdout


def _telemetry_policy_labels(policy_name: str) -> dict[str, Any]:
    policy = _get_cr("telemetrypolicy", policy_name, namespace=GATEWAY_NAMESPACE)
    assert policy is not None, f"TelemetryPolicy {policy_name} was not reconciled"
    labels = policy.get("spec", {}).get("metrics", {}).get("default", {}).get("labels")
    assert isinstance(labels, dict), f"invalid TelemetryPolicy labels: {labels!r}"
    return labels


def _cel_string(value: Any) -> str:
    try:
        decoded = json.loads(value)
    except (TypeError, json.JSONDecodeError):
        return str(value)
    return decoded if isinstance(decoded, str) else str(value)


def _wait_for_telemetry_policy_labels(policy_name: str, timeout: int = 120) -> dict[str, Any]:
    deadline = time.monotonic() + timeout
    last_error = "TelemetryPolicy was not observed"
    while time.monotonic() < deadline:
        try:
            labels = _telemetry_policy_labels(policy_name)
            if labels.get("tenant_name") and labels.get("subscription"):
                return labels
            last_error = f"producer scope labels are missing: {labels!r}"
        except AssertionError as error:
            last_error = str(error)
        time.sleep(3)
    pytest.fail(last_error)


def _wait_for_telemetry_policy_user(policy_name: str, timeout: int = 120) -> None:
    labels = _wait_for_telemetry_policy_labels(policy_name)
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if labels.get("user") == "auth.identity.userid":
            return
        labels = _telemetry_policy_labels(policy_name)
        time.sleep(3)
    pytest.fail(f"user label is missing or invalid: {labels.get('user')!r}")


def _apply_governance(
    context: WorkerTenantContext,
    subscriptions: dict[str, str],
) -> None:
    _apply_cr(
        {
            "apiVersion": "maas.opendatahub.io/v1alpha1",
            "kind": "MaaSAuthPolicy",
            "metadata": {
                "name": context.policy_name,
                "namespace": context.tenant_namespace,
            },
            "spec": {
                "modelRefs": [
                    {
                        "name": context.model_ref,
                        "namespace": context.model_namespace,
                    }
                ],
                "subjects": {"groups": [{"name": "system:authenticated"}]},
            },
        }
    )
    for subscription_name, organization_id in subscriptions.items():
        _apply_cr(
            {
                "apiVersion": "maas.opendatahub.io/v1alpha1",
                "kind": "MaaSSubscription",
                "metadata": {
                    "name": subscription_name,
                    "namespace": context.tenant_namespace,
                },
                "spec": {
                    "organization": {"id": organization_id},
                    "owner": {"groups": [{"name": "system:authenticated"}]},
                    "modelRefs": [
                        {
                            "name": context.model_ref,
                            "namespace": context.model_namespace,
                            "tokenRateLimits": [
                                {"limit": 1000, "window": "1m"}
                            ],
                        }
                    ],
                },
            }
        )

    _wait_for_maas_auth_policy_phase(
        context.policy_name,
        namespace=context.tenant_namespace,
        require_enforced=False,
    )
    for subscription_name in subscriptions:
        _wait_for_maas_subscription_phase(
            subscription_name,
            namespace=context.tenant_namespace,
        )
        _wait_for_subscription_inference_ready(
            subscription_name,
            context.model_ref,
            namespace=context.tenant_namespace,
            model_namespace=context.model_namespace,
        )


def _limitador_metric_labels() -> list[dict[str, str]]:
    result = subprocess.run(
        ["oc", "get", "limitador", "-A", "-o", "json"],
        capture_output=True,
        text=True,
        timeout=30,
        check=False,
    )
    if result.returncode != 0:
        pytest.skip(f"Limitador CR not readable: {result.stderr.strip()}")
    limitadors = json.loads(result.stdout).get("items") or []
    if not limitadors:
        pytest.skip("No Limitador CR is installed")

    labels: list[dict[str, str]] = []
    for limitador in limitadors:
        metadata = limitador["metadata"]
        metrics = subprocess.run(
            [
                "oc",
                "get",
                "--raw",
                (
                    f"/api/v1/namespaces/{metadata['namespace']}/services/"
                    f"limitador-{metadata['name']}:8080/proxy/metrics"
                ),
            ],
            capture_output=True,
            text=True,
            timeout=30,
            check=False,
        )
        if metrics.returncode != 0:
            pytest.skip(f"Limitador metrics not reachable: {metrics.stderr.strip()}")
        for line in metrics.stdout.splitlines():
            if line.startswith(("authorized_hits{", "authorized_hits_total{")):
                labels.append(dict(_LABEL_RE.findall(line.split(" ", 1)[0])))
    return labels


def _has_scoped_series(
    series: list[dict[str, str]],
    expected: dict[str, str],
    *,
    require_user: bool = False,
) -> bool:
    for labels in series:
        if not all(labels.get(key) == value for key, value in expected.items()):
            continue
        if require_user and labels.get("user", "") == "":
            continue
        return True
    return False


def _wait_for_scoped_series(
    expected_a: dict[str, str],
    expected_b: dict[str, str],
    expected_unrelated: dict[str, str],
    timeout: int = 180,
) -> list[dict[str, str]]:
    deadline = time.monotonic() + timeout
    series: list[dict[str, str]] = []
    while time.monotonic() < deadline:
        series = _limitador_metric_labels()
        if (
            _has_scoped_series(series, expected_a, require_user=True)
            and _has_scoped_series(series, expected_b, require_user=True)
            and _has_scoped_series(series, expected_unrelated)
        ):
            return series
        time.sleep(5)
    pytest.fail(
        "Expected scoped authorized_hits series did not appear; "
        f"expected A={expected_a}, B={expected_b}, unrelated={expected_unrelated}, "
        f"observed={series}"
    )


def test_limitador_series_are_producer_scoped_and_isolated() -> None:
    require_aitenant_crd()
    require_tenant_namespace_discovery()

    suffix = uuid.uuid4().hex[:8]
    organization_a = f"e2e-org-a-{suffix}"
    organization_b = f"e2e-org-b-{suffix}"
    organization_unrelated = f"e2e-org-u-{suffix}"

    shared = build_worker_tenant_case(f"telshared{suffix}")
    unrelated = build_worker_tenant_case(f"telunrel{suffix}")
    subscription_a = f"{shared.subscription_name}-a"
    subscription_b = f"{shared.subscription_name}-b"
    subscription_unrelated = unrelated.subscription_name

    try:
        shared = bootstrap_worker_tenant(shared, baseline=False)
        unrelated = bootstrap_worker_tenant(unrelated, baseline=False)
        _patch_metrics_capture_user(shared.tenant_namespace)

        _apply_governance(
            shared,
            {subscription_a: organization_a, subscription_b: organization_b},
        )
        _apply_governance(unrelated, {subscription_unrelated: organization_unrelated})

        _wait_for_telemetry_policy_user(f"maas-telemetry-{shared.tenant_name}")
        shared_policy = _telemetry_policy_labels(
            f"maas-telemetry-{shared.tenant_name}"
        )
        unrelated_policy = _wait_for_telemetry_policy_labels(
            f"maas-telemetry-{unrelated.tenant_name}"
        )

        assert "cluster_id" not in shared_policy
        assert "cluster_id" not in unrelated_policy

        token = _get_cluster_token()
        with activate_worker_tenant(shared):
            key_a = _create_api_key(token, subscription=subscription_a)
            key_b = _create_api_key(token, subscription=subscription_b)
            response_a = _inference(
                key_a,
                path=f"/{shared.model_namespace}/{shared.model_ref}",
                model_name=f"e2e/{shared.model_ref}",
            )
            assert response_a.status_code == 200, response_a.text[:500]
            rejected_response = _inference(
                key_b,
                path=f"/{shared.model_namespace}/{shared.model_ref}",
                model_name=f"e2e/{shared.model_ref}",
                extra_headers={
                    "x-maas-organization-id": "forged-org",
                },
            )
            assert rejected_response.status_code == 403, rejected_response.text[:500]

            response_b = _inference(
                key_b,
                path=f"/{shared.model_namespace}/{shared.model_ref}",
                model_name=f"e2e/{shared.model_ref}",
                extra_headers={
                    "x-maas-subscription": "forged-subscription",
                    "x-maas-subscription-namespace": "forged-namespace",
                },
            )
            assert response_b.status_code == 200, response_b.text[:500]

        with activate_worker_tenant(unrelated):
            key_unrelated = _create_api_key(
                token, subscription=subscription_unrelated
            )
            response_unrelated = _inference(
                key_unrelated,
                path=f"/{unrelated.model_namespace}/{unrelated.model_ref}",
                model_name=f"e2e/{unrelated.model_ref}",
            )
            assert response_unrelated.status_code == 200, response_unrelated.text[:500]

        expected_a = {
            "tenant_name": shared.tenant_name,
            "tenant_namespace": shared.tenant_namespace,
            "subscription": subscription_a,
            "organization_id": organization_a,
        }
        expected_b = expected_a | {"subscription": subscription_b, "organization_id": organization_b}
        expected_unrelated = {
            "tenant_name": unrelated.tenant_name,
            "tenant_namespace": unrelated.tenant_namespace,
            "subscription": subscription_unrelated,
            "organization_id": organization_unrelated,
        }

        series = _wait_for_scoped_series(
            expected_a, expected_b, expected_unrelated
        )

        for labels in series:
            assert labels.get("organization_id") != "forged-org"
            assert labels.get("subscription") != "forged-subscription"
            organization_id = labels.get("organization_id", "")
            if organization_id in {organization_a, organization_b}:
                assert labels.get("tenant_name") == shared.tenant_name
                assert labels.get("organization_id") != "forged-org"
                assert labels.get("tenant_namespace") == shared.tenant_namespace
                assert "subscription_namespace" not in labels
                assert labels.get("user", "") != ""
            elif organization_id == organization_unrelated:
                assert labels.get("tenant_name") == unrelated.tenant_name
                assert labels.get("tenant_namespace") == unrelated.tenant_namespace
                assert "subscription_namespace" not in labels

    finally:
        try:
            teardown_worker_tenant(shared)
        finally:
            teardown_worker_tenant(unrelated)
