"""E2E: model-name conflicts and same-named objects across namespaces.

P0 scenarios:

7. Two ExternalModels with the same client-facing modelName attached to one
   gateway from different namespaces: the tenant's own model always wins,
   otherwise the oldest; requests reach only the winner's provider and the
   loser reports ModelNameConflict.
8. Same-named ExternalModels and ExternalProviders in two user namespaces
   attached to one gateway both serve correctly, with no overlay stable_id or
   DestinationRule collision (regression coverage for the shared-provider
   overlay collision bug).

Conflict pairs share the CR name across namespaces (the client-facing name
defaults to it), which keeps the client-facing name equal to the
MaaSModelRef name: path-based inference stays authorizable that way,
because the gateway auth matches ``{namespace}/{name}`` path identities
against MaaSModelRef names. The same-named scenario also needs distinct
client-facing names (a shared one is the conflict above), and models whose
client-facing name differs from their CRD name cannot be called on this
stack — the auth resolves the CRD name (status.resolvedModelAlias) while
the routes match the client-facing name — so its serving test proves the
default-named model with a real request and skips the overridden model's
request until that gap is fixed.

Conflict CR names are unique per test: a previous test's models live until
module teardown, and a tenant-own model pre-holds its client-facing name
for the whole gateway — a shared name would make every later pair lose.

Shares the tenant_external_model session tenants (see conftest.tenant_external_model_tenants);
this module creates its own user namespaces with unique names.
"""

from __future__ import annotations

import logging
import time
import uuid

import pytest
from multitenancy_helpers import GATEWAY_NAMESPACE, delete_namespace_best_effort
from tenant_external_model_helpers import (
    TenantExternalModelResources,
    assert_served_by_provider,
    attachment_httproutes,
    destination_rules_referencing,
    gateway_ref,
    infer_external_chat,
    maas_model_ref_alias,
    new_admitted_user_namespace,
    provision_external_model_stack,
    redact_sensitive,
    route_is_for_model,
    serve_external_model_in_tenant,
    wait_for_attachment_httproute,
    wait_for_gateway_attachment_reason,
    wait_for_no_attachment_httproute,
)

log = logging.getLogger(__name__)

# Serialized with tenant_ipp until the legacy IPP controllers skip
# gatewayRefs ExternalModels (image pin pending): without that guard they
# adopt this suite's models and flap status.httpRouteName.
pytestmark = pytest.mark.xdist_group("tenant_ipp")


def _conflict_cr_name() -> str:
    """A CR name shared by one test's conflict pair, unique across tests."""
    return f"e2e-cfl-{uuid.uuid4().hex[:6]}"


@pytest.fixture(scope="module")
def conflicts_env(tenant_external_model_tenants):
    tenant_a = tenant_external_model_tenants["tenant_a"]
    user_ns_1 = new_admitted_user_namespace("e2e-extmod-cfl-1", tenant_a["gateway_name"])
    user_ns_2 = new_admitted_user_namespace("e2e-extmod-cfl-2", tenant_a["gateway_name"])
    resources = TenantExternalModelResources()
    try:
        yield {
            "tenant_a": tenant_a,
            "user_ns_1": user_ns_1,
            "user_ns_2": user_ns_2,
            "resources": resources,
        }
    finally:
        resources.cleanup()
        delete_namespace_best_effort(user_ns_1)
        delete_namespace_best_effort(user_ns_2)


def _serve_in_tenant(env: dict, case: dict, model: dict, **kwargs) -> dict:
    # Subscription and policy names must be unique within the tenant
    # namespace: same-named models in different namespaces would otherwise
    # share f"{name}-sub" and the second apply would clobber the first
    # (orphaning the first key).
    ns_suffix = model["namespace"].rsplit("-", 1)[-1]
    access = serve_external_model_in_tenant(
        case,
        model["name"],
        model["namespace"],
        policy_name=f"{model['name']}-{ns_suffix}-access",
        subscription_name=f"{model['name']}-{ns_suffix}-sub",
        **kwargs,
    )
    env["resources"].track("maasauthpolicy", access["policy"], case["tenant_ns"])
    env["resources"].track("maassubscription", access["subscription"], case["tenant_ns"])
    return access


