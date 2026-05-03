package tenantreconcile

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const ssaFieldOwner = "maas-controller"

func parseParams(fileName string) (map[string]string, error) {
	paramsEnv, err := os.Open(fileName)
	if err != nil {
		return nil, err
	}
	defer paramsEnv.Close()

	paramsEnvMap := make(map[string]string)
	scanner := bufio.NewScanner(paramsEnv)
	for scanner.Scan() {
		line := scanner.Text()
		key, value, found := strings.Cut(line, "=")
		if found {
			paramsEnvMap[key] = value
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	return paramsEnvMap, nil
}

// BuildParamsMap reads the on-disk params.env as a read-only template,
// applies RELATED_IMAGE_* env-var overrides and any extra key-value maps,
// and returns the fully resolved parameter set without writing anything to disk.
// This eliminates the circular-write problem where both the platform operator
// and maas-controller used to mutate the same params.env file.
func BuildParamsMap(componentPath, file string, imageParamsMap map[string]string, extraParamsMaps ...map[string]string) (map[string]string, error) {
	paramsFile := filepath.Join(componentPath, file)

	paramsEnvMap, err := parseParams(paramsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]string), nil
		}
		return nil, err
	}

	for key := range paramsEnvMap {
		if envVar, ok := imageParamsMap[key]; ok {
			if val := os.Getenv(envVar); val != "" {
				paramsEnvMap[key] = val
			}
		}
	}
	for _, extra := range extraParamsMaps {
		for k, v := range extra {
			paramsEnvMap[k] = v
		}
	}

	return paramsEnvMap, nil
}

// ApplyRendered server-side-applies rendered objects with Tenant as controller owner (ODH deploy parity).
// Same-namespace children get a standard ownerReference; cluster-scoped and cross-namespace children
// get tracking labels instead (Kubernetes forbids cross-namespace and namespaced-to-cluster ownerReferences).
func ApplyRendered(ctx context.Context, c client.Client, scheme *runtime.Scheme, tenant *maasv1alpha1.Tenant, objs []unstructured.Unstructured) error {
	for i := range objs {
		u := objs[i].DeepCopy()

		childNs := u.GetNamespace()
		if childNs != "" && childNs == tenant.Namespace {
			if err := controllerutil.SetControllerReference(tenant, u, scheme); err != nil {
				return fmt.Errorf("set controller reference on %s %s/%s: %w", u.GetKind(), u.GetNamespace(), u.GetName(), err)
			}
		} else {
			setTenantTrackingLabels(u, tenant)
		}
		unstructured.RemoveNestedField(u.Object, "metadata", "managedFields")
		unstructured.RemoveNestedField(u.Object, "metadata", "resourceVersion")
		unstructured.RemoveNestedField(u.Object, "status")
		// ForceOwnership is intentional: maas-controller is the sole manager for
		// Tenant platform resources, ensuring a clean field-manager handoff.
		if err := c.Patch(ctx, u, client.Apply, client.FieldOwner(ssaFieldOwner), client.ForceOwnership); err != nil {
			if apimeta.IsNoMatchError(err) && isOptionalAPIGroup(u.GroupVersionKind().Group) {
				// CRD not yet registered for a known optional dependency (e.g. Perses CRDs
				// installed by COO which may not be present yet). Skip so the rest of the
				// platform manifests are applied and Tenant reconcile does not fail.
				// The CRD watch will re-trigger reconcile once the CRDs appear.
				log.FromContext(ctx).Info("skipping resource: optional CRD not yet registered, will apply once installed",
					"group", u.GroupVersionKind().Group, "kind", u.GetKind(),
					"name", u.GetName(), "namespace", u.GetNamespace())
				continue
			}
			return fmt.Errorf("apply %s %s/%s: %w", u.GetKind(), u.GetNamespace(), u.GetName(), err)
		}
	}
	return nil
}

func setTenantTrackingLabels(obj *unstructured.Unstructured, tenant *maasv1alpha1.Tenant) {
	labels := obj.GetLabels()
	if labels == nil {
		labels = make(map[string]string)
	}
	labels[LabelTenantName] = tenant.Name
	labels[LabelTenantNamespace] = tenant.Namespace
	obj.SetLabels(labels)
}
