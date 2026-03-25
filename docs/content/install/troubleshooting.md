# Troubleshooting

This guide helps you diagnose and resolve common issues with MaaS Platform deployments.

## Common Issues

1. **Getting `501` Not Implemented errors**: Traffic is not making it to the Gateway.
      - [ ] Verify Gateway status and HTTPRoute configuration
2. **Getting `401` Unauthorized errors when trying to create an API key**: Authentication to maas-api is not working.
      - [ ] Verify `maas-api-auth-policy` AuthPolicy is applied
      - [ ] Check if your cluster uses a custom token review audience:

      ```bash
      # Detect your cluster's audience
      AUD="$(kubectl create token default --duration=10m 2>/dev/null | \
        cut -d. -f2 | jq -Rr '@base64d | fromjson | .aud[0]' 2>/dev/null)"
      echo "Cluster audience: ${AUD}"
      ```

      If the audience is NOT `https://kubernetes.default.svc`, patch the AuthPolicy:

      ```bash
      kubectl patch authpolicy maas-api-auth-policy -n opendatahub \
        --type=merge --patch "
      spec:
        rules:
          authentication:
            openshift-identities:
              kubernetesTokenReview:
                audiences:
                  - ${AUD}
                  - maas-default-gateway-sa"
      ```

3. **Getting `401` errors when trying to get models**: Authentication is not working for the models endpoint.
      - [ ] Create a new API key and use it in the Authorization header
      - [ ] Verify `gateway-auth-policy` AuthPolicy is applied
      - [ ] Validate that the service account has `post` access to the `llminferenceservices` resource per MaaSAuthPolicy
        - Note: this should be automated by the ODH Controller
4. **Getting `404` errors when trying to get models**: The models endpoint is not working.
      - [ ] Verify `model-route` HTTPRoute exist and is applied
      - [ ] Verify the model is deployed and the `LLMInferenceService` has the `maas-default-gateway` gateway specified
      - [ ] Verify that the model is recognized by maas-api by checking the `maas-api/v1/models` endpoint (see [Validation Guide - List Available Models](validation.md#3-list-available-models))
5. **Rate limiting not working**: Verify AuthPolicy and TokenRateLimitPolicy are applied
      - [ ] Verify `gateway-rate-limits` RateLimitPolicy is applied
      - [ ] Verify TokenRateLimitPolicy is applied (e.g. gateway-default-deny or per-route policies)
      - [ ] Verify the model is deployed and the `LLMInferenceService` has the `maas-default-gateway` gateway specified
      - [ ] Verify that the model is rate limited by checking the inference endpoint (see [Validation Guide - Test Rate Limiting](validation.md#6-test-rate-limiting))
      - [ ] Verify that the model is token rate limited by checking the inference endpoint (see [Validation Guide - Test Rate Limiting](validation.md#6-test-rate-limiting))
6. **Routes not accessible (503 errors)**: Check MaaS Default Gateway status and HTTPRoute configuration
      - [ ] Verify Gateway is in `Programmed` state: `kubectl get gateway -n openshift-ingress maas-default-gateway`
      - [ ] Check HTTPRoute configuration and status

## Additional Resources

- [Validation Guide](validation.md) — Manual validation steps
- [scripts/README.md](https://github.com/opendatahub-io/models-as-a-service/blob/main/scripts/README.md) — Deployment scripts documentation
