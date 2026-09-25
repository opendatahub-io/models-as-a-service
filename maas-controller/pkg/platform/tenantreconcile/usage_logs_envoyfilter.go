package tenantreconcile

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// usageLogsServiceNamespaceAttr is the OTel resource attribute carrying the namespace that
// usage log records are attributed to.
const usageLogsServiceNamespaceAttr = "service.namespace"

// otelAccessLoggerName is the Envoy access logger that carries resource_attributes.
// HttpConnectionManager.access_log is a repeated field, and resource_attributes exists only
// on OpenTelemetryAccessLogConfig — other logger types must be left alone.
const otelAccessLoggerName = "envoy.access_loggers.open_telemetry"

// PatchUsageLogsEnvoyFilterWorkloadSelector sets spec.workloadSelector.labels["gateway.networking.k8s.io/gateway-name"]
// to the tenant's gateway so the EnvoyFilter applies only to traffic through this tenant's gateway.
func PatchUsageLogsEnvoyFilterWorkloadSelector(ef *unstructured.Unstructured, gatewayName string) error {
	if err := unstructured.SetNestedStringMap(ef.Object,
		map[string]string{"gateway.networking.k8s.io/gateway-name": gatewayName},
		"spec", "workloadSelector", "labels"); err != nil {
		return fmt.Errorf("write workloadSelector: %w", err)
	}
	unstructured.RemoveNestedField(ef.Object, "spec", "targetRefs")
	return nil
}

// PatchUsageLogsServiceNamespace appends the service.namespace resource attribute on the OTel
// access logger. The manifest does not include this attribute; it is always added here so
// usage records are attributed to the per-tenant workload namespace (e.g. ai-tenant-redteam,
// or models-as-a-service for the default tenant) rather than the gateway namespace the
// EnvoyFilter lives in.
func PatchUsageLogsServiceNamespace(ef *unstructured.Unstructured, namespace string) error {
	if namespace == "" {
		return errors.New("service namespace must not be empty")
	}

	raw, found, err := unstructured.NestedFieldNoCopy(ef.Object, "spec", "configPatches")
	if err != nil {
		return fmt.Errorf("read configPatches: %w", err)
	}
	configPatches, ok := raw.([]any)
	if !found || !ok || len(configPatches) == 0 {
		return errors.New("configPatches not found or empty")
	}

	patched := false
	for _, cp := range configPatches {
		patch, ok := cp.(map[string]any)
		if !ok {
			continue
		}
		accessLogRaw, found, err := unstructured.NestedFieldNoCopy(patch, "patch", "value", "typed_config", "access_log")
		if err != nil {
			return fmt.Errorf("read access_log: %w", err)
		}
		accessLog, ok := accessLogRaw.([]any)
		if !found || !ok {
			continue
		}
		for i, entry := range accessLog {
			accessLogEntry, ok := entry.(map[string]any)
			if !ok {
				return fmt.Errorf("access_log[%d] is not an object", i)
			}
			if name, _, _ := unstructured.NestedString(accessLogEntry, "name"); name != otelAccessLoggerName {
				continue
			}
			if err := appendUsageLogsResourceAttribute(accessLogEntry, usageLogsServiceNamespaceAttr, namespace); err != nil {
				return err
			}
			patched = true
		}
	}
	if !patched {
		return fmt.Errorf("no %s access_log entry found in configPatches", otelAccessLoggerName)
	}
	return nil
}

// appendUsageLogsResourceAttribute appends a string resource attribute on a single OTel access
// log entry. The manifest is not expected to already contain this key.
func appendUsageLogsResourceAttribute(accessLogEntry map[string]any, key, value string) error {
	raw, found, err := unstructured.NestedFieldNoCopy(accessLogEntry, "typed_config", "resource_attributes", "values")
	if err != nil {
		return fmt.Errorf("read resource_attributes.values: %w", err)
	}
	values, ok := raw.([]any)
	if !found || !ok {
		values = nil
	}

	values = append(values, map[string]any{
		"key":   key,
		"value": map[string]any{"string_value": value},
	})
	if err := unstructured.SetNestedSlice(accessLogEntry, values, "typed_config", "resource_attributes", "values"); err != nil {
		return fmt.Errorf("append resource attribute %s: %w", key, err)
	}
	return nil
}

