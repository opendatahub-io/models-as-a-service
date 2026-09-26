"""
TokenRateLimitPolicy rate grouping e2e tests (RHOAIENG-95277).

Kuadrant copies every TokenRateLimitPolicy limit into every route-match
ActionSet of one wasm config object per gateway (EnvoyFilter
kuadrant-<gateway> on Kuadrant 1.5+, WasmPlugin kuadrant-<gateway> on 1.4.x).
With one limit per (MaaSSubscription, model) that object grew ~27 KB per pair
and hit the etcd object size limit.

Contract under test:
  - maas-trlp-<model> has one limit per distinct rate set on the model, named
    tokens-<limit>-per-<window>[-<limit>-per-<window>...]. Rates are rendered
    "<limit>/<window>", deduplicated and sorted as strings, then "/" becomes
    "-per-" and "," becomes "-".
  - The limit's predicate matches auth.identity.selected_subscription_key
    against every subscription in the group, keys sorted:
      one member:  <clause> && !request.path.endsWith("/v1/models")
      N members:   (<clause> || <clause>) && !request.path.endsWith("/v1/models")
  - Counters are [selected_subscription_key, userid], so subscriptions sharing
    a limit keep independent budgets, as do users on one subscription.
  - A subscription at an existing rate adds a predicate clause, not a limit.

All tests use the unconfigured model so shared baseline subscriptions are not
touched, and are serial because they rebuild that model's TRLP.

Requires:
  - GATEWAY_HOST env var
  - MAAS_API_BASE_URL env var (for API key creation)
  - oc/kubectl access to manage CRs, read TRLPs, HTTPRoutes and the gateway's
    EnvoyFilter or WasmPlugin

Environment variables:
  See test_helper.py module docstring for shared environment variables.
  File-specific (optional):
  - GATEWAY_NAME: fallback gateway name when the model HTTPRoute has no
    parentRef (default: maas-default-gateway)
  - E2E_TRLP_MAX_GROWTH_PER_SUBSCRIPTION: bytes one subscription at an
    existing rate may add to the gateway wasm config (default: 8192)
"""

import json
import logging
import os
import random
import re
import subprocess
import time
import uuid
from dataclasses import dataclass, field

import pytest

from test_helper import (
    GATEWAY_NAMESPACE,
    MODEL_NAMESPACE,
    UNCONFIGURED_MODEL_PATH,
    UNCONFIGURED_MODEL_REF,
    _create_api_key_raw,
    _create_sa_token,
    _create_test_auth_policy,
    _create_test_subscription,
    _delete_cr,
    _delete_sa,
    _get_cr,
    _inference,
    _ns,
    _poll_status,
    _revoke_api_key,
    _sa_to_user,
    _wait_for_cr_absent,
    _wait_for_gateway_auth_enforced,
    _wait_for_maas_auth_policy_phase,
    _wait_for_maas_subscription_phase,
)

log = logging.getLogger(__name__)

pytestmark = [pytest.mark.serial]

MODEL_REF = UNCONFIGURED_MODEL_REF
MODEL_PATH = UNCONFIGURED_MODEL_PATH
TRLP_NAME = f"maas-trlp-{MODEL_REF}"
SUBSCRIPTIONS_ANNOTATION = "maas.opendatahub.io/subscriptions"
DEFAULT_GATEWAY_NAME = os.environ.get("GATEWAY_NAME", "maas-default-gateway")

SUBSCRIPTION_KEY = "auth.identity.selected_subscription_key"
USERID = "auth.identity.userid"
MODELS_EXEMPTION = '!request.path.endsWith("/v1/models")'
_CLAUSE_RE = re.compile(r'auth\.identity\.selected_subscription_key == "([^"]+)"')

TRLP_SYNC_TIMEOUT = 180
WASM_SYNC_TIMEOUT = 180
SA_TOKEN_DURATION = "20m"

