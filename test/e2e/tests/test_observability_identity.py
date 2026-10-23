"""Opt-in reproduction of organization attribution through the MaaS gateway.

Requires tenant discovery, a simulator-capable MaaS deployment, and Limitador
telemetry. Enable with E2E_OBSERVABILITY_IDENTITY=true. Evidence is written under
ARTIFACT_DIR (default: reports/), including on setup or assertion failures.
"""

import json
import os
import re
import socket
import subprocess
import time
from pathlib import Path
from urllib.parse import urlencode

import pytest
import test_helper as helper
from multitenancy_helpers import (
    GATEWAY_NAMESPACE,
    INFRA_NAMESPACE,
    TENANT_CR_NAME,
    response_summary,
    wait_for_gateway_authpolicy_ready,
)
from worker_tenant_fixtures import (
    activate_worker_tenant,
    bootstrap_worker_tenant,
    build_worker_tenant_case,
    teardown_worker_tenant,
    xdist_worker_suffix,
)

pytestmark = [
    pytest.mark.xdist_group("observability_identity"),
    pytest.mark.skipif(
        os.environ.get("E2E_OBSERVABILITY_IDENTITY", "").lower() != "true",
        reason="Set E2E_OBSERVABILITY_IDENTITY=true to run the telemetry reproduction",
    ),
]

ORG_A = "11111111-1111-4111-8111-111111111111"
ORG_B = "22222222-2222-4222-8222-222222222222"
LEGACY_ORG = "33333333-3333-4333-8333-333333333333"
REQUESTS_PER_SUBSCRIPTION = 3
METRICS_TIMEOUT = 60
_SAMPLE = re.compile(r"^(authorized_(?:hits|calls)(?:_total)?)\{(.*)\}\s+(\S+)")
_LABEL = re.compile(r'([a-zA-Z_][a-zA-Z0-9_]*)="((?:\\.|[^"\\])*)"')
_EVIDENCE_LABELS = {
    "subscription",
    "organization_id",
    "model",
    "limitador_namespace",
    "grid_tenant_id",
    "tenant_id",
}


def _oc_json(*args):
    result = helper._run_oc(["oc", *args, "-o", "json"])
    assert result.returncode == 0, (
        f"oc {' '.join(args)} failed: {result.stderr.strip()}"
    )
    return json.loads(result.stdout)


def _resource(kind, name, namespace):
    resource = _oc_json("get", kind, name, "-n", namespace)
    return {
        "kind": resource["kind"],
        "name": resource["metadata"]["name"],
        "namespace": resource["metadata"].get("namespace"),
        "generation": resource["metadata"].get("generation"),
        "spec": resource.get("spec", {}),
        "status": resource.get("status", {}),
    }


def _limitador_source():
    candidates = _oc_json("get", "limitador", "-A")["items"]
    selected = os.environ.get("E2E_IDENTITY_LIMITADOR", "")
    if selected:
        candidates = [
            item
            for item in candidates
            if f"{item['metadata']['namespace']}/{item['metadata']['name']}" == selected
        ]
    assert len(candidates) == 1, (
        "Expected one Limitador; set E2E_IDENTITY_LIMITADOR=namespace/name "
        f"to choose explicitly (found {len(candidates)})"
    )
    metadata = candidates[0]["metadata"]
    namespace = metadata["namespace"]
    service = _oc_json(
        "get", "service", f"limitador-{metadata['name']}", "-n", namespace
    )
    ports = [port for port in service["spec"]["ports"] if port["port"] == 8080]
    assert len(ports) == 1, (
        "Limitador service must expose its metrics endpoint on port 8080"
    )
    return {
        "namespace": namespace,
        "service": service["metadata"]["name"],
        "port": ports[0]["port"],
    }


def _metrics_response(source, endpoint="/metrics"):
    path = (
        f"/api/v1/namespaces/{source['namespace']}/services/"
        f"{source['service']}:{source['port']}/proxy{endpoint}"
    )
    result = helper._run_oc(["oc", "get", "--raw", path])
    assert result.returncode == 0, (
        f"Cannot read metrics from {source}: {result.stderr.strip()}"
    )
    return result.stdout


def _source_metrics(source, subscriptions):
    samples = []
    for line in _metrics_response(source).splitlines():
        match = _SAMPLE.match(line)
        if not match:
            continue
        labels = {
            key: json.loads(f'"{value}"')
            for key, value in _LABEL.findall(match[2])
            if key in _EVIDENCE_LABELS
        }
        if labels.get("subscription") in subscriptions:
            samples.append(
                {"metric": match[1], "labels": labels, "value": float(match[3])}
            )
    return {"observed_at": time.time(), "samples": samples}


