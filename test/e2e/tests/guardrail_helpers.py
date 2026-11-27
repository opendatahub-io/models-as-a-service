"""Helpers for the guardrail e2e.

Self-contained on purpose: the guardrail tests are the only consumer, and
nothing here belongs in the shared conftest until something else wants it.

The provider is a stub, not the TrustyAI controller. The binding verdicts are
ai-gateway-controller's and its envtest covers them; what these tests own is
the MaaS chain around a provider: subscription selection, the AuthPolicy
header, AGC compiling the check, and the gateway enforcing the verdict. So the
cluster gets the NemoGuardrails CRD only (prow_run_smoke_test.sh applies the
vendored copy), and each provider is a CR whose status the test writes itself,
pointing at a small /v1/checks server that answers the way NeMo does.
"""
import json
import os
import subprocess
import time

import pytest
from test_helper import MAAS_API_DEPLOYMENT_NAMESPACE

# Opt-in gate, set by prow_run_smoke_test.sh alongside the CRD install.
RUN_GUARDRAILS = os.environ.get("E2E_RUN_GUARDRAILS", "").lower() in {"1", "true", "yes"}

NEMO_CRD = "nemoguardrails.trustyai.opendatahub.io"
NEMO_API_VERSION = "trustyai.opendatahub.io/v1alpha1"
AIGUARDRAIL_CRD = "aiguardrails.aigateway.opendatahub.io"
AIGUARDRAIL_API_VERSION = "aigateway.opendatahub.io/v1alpha1"

DEFAULT_CONFIG_ID = "default-all"
# AGC reads configuration names off the provider CR and never fetches the
# ConfigMaps, so the name only has to be stable, not to exist.
STUB_CONFIGMAP = "nemo-guardrails-default-all"

# AGC accepts a NeMo Service only in an AI Gateway infrastructure namespace,
# which is where maas-api runs.
INFRA_NAMESPACE = MAAS_API_DEPLOYMENT_NAMESPACE

# Pinned rather than a moving tag, so a base-image push cannot change a run.
STUB_IMAGE = os.environ.get(
    "E2E_GUARDRAIL_STUB_IMAGE",
    "registry.access.redhat.com/ubi9/python-312:1-1795522683",
)
# The pod port AGC opens egress to for a provider without kube-rbac-proxy.
STUB_PORT = 8000
# A prompt containing BLOCK_MARKER is blocked, anything else passes. Praxis
# answers a request-phase block with a 403 whose body is the rail name, so
# STUB_RAIL in a refusal proves the verdict came back from the callout.
BLOCK_MARKER = "e2e-guardrail-block"
STUB_RAIL = "e2e-stub-rail"

STUB_SERVER = """\
import json
import os
from http.server import BaseHTTPRequestHandler, HTTPServer

MARKER = os.environ["BLOCK_MARKER"].encode()
RAIL = os.environ["STUB_RAIL"]


class Checks(BaseHTTPRequestHandler):
    def _body(self):
        if self.headers.get("Transfer-Encoding", "").lower() == "chunked":
            body = b""
            while True:
                size = int(self.rfile.readline().strip(), 16)
                if size == 0:
                    self.rfile.readline()
                    return body
                body += self.rfile.read(size)
                self.rfile.readline()
        return self.rfile.read(int(self.headers.get("Content-Length") or 0))

    def do_POST(self):
        if self.path.rstrip("/") != "/v1/checks":
            self.send_error(404)
            return
        if MARKER in self._body():
            verdict = {"status": "blocked", "content": "", "rail": RAIL}
        else:
            verdict = {"status": "passed", "content": ""}
        payload = json.dumps(verdict).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(payload)))
        self.end_headers()
        self.wfile.write(payload)


HTTPServer(("", 8000), Checks).serve_forever()
"""

PROVIDER_READY_TIMEOUT = int(os.environ.get("E2E_GUARDRAIL_PROVIDER_TIMEOUT", "300"))
VERDICT_TIMEOUT = int(os.environ.get("E2E_GUARDRAIL_VERDICT_TIMEOUT", "120"))
POLL_INTERVAL = 3

# A polling loop cannot check its own deadline while oc is blocked, so a
# stalled request would outlast the two timeouts above and hold the worker.
# Matches the 30s the rest of the suite bounds its oc calls with.
OC_TIMEOUT = int(os.environ.get("E2E_GUARDRAIL_OC_TIMEOUT", "30"))


def oc(*args, check=True, input_text=None, timeout=OC_TIMEOUT):
    """Run an oc command, capturing output."""
    return subprocess.run(
        ["oc", *args],
        capture_output=True,
        text=True,
        check=check,
        input=input_text,
        timeout=timeout,
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
            "guardrail e2e is opt-in: apply "
            "test/e2e/fixtures/nemoguardrails-crd before the AGC companion and set "
            "E2E_RUN_GUARDRAILS=true"
        )


