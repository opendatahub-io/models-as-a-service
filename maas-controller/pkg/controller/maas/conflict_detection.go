/*
Copyright 2026.

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
	"sort"
	"strings"

	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

const ConditionConflictingAuthPolicy = "ConflictingAuthPolicy"

type conflictingPolicyInfo struct {
	Name          string
	Namespace     string
	HTTPRouteName string
	Model         string
	ModelNS       string
}

func (c conflictingPolicyInfo) String() string {
	return fmt.Sprintf("%s/%s (targets HTTPRoute %s, model %s/%s)", c.Namespace, c.Name, c.HTTPRouteName, c.ModelNS, c.Model)
}

// detectConflictingAuthPolicies finds non-MaaS Kuadrant AuthPolicies that target
// the same HTTPRoutes used by MaaS-governed models. These "rogue" policies can
// interfere with MaaS authentication/authorization enforcement.
func (r *MaaSAuthPolicyReconciler) detectConflictingAuthPolicies(ctx context.Context, log logr.Logger, policy *maasv1alpha1.MaaSAuthPolicy) ([]conflictingPolicyInfo, error) {
	var conflicts []conflictingPolicyInfo

	for _, ref := range policy.Spec.ModelRefs {
		httpRouteName, httpRouteNS, err := findHTTPRouteForModel(ctx, r.Client, ref.Namespace, ref.Name)
		if err != nil {
			if errors.Is(err, ErrModelNotFound) || errors.Is(err, ErrHTTPRouteNotFound) {
				continue
			}
			return nil, fmt.Errorf("resolve HTTPRoute for model %s/%s: %w", ref.Namespace, ref.Name, err)
		}

		modelConflicts, err := r.findConflictsForHTTPRoute(ctx, log, httpRouteName, httpRouteNS, ref.Name, ref.Namespace)
		if err != nil {
			return nil, err
		}
		conflicts = append(conflicts, modelConflicts...)
	}

	// Deduplicate: one rogue AuthPolicy can appear multiple times when
	// multiple modelRefs resolve to the same HTTPRoute.
	uniq := make(map[string]conflictingPolicyInfo, len(conflicts))
	for _, c := range conflicts {
		uniq[c.Namespace+"/"+c.Name] = c
	}
	conflicts = conflicts[:0]
	for _, c := range uniq {
		conflicts = append(conflicts, c)
	}

	sort.Slice(conflicts, func(i, j int) bool {
		ki := conflicts[i].Namespace + "/" + conflicts[i].Name
		kj := conflicts[j].Namespace + "/" + conflicts[j].Name
		return ki < kj
	})
	return conflicts, nil
}

// findConflictsForHTTPRoute lists all AuthPolicies in the given namespace and returns
// any that target the specified HTTPRoute but are not managed by MaaS.
func (r *MaaSAuthPolicyReconciler) findConflictsForHTTPRoute(ctx context.Context, log logr.Logger, httpRouteName, httpRouteNS, modelName, modelNS string) ([]conflictingPolicyInfo, error) {
	allAPs := &unstructured.UnstructuredList{}
	allAPs.SetGroupVersionKind(schema.GroupVersionKind{Group: "kuadrant.io", Version: "v1", Kind: "AuthPolicyList"})
	if err := r.List(ctx, allAPs, client.InNamespace(httpRouteNS)); err != nil {
		if apimeta.IsNoMatchError(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("list AuthPolicies in namespace %s: %w", httpRouteNS, err)
	}

	var conflicts []conflictingPolicyInfo
	for i := range allAPs.Items {
		ap := &allAPs.Items[i]

		if ap.GetLabels()["app.kubernetes.io/managed-by"] == "maas-controller" {
			continue
		}

		targetRefName, _, _ := unstructured.NestedString(ap.Object, "spec", "targetRef", "name")
		targetRefKind, _, _ := unstructured.NestedString(ap.Object, "spec", "targetRef", "kind")
		targetRefGroup, _, _ := unstructured.NestedString(ap.Object, "spec", "targetRef", "group")

		if targetRefName == httpRouteName &&
			targetRefKind == "HTTPRoute" &&
			targetRefGroup == "gateway.networking.k8s.io" {
			conflicts = append(conflicts, conflictingPolicyInfo{
				Name:          ap.GetName(),
				Namespace:     ap.GetNamespace(),
				HTTPRouteName: httpRouteName,
				Model:         modelName,
				ModelNS:       modelNS,
			})
			log.Info("detected conflicting AuthPolicy on MaaS auth surface",
				"conflictingPolicy", ap.GetNamespace()+"/"+ap.GetName(),
				"httpRoute", httpRouteNS+"/"+httpRouteName,
				"model", modelNS+"/"+modelName)
		}
	}
	return conflicts, nil
}

// setConflictingAuthPolicyCondition updates the ConflictingAuthPolicy condition
// on a MaaSAuthPolicy based on detected conflicts.
func setConflictingAuthPolicyCondition(policy *maasv1alpha1.MaaSAuthPolicy, conflicts []conflictingPolicyInfo) {
	if len(conflicts) == 0 {
		apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
			Type:               ConditionConflictingAuthPolicy,
			Status:             metav1.ConditionFalse,
			Reason:             "NoConflict",
			Message:            "No conflicting AuthPolicies detected on MaaS-managed HTTPRoutes",
			ObservedGeneration: policy.GetGeneration(),
		})
		return
	}

	var names []string
	for _, c := range conflicts {
		names = append(names, c.Namespace+"/"+c.Name)
	}
	msg := fmt.Sprintf("Detected %d non-MaaS AuthPolic%s targeting MaaS-managed HTTPRoutes: %s. "+
		"These policies may override MaaS authentication. "+
		"See MaaS troubleshooting documentation for remediation.",
		len(conflicts), pluralY(len(conflicts)), strings.Join(names, ", "))

	if len(msg) > 1024 {
		msg = msg[:1021] + "..."
	}

	apimeta.SetStatusCondition(&policy.Status.Conditions, metav1.Condition{
		Type:               ConditionConflictingAuthPolicy,
		Status:             metav1.ConditionTrue,
		Reason:             "ConflictDetected",
		Message:            msg,
		ObservedGeneration: policy.GetGeneration(),
	})
}

func pluralY(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}