# Same shape as test_rate_limit_exhaustion_gets_429: tokens consumed per
# response vary, so send enough requests to cross a small budget. The window
# outlasts any single test so no counter resets mid-test; every run uses fresh
# subscription names, so counters never carry over between runs.
LOW_TOKEN_LIMIT = 10
EXHAUST_WINDOW = "10m"
EXHAUST_MAX_REQUESTS = 15

# One limit per subscription cost ~27 KB per subscription per listener; a
# predicate clause costs ~3 KB per listener at 19 route matches.
MAX_GROWTH_PER_SUBSCRIPTION = int(os.environ.get("E2E_TRLP_MAX_GROWTH_PER_SUBSCRIPTION", "8192"))
SIZE_EXTRA_SUBSCRIPTIONS = 10


# ---------------------------------------------------------------------------
# Expected TRLP shape (mirrors maas-controller/pkg/controller/maas/ratetier.go)
# ---------------------------------------------------------------------------

def _rate_key(*rates):
    """rateGroupKey: "<limit>/<window>" per rate, deduplicated, sorted as strings, joined with ","."""
    return ",".join(sorted({f"{limit}/{window}" for limit, window in rates}))


def _limit_name(*rates):
    """rateGroupLimitName for a set of (limit, window) rates."""
    return "tokens-" + _rate_key(*rates).replace("/", "-per-").replace(",", "-")


def _sub_key(sub, namespace=None):
    """selected_subscription_key of sub on the model under test."""
    return f"{namespace or _ns()}/{sub}@{MODEL_NAMESPACE}/{MODEL_REF}"


def _expected_predicate(keys):
    """buildGroupLimit's predicate for a group with these subscription keys."""
    clauses = [f'{SUBSCRIPTION_KEY} == "{key}"' for key in sorted(keys)]
    match = clauses[0] if len(clauses) == 1 else "(" + " || ".join(clauses) + ")"
    return f"{match} && {MODELS_EXEMPTION}"


# ---------------------------------------------------------------------------
# TRLP inspection
# ---------------------------------------------------------------------------

def _suffix():
    return uuid.uuid4().hex[:6]


def _trlp_limits(trlp):
    return (trlp or {}).get("spec", {}).get("limits", {}) or {}


def _limit_predicates(limit):
    return [w.get("predicate", "") for w in limit.get("when", []) or []]


def _limit_counters(limit):
    return [c.get("expression", "") for c in limit.get("counters", []) or []]


def _limit_rates(limit):
    return {(r.get("limit"), r.get("window")) for r in limit.get("rates", []) or []}


def _limit_members(limit):
    """Subscription keys a limit's predicate matches."""
    return {key for predicate in _limit_predicates(limit) for key in _CLAUSE_RE.findall(predicate)}


def _limits_matching(limits, key):
    """Sorted names of the limits whose predicate matches key."""
    return sorted(name for name, limit in limits.items() if key in _limit_members(limit))


def _describe_limits(limits):
    return {name: sorted(_limit_members(limit)) for name, limit in sorted(limits.items())}


def _trlp_subscriptions(trlp):
    annotations = (trlp or {}).get("metadata", {}).get("annotations", {}) or {}
    return {s.strip() for s in annotations.get(SUBSCRIPTIONS_ANNOTATION, "").split(",") if s.strip()}


def _trlp_enforced(trlp):
    status = (trlp or {}).get("status", {}) or {}
    observed = status.get("observedGeneration")
    generation = trlp.get("metadata", {}).get("generation")
    if observed is not None and generation is not None and observed != generation:
        return False
    return any(
        c.get("type") == "Enforced" and c.get("status") == "True"
        for c in status.get("conditions", []) or []
    )


def _wait_for_trlp(ready, what, timeout=TRLP_SYNC_TIMEOUT):
    """Poll maas-trlp-<model> until ready(trlp) holds and Kuadrant reports the
    current generation Enforced.

    Phase Active on a MaaSSubscription does not mean the TRLP was rebuilt yet,
    and a stale Enforced condition can refer to the previous spec.
    """
    deadline = time.time() + timeout
    trlp = None
    while time.time() < deadline:
        trlp = _get_cr("tokenratelimitpolicy", TRLP_NAME, MODEL_NAMESPACE)
        if trlp and ready(trlp) and _trlp_enforced(trlp):
            return trlp
        time.sleep(3)
    raise TimeoutError(
        f"TRLP {MODEL_NAMESPACE}/{TRLP_NAME} did not {what} with Enforced=True within {timeout}s; "
        f"last annotation: {sorted(_trlp_subscriptions(trlp))}, "
        f"last limits: {_describe_limits(_trlp_limits(trlp))}, "
        f"last status: {(trlp or {}).get('status')}"
    )


