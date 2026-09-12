package tenantreconcile

import (
	"errors"
	"fmt"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

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