class TestModelNameConflict:
    """Same client-facing modelName on one gateway from two namespaces."""

    def test_tenant_own_model_wins_over_older_user_namespace_model(self, conflicts_env):
        """The tenant's own model wins even when a user-namespace model with
        the same client-facing name is older; the loser reports
        ModelNameConflict and never receives traffic."""
        env = conflicts_env
        tenant_a = env["tenant_a"]
        gateway = gateway_ref(tenant_a["gateway_name"])
        # One shared CR name — and therefore client-facing name — across a
        # user namespace and the tenant namespace.
        name = _conflict_cr_name()

        # The older model lives in a user namespace...
        older = provision_external_model_stack(
            env["resources"],
            name=name,
            namespace=env["user_ns_1"],
            gateway_refs=[gateway],
        )
        # Guarantee the challenger is strictly older: creationTimestamp has
        # one-second resolution, and on a tie the AGC falls back to
        # namespace/name ordering — the test would risk passing through the
        # tiebreaker instead of the tenant-own rule.
        time.sleep(2)
        # ...the younger model lives in the tenant's own namespace.
        tenant_own = provision_external_model_stack(
            env["resources"],
            name=name,
            namespace=tenant_a["tenant_ns"],
            gateway_refs=[gateway],
        )
        access = _serve_in_tenant(env, tenant_a, tenant_own)

        wait_for_gateway_attachment_reason(
            older["name"], older["namespace"], tenant_a["gateway_name"], "ModelNameConflict"
        )
        # The conflict contract's serving effect: the loser is skipped by
        # admission, so it must hold no route on the gateway (matched by
        # route name — the namespace hosts other tests' models).
        wait_for_no_attachment_httproute(
            older["namespace"], tenant_a["gateway_name"], model_name=older["name"]
        )
        # The winner serves with its own credential.
        response = infer_external_chat(
            tenant_a["gateway_url"],
            tenant_own["namespace"],
            tenant_own["client_name"],
            access["api_key"],
            path_tag=tenant_own["path_tag"],
        )
        assert_served_by_provider(
            response, tenant_own["path_tag"], credential=tenant_own["credential"]
        )
        # Defense in depth only: this key is bound to the winner's
        # subscription, so auth denies the loser's path identity whether or
        # not a route lingers — the route absence above is the actual
        # conflict-suppression proof.
        denied = infer_external_chat(
            tenant_a["gateway_url"],
            older["namespace"],
            older["client_name"],
            access["api_key"],
            path_tag=older["path_tag"],
            retries=1,
        )
        assert denied.status_code != 200, (
            f"the conflicted loser must not serve, got {denied.status_code}: "
            f"{redact_sensitive(denied.text)}"
        )

    def test_oldest_wins_when_no_tenant_own_model(self, conflicts_env):
        """With no tenant-own model, the oldest of two user-namespace models
        with the same client-facing name wins."""
        env = conflicts_env
        tenant_a = env["tenant_a"]
        gateway = gateway_ref(tenant_a["gateway_name"])
        name = _conflict_cr_name()

        oldest = provision_external_model_stack(
            env["resources"],
            name=name,
            namespace=env["user_ns_1"],
            gateway_refs=[gateway],
        )
        # Guarantee distinct creation timestamps: the AGC decides the winner
        # by creation time (then namespace/name), and "oldest wins" is the
        # behavior under test.
        time.sleep(2)
        youngest = provision_external_model_stack(
            env["resources"],
            name=name,
            namespace=env["user_ns_2"],
            gateway_refs=[gateway],
        )
        access = _serve_in_tenant(env, tenant_a, oldest)

        wait_for_gateway_attachment_reason(
            youngest["name"], youngest["namespace"], tenant_a["gateway_name"], "ModelNameConflict"
        )
        # The conflict contract's serving effect: the loser is skipped by
        # admission, so it must hold no route on the gateway (matched by
        # route name — the namespace hosts other tests' models).
        wait_for_no_attachment_httproute(
            youngest["namespace"], tenant_a["gateway_name"], model_name=youngest["name"]
        )
        response = infer_external_chat(
            tenant_a["gateway_url"],
            oldest["namespace"],
            oldest["client_name"],
            access["api_key"],
            path_tag=oldest["path_tag"],
        )
        assert_served_by_provider(response, oldest["path_tag"], credential=oldest["credential"])
        # Defense in depth only: this key is bound to the winner's
        # subscription, so auth denies the loser's path identity whether or
        # not a route lingers — the route absence above is the actual
        # conflict-suppression proof.
        denied = infer_external_chat(
            tenant_a["gateway_url"],
            youngest["namespace"],
            youngest["client_name"],
            access["api_key"],
            path_tag=youngest["path_tag"],
            retries=1,
        )
        assert denied.status_code != 200, (
            f"the conflicted loser must not serve, got {denied.status_code}"
        )