def _wait_for_placement(placement, timeout=TRLP_SYNC_TIMEOUT):
    """Wait until the TRLP lists every subscription in placement ({subscription:
    limit name}) and each one's key is matched by exactly that limit."""
    listed = {f"{_ns()}/{sub}" for sub in placement}
    by_key = {_sub_key(sub): name for sub, name in placement.items()}

    def ready(trlp):
        limits = _trlp_limits(trlp)
        return listed <= _trlp_subscriptions(trlp) and all(
            _limits_matching(limits, key) == [name] for key, name in by_key.items()
        )

    return _wait_for_trlp(ready, f"place {placement}", timeout)


def _assert_limit_shape(name, limit, rates):
    """A grouped limit carries the given rates, the controller's predicate for
    its members and the [selected_subscription_key, userid] counters."""
    members = _limit_members(limit)
    assert members, f"{name}: predicate matches no subscription key: {_limit_predicates(limit)}"
    assert _limit_predicates(limit) == [_expected_predicate(members)], (
        f"{name}: predicate {_limit_predicates(limit)} is not the grouped form "
        f"{_expected_predicate(members)!r}"
    )
    assert _limit_counters(limit) == [SUBSCRIPTION_KEY, USERID], (
        f"{name}: counters must be [{SUBSCRIPTION_KEY}, {USERID}] so subscriptions sharing the "
        f"limit keep their own budgets; got {limit.get('counters')}"
    )
    assert _limit_rates(limit) == set(rates), (
        f"{name}: rates {sorted(_limit_rates(limit))} != {sorted(set(rates))}"
    )


# ---------------------------------------------------------------------------
# Gateway wasm config
# ---------------------------------------------------------------------------

def _gateway_for_trlp(trlp):
    """Resolve the Gateway behind the TRLP's HTTPRoute (name, namespace)."""
    route_name = trlp["spec"]["targetRef"]["name"]
    route_ns = trlp["metadata"]["namespace"]
    route = _get_cr("httproute", route_name, route_ns)
    parents = (route or {}).get("spec", {}).get("parentRefs", []) or []
    if not parents:
        return DEFAULT_GATEWAY_NAME, GATEWAY_NAMESPACE
    parent = parents[0]
    return parent["name"], parent.get("namespace", route_ns)


def _get_if_served(kind, name, namespace):
    """_get_cr, but None when the cluster does not serve kind at all."""
    try:
        return _get_cr(kind, name, namespace)
    except RuntimeError as e:
        if "doesn't have a resource type" in str(e):
            return None
        raise


def _wasm_config_kind(name, namespace):
    """Kind holding Kuadrant's wasm config for a gateway, or None.

    Kuadrant 1.5+ writes EnvoyFilter kuadrant-<gateway>; 1.4.x writes a
    WasmPlugin with the same name.
    """
    for kind in ("envoyfilter", "wasmplugin"):
        if _get_if_served(kind, name, namespace) is not None:
            return kind
    return None


def _wait_for_wasm_config(kind, name, namespace, keys, after_rv=None, timeout=WASM_SYNC_TIMEOUT):
    """Wait for a write of the wasm config newer than after_rv that renders every
    subscription key in keys. Returns (resourceVersion, compact JSON size).

    Kuadrant renders TRLP predicates verbatim into each action, so the keys
    showing up means the object reflects the TRLP that added them, not an
    intermediate write.
    """
    deadline = time.time() + timeout
    rv, missing = None, sorted(keys)
    while time.time() < deadline:
        obj = _get_cr(kind, name, namespace)
        assert obj is not None, f"{kind} {namespace}/{name} disappeared while measuring its size"
        text = json.dumps(obj, separators=(",", ":"), sort_keys=True)
        rv = obj["metadata"]["resourceVersion"]
        missing = sorted(k for k in keys if k not in text)
        if not missing and rv != after_rv:
            return rv, len(text.encode())
        time.sleep(5)
    raise TimeoutError(
        f"{kind} {namespace}/{name} did not render {missing} within {timeout}s "
        f"(resourceVersion {after_rv} -> {rv}); wrong object or Kuadrant not syncing"
    )