def _exported_sources():
    """Optionally verify both sides of the collector-to-ACM scrape boundary."""
    variables = {
        "collector": "E2E_IDENTITY_COLLECTOR",
        "scrape": "E2E_IDENTITY_PROMETHEUS",
    }
    if not any(os.environ.get(variable) for variable in variables.values()):
        return None
    sources = {}
    for stage, variable in variables.items():
        value = os.environ.get(variable, "")
        match = re.fullmatch(r"([a-z0-9.-]+)/([a-z0-9.-]+):([0-9]+)", value)
        assert match, f"Set {variable}=namespace/service:port for export verification"
        sources[stage] = {
            "namespace": match[1],
            "service": match[2],
            "port": int(match[3]),
        }
    return sources


def _scraped_metrics(source, subscriptions):
    names = "|".join(re.escape(name) for name in subscriptions)
    query = (
        f'{{__name__=~"authorized_(hits|calls)(_total)?",subscription=~{json.dumps(names)}'
        + "}"
    )
    response = json.loads(
        _metrics_response(
            source,
            "/api/v1/query?" + urlencode({"query": query}),
        )
    )
    assert response["status"] == "success", "Prometheus query failed"
    return {
        "observed_at": time.time(),
        "samples": [
            {
                "metric": item["metric"]["__name__"],
                "labels": {
                    key: value
                    for key, value in item["metric"].items()
                    if key in _EVIDENCE_LABELS
                },
                "value": float(item["value"][1]),
            }
            for item in response["data"]["result"]
        ],
    }


def _export_snapshot(sources, subscriptions):
    return {
        "collector": _source_metrics(sources["collector"], subscriptions),
        "scrape": _scraped_metrics(sources["scrape"], subscriptions),
    }


def _hits(snapshot, subscription):
    return sum(
        sample["value"]
        for sample in snapshot["samples"]
        if sample["metric"] in {"authorized_hits", "authorized_hits_total"}
        and sample["labels"]["subscription"] == subscription
    )


def _write_evidence(evidence, directory):
    directory.mkdir(parents=True, exist_ok=True)
    destination = directory / "evidence.json"
    destination.write_text(json.dumps(evidence, indent=2) + "\n")
    return destination


@pytest.fixture(scope="module", autouse=True)
def _verify_gateway_dns(organization_case):
    """Check our newly provisioned route instead of a pre-existing GATEWAY_HOST."""
    context, _, evidence, directory = organization_case
    try:
        socket.getaddrinfo(context.route_host, 443)
    except OSError as exc:
        evidence["outcome"] = "setup_failed"
        evidence["failure"] = str(exc)
        _write_evidence(evidence, directory)
        raise