// PatchUsageLogsUserID injects user_id capture into an EnvoyFilter loaded from the
// base manifest (which omits user_id by default). It:
//  1. Prepends the X-MaaS-Username → user_id rule to every header_to_metadata
//     HTTP_FILTER configPatch (both the wasm and wasmplugin variants).
//  2. Prepends a user_id attribute to the NETWORK_FILTER OTel access log
//     attributes list.
func PatchUsageLogsUserID(ef *unstructured.Unstructured) error {
	configPatches, found, err := unstructured.NestedSlice(ef.Object, "spec", "configPatches")
	if err != nil {
		return fmt.Errorf("read configPatches: %w", err)
	}
	if !found {
		return errors.New("configPatches not found")
	}

	userIDRule := map[string]any{
		"header": "X-MaaS-Username",
		"on_header_present": map[string]any{
			"metadata_namespace": "envoy.filters.http.header_to_metadata",
			"key":                "user_id",
			"type":               "STRING",
		},
	}
	userIDAttr := map[string]any{
		"key": "user_id",
		"value": map[string]any{
			"string_value": `%CEL("envoy.filters.http.header_to_metadata" in metadata.filter_metadata ? metadata.filter_metadata["envoy.filters.http.header_to_metadata"]["user_id"] : request.headers["x-maas-username"])%`,
		},
	}

	for i, p := range configPatches {
		patch, ok := p.(map[string]any)
		if !ok {
			continue
		}
		applyTo, _ := patch["applyTo"].(string)

		switch applyTo {
		case "HTTP_FILTER":
			name, _, _ := unstructured.NestedString(patch, "patch", "value", "name")
			if name != "envoy.filters.http.header_to_metadata.identity" {
				continue
			}
			rules, _, err := unstructured.NestedSlice(patch, "patch", "value", "typed_config", "request_rules")
			if err != nil {
				return fmt.Errorf("read request_rules from configPatch %d: %w", i, err)
			}
			rules = append([]any{userIDRule}, rules...)
			if err := unstructured.SetNestedSlice(patch, rules, "patch", "value", "typed_config", "request_rules"); err != nil {
				return fmt.Errorf("set request_rules in configPatch %d: %w", i, err)
			}
			configPatches[i] = patch

		case "NETWORK_FILTER":
			accessLogs, _, err := unstructured.NestedSlice(patch, "patch", "value", "typed_config", "access_log")
			if err != nil {
				return fmt.Errorf("read access_log from NETWORK_FILTER patch: %w", err)
			}
			if len(accessLogs) == 0 {
				continue
			}
			al, ok := accessLogs[0].(map[string]any)
			if !ok {
				continue
			}
			attrs, _, err := unstructured.NestedSlice(al, "typed_config", "attributes", "values")
			if err != nil {
				return fmt.Errorf("read attributes.values: %w", err)
			}
			attrs = append([]any{userIDAttr}, attrs...)
			if err := unstructured.SetNestedSlice(al, attrs, "typed_config", "attributes", "values"); err != nil {
				return fmt.Errorf("set attributes.values: %w", err)
			}
			accessLogs[0] = al
			if err := unstructured.SetNestedSlice(patch, accessLogs, "patch", "value", "typed_config", "access_log"); err != nil {
				return fmt.Errorf("write back access_log: %w", err)
			}
			configPatches[i] = patch
		}
	}

	return unstructured.SetNestedSlice(ef.Object, configPatches, "spec", "configPatches")
}

// PatchUsageLogsClusterAddress sets the collector address in the CLUSTER configPatch.
func PatchUsageLogsClusterAddress(ef *unstructured.Unstructured, address string) error {
	configPatches, found, err := unstructured.NestedSlice(ef.Object, "spec", "configPatches")
	if err != nil {
		return fmt.Errorf("read configPatches: %w", err)
	}
	if !found || len(configPatches) == 0 {
		return errors.New("configPatches not found or empty")
	}

	patch, ok := configPatches[0].(map[string]any)
	if !ok {
		return errors.New("configPatches[0] is not an object")
	}

	endpoints, found, err := unstructured.NestedSlice(patch, "patch", "value", "load_assignment", "endpoints")
	if err != nil {
		return fmt.Errorf("read load_assignment.endpoints: %w", err)
	}
	if !found || len(endpoints) == 0 {
		return errors.New("load_assignment.endpoints not found or empty")
	}
	ep0, ok := endpoints[0].(map[string]any)
	if !ok {
		return errors.New("endpoints[0] is not an object")
	}
	lbEndpoints, found, err := unstructured.NestedSlice(ep0, "lb_endpoints")
	if err != nil {
		return fmt.Errorf("read lb_endpoints: %w", err)
	}
	if !found || len(lbEndpoints) == 0 {
		return errors.New("lb_endpoints not found or empty")
	}
	lbe0, ok := lbEndpoints[0].(map[string]any)
	if !ok {
		return errors.New("lb_endpoints[0] is not an object")
	}

	if err := unstructured.SetNestedField(lbe0, address,
		"endpoint", "address", "socket_address", "address"); err != nil {
		return fmt.Errorf("set socket_address.address: %w", err)
	}

	lbEndpoints[0] = lbe0
	ep0["lb_endpoints"] = lbEndpoints
	endpoints[0] = ep0
	if err := unstructured.SetNestedSlice(patch, endpoints,
		"patch", "value", "load_assignment", "endpoints"); err != nil {
		return fmt.Errorf("write back endpoints: %w", err)
	}
	configPatches[0] = patch
	return unstructured.SetNestedSlice(ef.Object, configPatches, "spec", "configPatches")
}