# ---------------------------------------------------------------------------
# Inference
# ---------------------------------------------------------------------------

def _inference_min(api_key, path=None, extra_headers=None, model_name=None):
    return _inference(api_key, path=path, extra_headers=extra_headers, model_name=model_name, max_tokens=1)


def _warm_up(api_key):
    """Wait for the gateway to accept the key; costs the key one small request."""
    _poll_status(api_key, 200, path=MODEL_PATH, timeout=120, inference_fn=_inference_min)


def _exhaust(api_key, max_requests=EXHAUST_MAX_REQUESTS):
    """Send minimal completions until 429. Returns (successes, rate_limited).

    Any status other than 200 or 429 fails the test.
    """
    successes = 0
    for i in range(max_requests):
        r = _inference_min(api_key, path=MODEL_PATH)
        log.info("Request %d/%d: %d", i + 1, max_requests, r.status_code)
        if r.status_code == 429:
            return successes, True
        assert r.status_code == 200, f"Unexpected status {r.status_code} at request {i + 1}: {r.text[:200]}"
        successes += 1
        time.sleep(0.1)
    return successes, False


# ---------------------------------------------------------------------------
# Setup and teardown
# ---------------------------------------------------------------------------

@dataclass
class _Created:
    crs: list = field(default_factory=list)   # (kind, name)
    sas: list = field(default_factory=list)   # service account names
    keys: list = field(default_factory=list)  # (oc_token, key_id)


def _wait_for_trlp_to_drop(subscriptions, timeout=TRLP_SYNC_TIMEOUT):
    """Wait until the TRLP is gone or no longer lists any of subscriptions, so
    the next test starts from a settled TRLP and gateway wasm config."""
    deadline = time.time() + timeout
    listed = set()
    while time.time() < deadline:
        trlp = _get_cr("tokenratelimitpolicy", TRLP_NAME, MODEL_NAMESPACE)
        listed = _trlp_subscriptions(trlp) & subscriptions
        if trlp is None or not listed:
            return
        time.sleep(3)
    raise TimeoutError(f"TRLP {MODEL_NAMESPACE}/{TRLP_NAME} still lists {sorted(listed)} after {timeout}s")


@pytest.fixture
def cleanup():
    """Collect API keys, CRs and service accounts; remove them after the test.

    Every step is best-effort so one failure does not leak the rest.
    """
    created = _Created()
    yield created
    ns = _ns()
    try:
        # Revoke before the owning service accounts go, while their tokens work.
        for oc_token, key_id in created.keys:
            try:
                _revoke_api_key(oc_token, key_id)
            except Exception as e:
                log.warning("Failed to revoke API key %s: %s", key_id, e)
        for kind, name in reversed(created.crs):
            _delete_cr(kind, name, namespace=ns)
        subscriptions = [name for kind, name in created.crs if kind == "maassubscription"]
        for name in subscriptions:
            try:
                _wait_for_cr_absent("maassubscription", name, namespace=ns, timeout=60)
            except Exception as e:
                log.warning("maassubscription %s/%s not removed: %s", ns, name, e)
        if subscriptions:
            try:
                _wait_for_trlp_to_drop({f"{ns}/{name}" for name in subscriptions})
            except Exception as e:
                log.warning("%s", e)
    finally:
        for sa in created.sas:
            _delete_sa(sa, namespace=ns)