@pytest.fixture(scope="module")
def organization_case():
    """Own one gateway/model and two explicitly organization-bound subscriptions."""
    context = build_worker_tenant_case(f"oid-{xdist_worker_suffix()}")
    directory = (
        Path(os.environ.get("ARTIFACT_DIR", "reports"))
        / f"observability-identity-{context.suffix}"
    )
    subscriptions = {
        f"identity-a-{context.suffix}": ORG_A,
        f"identity-b-{context.suffix}": ORG_B,
    }
    evidence = {
        "started_at": time.time(),
        "expected_organizations": subscriptions,
        "legacy_decoy": LEGACY_ORG,
        "requests_per_subscription": REQUESTS_PER_SUBSCRIPTION,
        "requests": [],
        "outcome": "setup",
        "context": {
            "tenant": context.tenant_name,
            "namespace": context.tenant_namespace,
            "gateway": context.gateway_name,
            "model": context.model_ref,
            "model_namespace": context.model_namespace,
        },
    }
    key_ids = []
    keys = {}
    token = None
    provisioning_started = False
    _write_evidence(evidence, directory)
    print(
        f"[observability_identity] evidence: {(directory / 'evidence.json').resolve()}"
    )
    try:
        source = _limitador_source()
        evidence["metrics_source"] = source
        exported_sources = _exported_sources()
        if exported_sources:
            evidence["exported_metrics"] = {
                "sources": exported_sources,
                "expected_tenant_id": os.environ.get("E2E_IDENTITY_TENANT_ID"),
            }
        server = helper._run_oc(["oc", "whoami", "--show-server"])
        evidence["cluster"] = server.stdout.strip()
        revision = subprocess.run(
            ["git", "rev-parse", "HEAD"],
            capture_output=True,
            text=True,
            check=True,
        )
        evidence["checkout_revision"] = revision.stdout.strip()
        provisioning_started = True
        context = bootstrap_worker_tenant(context, baseline=False)
        evidence["context"]["gateway_host"] = context.route_host
        with activate_worker_tenant(context):
            tenant = _resource(
                "maastenantconfig", TENANT_CR_NAME, context.tenant_namespace
            )
            tenant["spec"].setdefault("telemetry", {}).update(
                {
                    "enabled": True,
                    "metrics": {
                        "captureOrganization": True,
                        "captureModelUsage": True,
                        "captureUser": False,
                        "captureGroup": False,
                    },
                }
            )
            helper._apply_cr(
                {
                    "apiVersion": "maas.opendatahub.io/v1alpha1",
                    "kind": "MaasTenantConfig",
                    "metadata": {
                        "name": TENANT_CR_NAME,
                        "namespace": context.tenant_namespace,
                    },
                    "spec": tenant["spec"],
                }
            )
            helper._create_test_auth_policy(
                context.policy_name,
                model_refs=[context.model_ref],
                model_namespace=context.model_namespace,
                groups=["system:authenticated"],
            )
            for name, organization_id in subscriptions.items():
                helper._create_test_subscription(
                    name,
                    [context.model_ref],
                    model_namespace=context.model_namespace,
                    groups=["system:authenticated"],
                    token_limit=10000,
                    organization_id=organization_id,
                    token_metadata_org_id=LEGACY_ORG
                    if organization_id == ORG_A
                    else None,
                )
                helper._wait_for_subscription_inference_ready(
                    name,
                    context.model_ref,
                    model_namespace=context.model_namespace,
                )
            wait_for_gateway_authpolicy_ready(context.gateway_name)
            evidence["subscriptions"] = [
                _resource("maassubscription", name, context.tenant_namespace)
                for name in subscriptions
            ]
            evidence["gateway"] = _resource(
                "gateway", context.gateway_name, GATEWAY_NAMESPACE
            )
            evidence["model_ref"] = _resource(
                "maasmodelref", context.model_ref, context.model_namespace
            )
            evidence["simulator"] = _resource(
                "llminferenceservice", context.model_ref, context.model_namespace
            )
            policies = _oc_json("get", "telemetrypolicy", "-n", GATEWAY_NAMESPACE)[
                "items"
            ]
            evidence["telemetry_policies"] = [
                {
                    "name": item["metadata"]["name"],
                    "spec": item["spec"],
                    "status": item.get("status", {}),
                }
                for item in policies
                if item.get("spec", {}).get("targetRef", {}).get("name")
                == context.gateway_name
            ]
            deployments = _oc_json("get", "deployment", "-A")["items"]
            evidence["images"] = [
                {
                    "namespace": item["metadata"]["namespace"],
                    "deployment": item["metadata"]["name"],
                    "containers": [
                        {"name": c["name"], "image": c["image"]}
                        for c in item["spec"]["template"]["spec"]["containers"]
                    ],
                }
                for item in deployments
                if (
                    item["metadata"]["namespace"]
                    in {context.model_namespace, GATEWAY_NAMESPACE, source["namespace"]}
                    or "maas-controller" in item["metadata"]["name"]
                    or (
                        item["metadata"]["namespace"] == INFRA_NAMESPACE
                        and item["metadata"]["name"] == context.api_deployment_name
                    )
                )
            ]
            image_namespaces = {item["namespace"] for item in evidence["images"]}
            evidence["resolved_images"] = [
                {
                    "namespace": item["metadata"]["namespace"],
                    "pod": item["metadata"]["name"],
                    "containers": [
                        {
                            "name": c["name"],
                            "image": c["image"],
                            "image_id": c.get("imageID"),
                        }
                        for c in item.get("status", {}).get("containerStatuses", [])
                    ],
                }
                for item in _oc_json("get", "pods", "-A")["items"]
                if item["metadata"]["namespace"] in image_namespaces
            ]
            token = helper._get_cluster_token()
            for name in subscriptions:
                response = helper._create_api_key_raw(
                    token, name=name, subscription=name
                )
                assert response.status_code in (200, 201), (
                    f"Key creation failed: {response_summary(response)}"
                )
                data = response.json()
                key_ids.append(data["id"])
                keys[name] = data["key"]
            evidence["outcome"] = "ready"
            _write_evidence(evidence, directory)
            yield context, keys, evidence, directory
    except Exception as exc:
        if evidence["outcome"] in {"setup", "ready"}:
            evidence["outcome"] = "setup_failed"
            evidence["failure"] = str(exc)
        raise
    finally:
        # activate_worker_tenant restores the outer environment when setup/test fails.
        # Re-enter it so cleanup always revokes keys against their issuing API.
        try:
            if provisioning_started:
                with activate_worker_tenant(context):
                    for key_id in key_ids:
                        helper._revoke_api_key(token, key_id)
                    helper._delete_governance_and_wait(
                        subscriptions=[
                            (name, context.tenant_namespace) for name in subscriptions
                        ],
                        auth_policies=[(context.policy_name, context.tenant_namespace)],
                    )
                    # Finalize the model reference while its backend and tenant
                    # still exist; namespace-first deletion can strand it.
                    for kind in ("maasmodelref", "llminferenceservice"):
                        helper._delete_cr(
                            kind, context.model_ref, context.model_namespace
                        )
                        helper._wait_for_cr_absent(
                            kind, context.model_ref, context.model_namespace, timeout=90
                        )
                    evidence["model_resources_deleted"] = True
        finally:
            try:
                if provisioning_started:
                    teardown_worker_tenant(context)
            finally:
                evidence["finished_at"] = time.time()
                _write_evidence(evidence, directory)