class TestSameNamedObjectsTwoNamespaces:
    """Same-named ExternalModels/Providers/Secrets in two user namespaces on
    one gateway set up independent serving stacks without colliding (overlay
    collision regression)."""

    @pytest.fixture(scope="class")
    def same_named_pair(self, conflicts_env):
        env = conflicts_env
        tenant_a = env["tenant_a"]
        gateway = gateway_ref(tenant_a["gateway_name"])
        shared_cr_name = f"e2e-cfl-same-{uuid.uuid4().hex[:6]}"
        shared_secret_name = f"{shared_cr_name}-provider-cred"

        # Identical CR, provider, and Secret names in both namespaces.
        # Distinct client-facing modelNames (a shared client name would be
        # the ModelNameConflict scenario instead): model 1 keeps the default
        # name (client == CRD name), model 2 overrides — see the serving
        # test for what that costs today.
        # Model 2 is created and admitted first: on a build whose attachment
        # stable_ids collide (the overlay bug this test regresses) the first
        # attachment keeps the shared id, so the callable model 1 would lose
        # its route and fail the serving proof — with model 1 first, the
        # collision would pass silently.
        model_2 = provision_external_model_stack(
            env["resources"],
            name=shared_cr_name,
            namespace=env["user_ns_2"],
            gateway_refs=[gateway],
            model_name=f"{shared_cr_name}-two",
            provider_name=shared_cr_name,
            credential_secret_name=shared_secret_name,
        )
        wait_for_attachment_httproute(
            model_2["namespace"], tenant_a["gateway_name"], model_name=model_2["name"]
        )
        model_1 = provision_external_model_stack(
            env["resources"],
            name=shared_cr_name,
            namespace=env["user_ns_1"],
            gateway_refs=[gateway],
            provider_name=shared_cr_name,
            credential_secret_name=shared_secret_name,
        )
        assert model_1["credential"] != model_2["credential"]
        assert model_1["path_tag"] != model_2["path_tag"]

        # Both models attach with their own admitted route and complete
        # their own MaaS stack (the subscriptions get unique names per
        # namespace — see _serve_in_tenant).
        for model in (model_1, model_2):
            wait_for_attachment_httproute(
                model["namespace"], tenant_a["gateway_name"], model_name=model["name"]
            )
        access_1 = _serve_in_tenant(env, tenant_a, model_1)
        access_2 = _serve_in_tenant(env, tenant_a, model_2)
        return {
            "shared_cr_name": shared_cr_name,
            "models": (model_1, model_2),
            "accesses": (access_1, access_2),
        }

    def test_same_named_objects_both_attach(self, conflicts_env, same_named_pair):
        """Both same-named models hold their own admitted route and completed
        MaaS stack, and tenant-published DestinationRules stay distinct
        objects (no shared-name collision)."""
        tenant_a = conflicts_env["tenant_a"]
        pair = same_named_pair

        for model in pair["models"]:
            routes = [
                r
                for r in attachment_httproutes(model["namespace"], tenant_a["gateway_name"])
                if route_is_for_model(r, model["name"])
            ]
            assert len(routes) == 1, (
                f"expected one route for {model['namespace']}/{model['name']}, "
                f"got {[r['metadata']['name'] for r in routes]}"
            )

        # No DestinationRule collision: AGC keeps each model's provider
        # DestinationRule in the Gateway namespace under a name hashed over
        # the gateway and the provider's namespace, so the same-named
        # providers in two model namespaces must stay distinct objects.
        rules = destination_rules_referencing(pair["shared_cr_name"], GATEWAY_NAMESPACE)
        rule_uids = {rule["metadata"]["uid"] for rule in rules}
        assert len(rule_uids) >= 2, (
            f"expected distinct DestinationRules per same-named model in {GATEWAY_NAMESPACE}, "
            f"got uids {rule_uids}"
        )

    def test_same_named_objects_both_serve(self, conflicts_env, same_named_pair):
        """Both same-named models serve with their own credentials.

        Model 1's client-facing name equals its CRD name, so it is callable
        through the normal path contract. Model 2's client-facing name
        differs from its CRD name, which the gateway auth cannot resolve on
        this stack: the controller publishes status.resolvedModelAlias from
        the ExternalModel CRD name while the routes match the client-facing
        name, so overridden modelNames are unreachable end to end. Its
        request proof is skipped until the alias carries the client-facing
        name.
        """
        tenant_a = conflicts_env["tenant_a"]
        (model_1, model_2) = same_named_pair["models"]
        (access_1, access_2) = same_named_pair["accesses"]

        response = infer_external_chat(
            tenant_a["gateway_url"],
            model_1["namespace"],
            model_1["client_name"],
            access_1["api_key"],
            path_tag=model_1["path_tag"],
        )
        assert_served_by_provider(
            response, model_1["path_tag"], credential=model_1["credential"]
        )

        alias = maas_model_ref_alias(model_2["namespace"], model_2["name"])
        if alias != model_2["client_name"]:
            pytest.skip(
                "client-facing model names are not resolvable through the gateway "
                f"yet: MaaSModelRef {model_2['namespace']}/{model_2['name']} "
                f"status.resolvedModelAlias={alias!r} != {model_2['client_name']!r}"
            )
        response = infer_external_chat(
            tenant_a["gateway_url"],
            model_2["namespace"],
            model_2["client_name"],
            access_2["api_key"],
            path_tag=model_2["path_tag"],
        )
        assert_served_by_provider(
            response, model_2["path_tag"], credential=model_2["credential"]
        )