def _setup_access(cleanup, users, suffix):
    name = f"e2e-rg-auth-{suffix}"
    cleanup.crs.append(("maasauthpolicy", name))
    _create_test_auth_policy(name=name, model_refs=[MODEL_REF], users=users)
    _wait_for_maas_auth_policy_phase(name, require_enforced=False)
    _wait_for_gateway_auth_enforced()


def _add_subscription(cleanup, name, users, limit, window, wait_phase=True):
    cleanup.crs.append(("maassubscription", name))
    _create_test_subscription(name=name, model_refs=[MODEL_REF], users=users, token_limit=limit, window=window)
    if wait_phase:
        _wait_for_maas_subscription_phase(name)


def _patch_rate(name, limit, window):
    """Replace the rates of a live subscription created by _add_subscription."""
    patch = [{
        "op": "replace",
        "path": "/spec/modelRefs/0/tokenRateLimits",
        "value": [{"limit": limit, "window": window}],
    }]
    result = subprocess.run(
        ["oc", "patch", "maassubscription", name, "-n", _ns(), "--type=json", "-p", json.dumps(patch)],
        capture_output=True, text=True,
    )
    if result.returncode != 0:
        raise RuntimeError(f"Failed to patch rates of maassubscription {name}: {result.stderr.strip()}")


def _new_sa(cleanup, base):
    name = f"{base}-{_suffix()}"
    cleanup.sas.append(name)
    token = _create_sa_token(name, namespace=_ns(), duration=SA_TOKEN_DURATION)
    return token, _sa_to_user(name, namespace=_ns())


def _mint_key(cleanup, oc_token, name, subscription):
    """Mint an API key bound to subscription and register it for revocation."""
    r = _create_api_key_raw(oc_token, name=name, subscription=subscription)
    assert r.status_code in (200, 201), (
        f"minting API key {name} for {subscription} failed: {r.status_code} {r.text[:200]}"
    )
    body = r.json()
    if body.get("id"):
        cleanup.keys.append((oc_token, body["id"]))
    key = body.get("key")
    assert key, f"API key response for {name} has no 'key' field"
    return key


# ---------------------------------------------------------------------------
# Tests
# ---------------------------------------------------------------------------

class TestExpectedShapeHelpers:
    """The helpers above reproduce the controller's naming and predicate
    rendering. Cases mirror TestRateGroupKey and TestRateGroupLimitName in
    maas-controller/pkg/controller/maas/ratetier_test.go."""

    @pytest.mark.parametrize("rates, name", [
        pytest.param([(1000, "1h")], "tokens-1000-per-1h", id="single-rate"),
        pytest.param([(1000, "1h"), (99999, "1s")], "tokens-1000-per-1h-99999-per-1s", id="two-rates"),
        pytest.param([(99999, "1s"), (1000, "1h")], "tokens-1000-per-1h-99999-per-1s", id="input-order-ignored"),
        pytest.param([(100, "1m"), (100, "1m")], "tokens-100-per-1m", id="duplicates-collapse"),
        pytest.param([(100, "60s")], "tokens-100-per-60s", id="window-not-normalized"),
        pytest.param([(20, "1m"), (100, "1m")], "tokens-100-per-1m-20-per-1m", id="sorted-as-strings"),
    ])
    def test_limit_name(self, rates, name):
        assert _limit_name(*rates) == name

    def test_single_member_predicate_is_unchanged(self):
        """A group of one renders exactly the per-subscription predicate main had."""
        assert _expected_predicate(["ns/a@llm/m"]) == (
            'auth.identity.selected_subscription_key == "ns/a@llm/m" && !request.path.endsWith("/v1/models")'
        )

    def test_multi_member_predicate_is_sorted_or(self):
        predicate = _expected_predicate(["ns/b@llm/m", "ns/a@llm/m"])
        assert predicate == (
            '(auth.identity.selected_subscription_key == "ns/a@llm/m"'
            ' || auth.identity.selected_subscription_key == "ns/b@llm/m")'
            ' && !request.path.endsWith("/v1/models")'
        )
        assert _limit_members({"when": [{"predicate": predicate}]}) == {"ns/a@llm/m", "ns/b@llm/m"}