def require_guardrail_platform():
    """Skip unless both CRDs the binding needs are installed."""
    require_opt_in()
    if not crd_installed(NEMO_CRD):
        pytest.skip(f"{NEMO_CRD} is not installed")
    if not crd_installed(AIGUARDRAIL_CRD):
        pytest.skip(f"{AIGUARDRAIL_CRD} is not installed")


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


def stub_service_name(namespace: str, name: str) -> str:
    """The NeMo Service AGC derives for provider namespace/name.

    Mirrors ai-gateway-controller's guardrail.NemoServiceFor: the provider name
    when the CR lives in the infrastructure namespace itself, and
    <namespace>-<name> otherwise.
    """
    return name if namespace == INFRA_NAMESPACE else f"{namespace}-{name}"


def _stub_workload(service: str) -> list[dict]:
    """The stub's script, Deployment and Service in the infrastructure namespace."""
    labels = {"app.kubernetes.io/name": "e2e-nemo-stub", "e2e-nemo-stub": service}
    return [
        {
            "apiVersion": "v1",
            "kind": "ConfigMap",
            "metadata": {"name": f"{service}-server", "namespace": INFRA_NAMESPACE},
            "data": {"server.py": STUB_SERVER},
        },
        {
            "apiVersion": "apps/v1",
            "kind": "Deployment",
            "metadata": {"name": service, "namespace": INFRA_NAMESPACE, "labels": labels},
            "spec": {
                "replicas": 1,
                "selector": {"matchLabels": labels},
                "template": {
                    "metadata": {"labels": labels},
                    "spec": {
                        "containers": [{
                            "name": "checks",
                            "image": STUB_IMAGE,
                            "command": ["python3", "/stub/server.py"],
                            "env": [
                                {"name": "BLOCK_MARKER", "value": BLOCK_MARKER},
                                {"name": "STUB_RAIL", "value": STUB_RAIL},
                            ],
                            "ports": [{"name": "http", "containerPort": STUB_PORT}],
                            "readinessProbe": {"tcpSocket": {"port": STUB_PORT}},
                            "volumeMounts": [{"name": "server", "mountPath": "/stub"}],
                            "securityContext": {
                                "allowPrivilegeEscalation": False,
                                "capabilities": {"drop": ["ALL"]},
                                "runAsNonRoot": True,
                                "seccompProfile": {"type": "RuntimeDefault"},
                            },
                        }],
                        "volumes": [{
                            "name": "server",
                            "configMap": {"name": f"{service}-server"},
                        }],
                    },
                },
            },
        },
        {
            "apiVersion": "v1",
            "kind": "Service",
            "metadata": {"name": service, "namespace": INFRA_NAMESPACE, "labels": labels},
            "spec": {
                "selector": labels,
                "ports": [{"name": "http", "port": 80, "targetPort": STUB_PORT}],
            },
        },
    ]


def deploy_stub_provider(namespace: str, name: str, config_id: str = DEFAULT_CONFIG_ID):
    """Stand up a NemoGuardrails provider backed by the stub server.

    The workload comes first: AGC reads the Service's selector to open egress,
    so it has to exist before a policy binds the provider. The status is then
    written the way TrustyAI publishes it. Without the enable-auth annotation
    AGC calls the Service over plain http, which is all the stub serves.
    """
    service = stub_service_name(namespace, name)
    for manifest in _stub_workload(service):
        apply(manifest)
    oc(
        "rollout", "status", f"deployment/{service}", "-n", INFRA_NAMESPACE,
        f"--timeout={PROVIDER_READY_TIMEOUT}s",
        timeout=PROVIDER_READY_TIMEOUT + OC_TIMEOUT,
    )
    apply({
        "apiVersion": NEMO_API_VERSION,
        "kind": "NemoGuardrails",
        "metadata": {"name": name, "namespace": namespace},
        "spec": {
            "nemoConfigs": [{
                "name": config_id,
                "default": True,
                "configMaps": [STUB_CONFIGMAP],
            }],
        },
    })
    oc(
        "patch", "nemoguardrails", name, "-n", namespace,
        "--subresource=status", "--type=merge",
        "-p", json.dumps({"status": {"workloadNamespace": INFRA_NAMESPACE}}),
    )


def delete_stub_provider(namespace: str, name: str):
    """Remove the provider CR and its stub workload."""
    delete("nemoguardrails", name, namespace)
    service = stub_service_name(namespace, name)
    delete("service", service, INFRA_NAMESPACE)
    delete("deployment", service, INFRA_NAMESPACE)
    delete("configmap", f"{service}-server", INFRA_NAMESPACE)


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
