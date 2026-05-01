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

func writeParamsToTmp(params map[string]string, tmpDir string) (string, error) {
	tmp, err := os.CreateTemp(tmpDir, "params.env-")
	if err != nil {
		return "", err
	}
	defer tmp.Close()

	writer := bufio.NewWriter(tmp)
	for key, value := range params {
		if _, err := fmt.Fprintf(writer, "%s=%s\n", key, value); err != nil {
			return "", err
		}
	}
	if err := writer.Flush(); err != nil {
		return "", fmt.Errorf("failed to write to file: %w", err)
	}

	return tmp.Name(), nil
}

func updateMap(m *map[string]string, key, val string) int {
	old := (*m)[key]
	if old == val {
		return 0
	}
	(*m)[key] = val
	return 1
}

// ApplyParams mirrors opendatahub-operator/pkg/deploy.ApplyParams for params.env substitution.
func ApplyParams(componentPath, file string, imageParamsMap map[string]string, extraParamsMaps ...map[string]string) error {
	paramsFile := filepath.Join(componentPath, file)

	paramsEnvMap, err := parseParams(paramsFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}

	updated := 0
	for i := range paramsEnvMap {
		relatedImageValue := os.Getenv(imageParamsMap[i])
		if relatedImageValue != "" {
			updated |= updateMap(&paramsEnvMap, i, relatedImageValue)
		}
	}
	for _, extraParamsMap := range extraParamsMaps {
		for eKey, eValue := range extraParamsMap {
			updated |= updateMap(&paramsEnvMap, eKey, eValue)
		}
	}

	if updated == 0 {
		return nil
	}

	tmp, err := writeParamsToTmp(paramsEnvMap, componentPath)
	if err != nil {
		return err
	}

	if err = os.Rename(tmp, paramsFile); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("failed rename %s to %s: %w", tmp, paramsFile, err)
	}

	return nil
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
		// Tenant platform resources. During migration from the ODH modelsasservice
		// pipeline, force ensures a clean field-manager handoff without conflicts.
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