class TestRateGrouping:
    """TRLP shape and budget isolation for subscriptions grouped by rate."""

    def test_same_rate_shares_limit_not_budget(self, cleanup):
        """Two subscriptions at one rate share a single TRLP limit, yet
        exhausting one leaves the same user's key on the other untouched.

        This is the core regression: with counters on userid alone, one user
        holding both subscriptions would drain both from one budget.
        """
        ns = _ns()
        s = _suffix()
        rate = (LOW_TOKEN_LIMIT, EXHAUST_WINDOW)
        name = _limit_name(rate)
        sub_a, sub_b = f"e2e-rg-same-a-{s}", f"e2e-rg-same-b-{s}"

        oc_token, user = _new_sa(cleanup, "e2e-rg-same")
        _setup_access(cleanup, [user], s)
        _add_subscription(cleanup, sub_a, [user], *rate)
        _add_subscription(cleanup, sub_b, [user], *rate)
        trlp = _wait_for_placement({sub_a: name, sub_b: name})

        limits = _trlp_limits(trlp)
        _assert_limit_shape(name, limits[name], [rate])
        assert {_sub_key(sub_a), _sub_key(sub_b)} <= _limit_members(limits[name])
        for sub in (sub_a, sub_b):
            legacy = f"{ns}-{sub}-{MODEL_REF}-tokens"
            assert legacy not in limits, f"per-subscription limit {legacy} should no longer be emitted"

        key_a = _mint_key(cleanup, oc_token, f"e2e-rg-same-a-{s}", sub_a)
        key_b = _mint_key(cleanup, oc_token, f"e2e-rg-same-b-{s}", sub_b)
        _warm_up(key_a)
        _warm_up(key_b)

        successes, limited = _exhaust(key_a)
        assert limited, f"{sub_a}: no 429 after {successes} successful requests at {_rate_key(rate)}"

        r = _inference_min(key_b, path=MODEL_PATH)
        assert r.status_code == 200, (
            f"{sub_b} shares limit {name} with exhausted {sub_a} but must keep its own budget; "
            f"got {r.status_code}: {r.text[:200]}"
        )

    def test_users_on_same_subscription_are_independent(self, cleanup):
        """Two users on one subscription each get the full budget."""
        s = _suffix()
        rate = (LOW_TOKEN_LIMIT, EXHAUST_WINDOW)
        sub = f"e2e-rg-users-{s}"

        token_1, user_1 = _new_sa(cleanup, "e2e-rg-user1")
        token_2, user_2 = _new_sa(cleanup, "e2e-rg-user2")
        _setup_access(cleanup, [user_1, user_2], s)
        _add_subscription(cleanup, sub, [user_1, user_2], *rate)
        _wait_for_placement({sub: _limit_name(rate)})

        key_1 = _mint_key(cleanup, token_1, f"e2e-rg-u1-{s}", sub)
        key_2 = _mint_key(cleanup, token_2, f"e2e-rg-u2-{s}", sub)
        _warm_up(key_1)
        _warm_up(key_2)

        successes, limited = _exhaust(key_1)
        assert limited, f"user 1: no 429 after {successes} successful requests"

        r = _inference_min(key_2, path=MODEL_PATH)
        assert r.status_code == 200, (
            f"user 2 on {sub} must not be limited by user 1's usage; got {r.status_code}: {r.text[:200]}"
        )

    def test_different_rates_get_separate_limits(self, cleanup):
        """Two rates on one model produce two limits, each enforced at its own budget."""
        s = _suffix()
        low, high = (LOW_TOKEN_LIMIT, EXHAUST_WINDOW), (LOW_TOKEN_LIMIT * 4, EXHAUST_WINDOW)
        low_name, high_name = _limit_name(low), _limit_name(high)
        sub_low, sub_high = f"e2e-rg-low-{s}", f"e2e-rg-high-{s}"

        oc_token, user = _new_sa(cleanup, "e2e-rg-diff")
        _setup_access(cleanup, [user], s)
        _add_subscription(cleanup, sub_low, [user], *low)
        _add_subscription(cleanup, sub_high, [user], *high)
        trlp = _wait_for_placement({sub_low: low_name, sub_high: high_name})

        limits = _trlp_limits(trlp)
        _assert_limit_shape(low_name, limits[low_name], [low])
        _assert_limit_shape(high_name, limits[high_name], [high])

        key_low = _mint_key(cleanup, oc_token, f"e2e-rg-low-{s}", sub_low)
        key_high = _mint_key(cleanup, oc_token, f"e2e-rg-high-{s}", sub_high)
        _warm_up(key_low)
        _warm_up(key_high)

        low_ok, low_limited = _exhaust(key_low)
        assert low_limited, f"{sub_low}: no 429 after {low_ok} successful requests at {_rate_key(low)}"

        # Same request shape on both keys, so the 4x budget must admit more requests.
        high_ok, high_limited = _exhaust(key_high, max_requests=EXHAUST_MAX_REQUESTS * 4)
        assert high_ok > low_ok, (
            f"{sub_high} ({_rate_key(high)}) admitted {high_ok} requests, not more than "
            f"{sub_low} ({_rate_key(low)}) with {low_ok}; limits are not enforced per rate"
        )
        assert high_limited, f"{sub_high}: no 429 after {high_ok} successful requests at {_rate_key(high)}"

    def test_rate_edit_moves_subscription_between_limits(self, cleanup):
        """Editing a live subscription's rate moves its clause out of the shared
        limit into the limit for the new rate, and its key stays rate limited.

        The key's selected_subscription_key does not change with the rate, and
        the TRLP update moves the clause in one write, so each request is
        counted by the old limit or the new one, never by neither.
        """
        s = _suffix()
        old_rate = (LOW_TOKEN_LIMIT, EXHAUST_WINDOW)
        new_rate = (LOW_TOKEN_LIMIT + 6, EXHAUST_WINDOW)
        old_name, new_name = _limit_name(old_rate), _limit_name(new_rate)
        sub_a, sub_b = f"e2e-rg-edit-a-{s}", f"e2e-rg-edit-b-{s}"

        oc_token, user = _new_sa(cleanup, "e2e-rg-edit")
        _setup_access(cleanup, [user], s)
        _add_subscription(cleanup, sub_a, [user], *old_rate)
        _add_subscription(cleanup, sub_b, [user], *old_rate)
        _wait_for_placement({sub_a: old_name, sub_b: old_name})

        key = _mint_key(cleanup, oc_token, f"e2e-rg-edit-{s}", sub_a)
        _warm_up(key)

        _patch_rate(sub_a, *new_rate)
        trlp = _wait_for_placement({sub_a: new_name, sub_b: old_name})

        limits = _trlp_limits(trlp)
        assert _sub_key(sub_a) not in _limit_members(limits[old_name]), (
            f"{sub_a}'s clause must leave {old_name} after its rate edit: {_limit_predicates(limits[old_name])}"
        )
        _assert_limit_shape(old_name, limits[old_name], [old_rate])
        _assert_limit_shape(new_name, limits[new_name], [new_rate])

        # Until Envoy loads the new config the old counter may still take a few
        # requests; the bound covers both budgets at one token per request.
        max_requests = old_rate[0] + new_rate[0] + 10
        successes, limited = _exhaust(key, max_requests=max_requests)
        assert limited, (
            f"{sub_a}: no 429 after {successes} requests following its rate edit "
            f"{_rate_key(old_rate)} -> {_rate_key(new_rate)}"
        )

    def test_gateway_wasm_config_grows_by_clause_not_limit(self, cleanup):
        """Subscriptions at an existing rate add predicate clauses to the
        gateway's wasm config, not limits (RHOAIENG-95277).

        Kuadrant renders every TRLP limit as two wasm actions in each
        route-match ActionSet of kuadrant-<gateway>. With one limit per
        subscription each (subscription, model) pair cost ~27 KB per listener
        and the object hit etcd's size limit. A clause costs ~3 KB per listener
        at 19 route matches, so MAX_GROWTH_PER_SUBSCRIPTION sits well below a
        full limit. A new rate still adds exactly one limit.
        """
        s = _suffix()
        window = "1h"
        # Random rates keep objects leaked by a failed run out of this run's limits.
        existing_rate = (5000 + random.randrange(100000), window)
        new_rate = (existing_rate[0] + 1, window)
        existing_name, new_name = _limit_name(existing_rate), _limit_name(new_rate)
        # Nobody calls the model here, so the owner needs no service account:
        # the controller renders limits from the subscription spec alone.
        owner = [f"system:serviceaccount:{_ns()}:e2e-rg-size-nobody"]

        base_sub = f"e2e-rg-size-base-{s}"
        _add_subscription(cleanup, base_sub, owner, *existing_rate)
        trlp = _wait_for_placement({base_sub: existing_name})
        base_limits = set(_trlp_limits(trlp))

        gw_name, gw_ns = _gateway_for_trlp(trlp)
        obj_name = f"kuadrant-{gw_name}"
        kind = _wasm_config_kind(obj_name, gw_ns)
        if kind is None:
            pytest.skip(
                f"neither EnvoyFilter nor WasmPlugin {gw_ns}/{obj_name} exists; "
                f"cannot measure Kuadrant's wasm config for gateway {gw_name}"
            )
        base_rv, base_size = _wait_for_wasm_config(kind, obj_name, gw_ns, [_sub_key(base_sub)])
        log.info("Baseline: limits %s, %s %s/%s %d bytes", sorted(base_limits), kind, gw_ns, obj_name, base_size)

        extra = [f"e2e-rg-size-{i}-{s}" for i in range(SIZE_EXTRA_SUBSCRIPTIONS)]
        for name in extra:
            _add_subscription(cleanup, name, owner, *existing_rate, wait_phase=False)
        for name in extra:
            _wait_for_maas_subscription_phase(name)
        trlp = _wait_for_placement({name: existing_name for name in [base_sub, *extra]})

        limits = _trlp_limits(trlp)
        assert set(limits) == base_limits, (
            f"{len(extra)} subscriptions at an existing rate changed the TRLP limits: "
            f"{sorted(base_limits)} -> {sorted(limits)}"
        )
        _assert_limit_shape(existing_name, limits[existing_name], [existing_rate])

        rv, size = _wait_for_wasm_config(
            kind, obj_name, gw_ns, [_sub_key(name) for name in extra], after_rv=base_rv
        )
        per_sub = (size - base_size) / len(extra)
        log.info("After %d same-rate subscriptions: %s %d bytes (%.0f B/subscription)", len(extra), kind, size, per_sub)
        assert per_sub < MAX_GROWTH_PER_SUBSCRIPTION, (
            f"{kind} {gw_ns}/{obj_name} grew {per_sub:.0f} B per subscription at an existing rate "
            f"({base_size} -> {size} for {len(extra)}), limit {MAX_GROWTH_PER_SUBSCRIPTION} B; "
            f"looks like each subscription pays for a full limit again, not a predicate clause"
        )

        new_sub = f"e2e-rg-size-new-{s}"
        _add_subscription(cleanup, new_sub, owner, *new_rate)
        trlp = _wait_for_placement({new_sub: new_name})

        limits = _trlp_limits(trlp)
        assert set(limits) - base_limits == {new_name}, (
            f"a new rate should add exactly one limit {new_name}, added {sorted(set(limits) - base_limits)}"
        )
        assert len(limits) == len(base_limits) + 1, (
            f"expected {len(base_limits) + 1} limits, got {sorted(limits)}"
        )

        _, new_size = _wait_for_wasm_config(kind, obj_name, gw_ns, [_sub_key(new_sub)], after_rv=rv)
        limit_cost = new_size - size
        log.info("After a new rate: %s %d bytes (one limit costs %d B)", kind, new_size, limit_cost)
        # Listener count scales both sides equally, so this holds on any gateway.
        assert per_sub * 2 < limit_cost, (
            f"one subscription at an existing rate cost {per_sub:.0f} B, not well below the "
            f"{limit_cost} B of one new limit; grouping is not saving gateway config size"
        )
