/*
Copyright 2025.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package maas

import (
	"context"
	"errors"
	"fmt"

	"github.com/go-logr/logr"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

const (
	discoveryDeploymentName = "maas-discovery"
	discoveryContainerName  = "maas-discovery"
)

func (r *LifecycleReconciler) ensureDiscoveryService(ctx context.Context, log logr.Logger) error {
	if r.DiscoveryManifestPath == "" {
		log.V(1).Info("discovery manifest path not configured; skipping discovery service")
		return nil
	}

	var cfg maasv1alpha1.Config
	if err := r.Get(ctx, client.ObjectKey{Name: maasv1alpha1.ConfigInstanceName}, &cfg); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}

	discoveryNS := r.DiscoveryNamespace
	if discoveryNS == "" {
		discoveryNS = r.DeploymentNS
	}

	resources, err := tenantreconcile.RenderKustomize(r.DiscoveryManifestPath, discoveryNS)
	if err != nil {
		return fmt.Errorf("render discovery service: %w", err)
	}

	crossNS := buildDiscoveryCrossNamespaceRBAC(discoveryNS, r.AITenantNamespace, r.GatewayNamespace)
	resources = append(resources, crossNS...)

	if !r.DiscoveryEnabled {
		return r.teardownDiscoveryResources(ctx, log, &cfg, resources)
	}

	for i := range resources {
		res := &resources[i]

		if err := patchDiscoveryImage(res, r.DiscoveryImage); err != nil {
			return fmt.Errorf("patch discovery image: %w", err)
		}
		if err := patchDiscoveryArgs(res, r.AITenantNamespace, r.GatewayNamespace); err != nil {
			return fmt.Errorf("patch discovery args: %w", err)
		}
		patchDiscoveryReplicas(res, r.DiscoveryReplicas)

		if err := controllerutil.SetControllerReference(&cfg, res, r.Scheme); err != nil {
			return fmt.Errorf("set controller reference on %s %s: %w", res.GetKind(), res.GetName(), err)
		}
		if err := r.Patch(ctx, res, client.Apply, client.ForceOwnership, client.FieldOwner("maas-controller")); err != nil {
			return fmt.Errorf("apply %s %s/%s: %w", res.GetKind(), res.GetNamespace(), res.GetName(), err)
		}
	}

	log.V(1).Info("discovery service resources applied", "count", len(resources))
	return nil
}

func (r *LifecycleReconciler) teardownDiscoveryResources(ctx context.Context, log logr.Logger, cfg *maasv1alpha1.Config, resources []unstructured.Unstructured) error {
	for _, resource := range resources {
		res := resource.DeepCopy()
		key := client.ObjectKeyFromObject(res)
		existing := &unstructured.Unstructured{}
		existing.SetGroupVersionKind(res.GroupVersionKind())

		if err := r.Get(ctx, key, existing); err != nil {
			if apierrors.IsNotFound(err) || apimeta.IsNoMatchError(err) {
				continue
			}
			return fmt.Errorf("get %s %s/%s before delete: %w", res.GetKind(), res.GetNamespace(), res.GetName(), err)
		}

		if !isOwnedByConfigOrController(existing, cfg.UID) {
			log.V(1).Info("skipping deletion of unowned discovery resource",
				"kind", res.GetKind(), "name", res.GetName(), "namespace", res.GetNamespace())
			continue
		}

		if err := r.Delete(ctx, existing); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return fmt.Errorf("delete %s %s/%s: %w", res.GetKind(), res.GetNamespace(), res.GetName(), err)
		}
		log.V(1).Info("deleted discovery resource (discovery disabled)",
			"kind", res.GetKind(), "name", res.GetName(), "namespace", res.GetNamespace())
	}
	return nil
}

func patchDiscoveryImage(res *unstructured.Unstructured, image string) error {
	if res.GetKind() != "Deployment" || res.GetName() != discoveryDeploymentName {
		return nil
	}

	if image == "" {
		image = DefaultMaaSDiscoveryImage
	}

	containers, found, err := unstructured.NestedSlice(res.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return fmt.Errorf("read containers in deployment: %w", err)
	}
	if !found {
		return errors.New("containers not found in maas-discovery deployment")
	}

	for i, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok || cm["name"] != discoveryContainerName {
			continue
		}
		cm["image"] = image
		containers[i] = cm
		return unstructured.SetNestedSlice(res.Object, containers, "spec", "template", "spec", "containers")
	}

	return errors.New("maas-discovery container not found in deployment")
}

func patchDiscoveryArgs(res *unstructured.Unstructured, aitenantNS, gatewayNS string) error {
	if res.GetKind() != "Deployment" || res.GetName() != discoveryDeploymentName {
		return nil
	}

	containers, found, err := unstructured.NestedSlice(res.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return fmt.Errorf("read containers in deployment: %w", err)
	}
	if !found {
		return nil
	}

	for i, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok || cm["name"] != discoveryContainerName {
			continue
		}
		rawArgs, ok := cm["args"]
		if !ok {
			return nil
		}
		args, ok := rawArgs.([]any)
		if !ok {
			return nil
		}

		for j, arg := range args {
			s, ok := arg.(string)
			if !ok {
				continue
			}
			if len(s) > len("--aitenant-namespace=") && s[:len("--aitenant-namespace=")] == "--aitenant-namespace=" {
				args[j] = "--aitenant-namespace=" + aitenantNS
			}
			if len(s) > len("--gateway-namespace=") && s[:len("--gateway-namespace=")] == "--gateway-namespace=" {
				args[j] = "--gateway-namespace=" + gatewayNS
			}
		}

		cm["args"] = args
		containers[i] = cm
		return unstructured.SetNestedSlice(res.Object, containers, "spec", "template", "spec", "containers")
	}

	return nil
}

func patchDiscoveryReplicas(res *unstructured.Unstructured, replicas *int32) {
	if replicas == nil || res.GetKind() != "Deployment" || res.GetName() != discoveryDeploymentName {
		return
	}
	_ = unstructured.SetNestedField(res.Object, int64(*replicas), "spec", "replicas")
}

func buildDiscoveryCrossNamespaceRBAC(controllerNS, aitenantNS, gatewayNS string) []unstructured.Unstructured {
	subject := rbacv1.Subject{
		Kind:      "ServiceAccount",
		Name:      discoveryDeploymentName,
		Namespace: controllerNS,
	}

	aiTenantRole := toUnstructured(&rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      discoveryDeploymentName,
			Namespace: aitenantNS,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{maasv1alpha1.GroupVersion.Group},
				Resources: []string{"aitenants"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	})

	aiTenantBinding := toUnstructured(&rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      discoveryDeploymentName,
			Namespace: aitenantNS,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     discoveryDeploymentName,
		},
		Subjects: []rbacv1.Subject{subject},
	})

	gatewayRole := toUnstructured(&rbacv1.Role{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "Role"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      discoveryDeploymentName,
			Namespace: gatewayNS,
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"gateway.networking.k8s.io"},
				Resources: []string{"gateways"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	})

	gatewayBinding := toUnstructured(&rbacv1.RoleBinding{
		TypeMeta: metav1.TypeMeta{APIVersion: "rbac.authorization.k8s.io/v1", Kind: "RoleBinding"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      discoveryDeploymentName,
			Namespace: gatewayNS,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: "rbac.authorization.k8s.io",
			Kind:     "Role",
			Name:     discoveryDeploymentName,
		},
		Subjects: []rbacv1.Subject{subject},
	})

	return []unstructured.Unstructured{aiTenantRole, aiTenantBinding, gatewayRole, gatewayBinding}
}

func toUnstructured(obj any) unstructured.Unstructured {
	data, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		panic(fmt.Sprintf("failed to convert to unstructured: %v", err))
	}
	return unstructured.Unstructured{Object: data}
}