def test_subscription_organization_identity(organization_case):
    """Two keys on one gateway/model meter usage under their stable organization IDs."""
    context, keys, evidence, directory = organization_case
    source = evidence["metrics_source"]
    expected = evidence["expected_organizations"]

    def inference(api_key, *, subscription, phase):
        response = helper._inference(
            api_key,
            path=f"/{context.model_namespace}/{context.model_ref}",
            model_name=f"e2e/{context.model_ref}",
            max_tokens=3,
        )
        request = {
            "at": time.time(),
            "subscription": subscription,
            "phase": phase,
            "status": response.status_code,
        }
        if response.status_code == 200:
            body = response.json()
            request["total_tokens"] = body.get("usage", {}).get("total_tokens")
        evidence["requests"].append(request)
        _write_evidence(evidence, directory)
        return response

    try:
        # Warm-up retries are recorded separately from the exact measured batch.
        for name, key in keys.items():
            helper._poll_status(
                key,
                200,
                timeout=90,
                inference_fn=lambda api_key, subscription=name, **kwargs: inference(
                    api_key,
                    subscription=subscription,
                    phase="warmup",
                ),
            )
        # HTTP readiness can precede propagation of TelemetryPolicy to Envoy.
        # Require metered warm-up traffic before recording the measured baseline.
        deadline = time.monotonic() + 90
        while True:
            evidence["before"] = _source_metrics(source, expected)
            missing = [
                name for name in expected if _hits(evidence["before"], name) <= 0
            ]
            if not missing:
                break
            assert time.monotonic() < deadline, (
                f"No metered warm-up requests for subscriptions: {missing}"
            )
            for name in missing:
                inference(keys[name], subscription=name, phase="warmup")
            time.sleep(2)
        exported = evidence.get("exported_metrics")
        if exported:
            exported["before"] = _export_snapshot(exported["sources"], expected)
        for name, key in keys.items():
            for _ in range(REQUESTS_PER_SUBSCRIPTION):
                response = inference(key, subscription=name, phase="measured")
                assert response.status_code == 200, (
                    f"Inference for {name} returned HTTP {response.status_code}"
                )
        deadline = time.monotonic() + METRICS_TIMEOUT
        while True:
            evidence["after"] = _source_metrics(source, expected)
            _write_evidence(evidence, directory)
            if all(
                _hits(evidence["after"], name) > _hits(evidence["before"], name)
                for name in expected
            ):
                break
            assert time.monotonic() < deadline, (
                "Limitador authorized_hits did not increase for both subscriptions"
            )
            time.sleep(2)
        for name, organization_id in expected.items():
            samples = [
                sample
                for sample in evidence["after"]["samples"]
                if sample["labels"]["subscription"] == name
            ]
            assert samples, f"No source metrics found for {name}"
            observed = {sample["labels"].get("organization_id") for sample in samples}
            assert observed == {organization_id}, (
                f"{name}: expected spec.organization.id={organization_id}, observed organization_id={observed}"
            )
        if exported:
            deadline = time.monotonic() + 120
            while True:
                exported["after"] = _export_snapshot(exported["sources"], expected)
                _write_evidence(evidence, directory)
                if all(
                    _hits(exported["after"][stage], name)
                    > _hits(exported["before"][stage], name)
                    for stage in ("collector", "scrape")
                    for name in expected
                ):
                    break
                assert time.monotonic() < deadline, (
                    "Usage counters did not increase at both collector and scrape endpoints"
                )
                time.sleep(3)
            for stage, snapshot in exported["after"].items():
                for sample in snapshot["samples"]:
                    labels = sample["labels"]
                    organization_id = expected[labels["subscription"]]
                    assert labels.get("organization_id") == organization_id, (
                        f"{stage}: organization_id was lost or changed: {labels}"
                    )
                    if stage == "scrape":
                        assert labels.get("grid_tenant_id") == organization_id, (
                            f"Scraped grid_tenant_id does not match spec.organization.id: {labels}"
                        )
                        if exported["expected_tenant_id"] is not None:
                            assert (
                                labels.get("tenant_id")
                                == exported["expected_tenant_id"]
                            ), f"ACM tenant_id was lost or changed: {labels}"
        evidence["outcome"] = "passed"
    except Exception as exc:
        evidence["outcome"] = "failed"
        evidence["failure"] = str(exc)
        raise
    finally:
        _write_evidence(evidence, directory)
