from typing import Any
from ocp_resources.resource import NamespacedResource
from ocp_resources.exceptions import MissingRequiredArgumentError



class RateLimitPolicy(NamespacedResource):
    """
    RateLimitPolicy enables rate limiting for service workloads in a Gateway API network
    """

    api_group: str = "kuadrant.io"

    def __init__(
        self,
        defaults: dict[str, Any] | None = None,
        limits: dict[str, Any] | None = None,
        overrides: dict[str, Any] | None = None,
        target_ref: dict[str, Any] | None = None,
        when: list[Any] | None = None,
        **kwargs: Any,
    ) -> None:
        r"""
        Args:
            defaults (dict[str, Any]): Rules to apply as defaults. Can be overridden by more specific policiy
              rules lower in the hierarchy and by less specific policy
              overrides. Use one of: defaults, overrides, or bare set of policy
              rules (implicit defaults).

            limits (dict[str, Any]): Limits holds the struct of limits indexed by a unique name

            overrides (dict[str, Any]): Rules to apply as overrides. Override all policy rules lower in the
              hierarchy. Can be overridden by less specific policy overrides.
              Use one of: defaults, overrides, or bare set of policy rules
              (implicit defaults).

            target_ref (dict[str, Any]): Reference to the object to which this policy applies.

            when (list[Any]): Overall conditions for the policy to be enforced. If omitted, the
              policy will be enforced at all requests to the protected routes.
              If present, all conditions must match for the policy to be
              enforced.

        """
        super().__init__(**kwargs)

        self.defaults = defaults
        self.limits = limits
        self.overrides = overrides
        self.target_ref = target_ref
        self.when = when

    def to_dict(self) -> None:

        super().to_dict()

        if not self.kind_dict and not self.yaml_file:
            if self.target_ref is None:
                raise MissingRequiredArgumentError(argument="self.target_ref")

            self.res["spec"] = {}
            _spec = self.res["spec"]

            _spec["targetRef"] = self.target_ref

            if self.defaults is not None:
                _spec["defaults"] = self.defaults

            if self.limits is not None:
                _spec["limits"] = self.limits

            if self.overrides is not None:
                _spec["overrides"] = self.overrides

            if self.when is not None:
                _spec["when"] = self.when