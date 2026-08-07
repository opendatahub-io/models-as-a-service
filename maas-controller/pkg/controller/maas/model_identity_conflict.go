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
	"fmt"
	"sort"
	"strings"

	"github.com/go-logr/logr"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
)

// ConditionModelIdentityUnique indicates whether this MaaSModelRef's resolved
// model alias (the canonical identity used for body-based routing, e.g.
// publishers/{namespace}/models/{name}) is unique among other MaaSModelRefs in
// the same namespace.
const ConditionModelIdentityUnique = "ModelIdentityUnique"

// findModelAliasConflicts returns the (sorted) names of other MaaSModelRefs in
// the same namespace whose ResolvedModelAlias collides with this model's alias.
//
// Body-based routing (BBR) selects a backend using the model identity carried
// in the request (e.g. the "model" field or x-gateway-model-name header) —
// it has no notion of which MaaSModelRef/subscription the caller intended.
// When two MaaSModelRefs resolve to the same alias (most commonly because two
// LLMInferenceServices in the same namespace declare the same spec.model.name),
// a request can be matched against the wrong backend and/or evaluated against
// the wrong subscription, causing spurious "subscription does not include
// model" denials or, worse, silently serving the wrong model.
func (r *MaaSModelRefReconciler) findModelAliasConflicts(ctx context.Context, model *maasv1alpha1.MaaSModelRef) ([]string, error) {
	if model.Status.ResolvedModelAlias == "" {
		return nil, nil
	}

	var siblings maasv1alpha1.MaaSModelRefList
	if err := r.List(ctx, &siblings, client.InNamespace(model.Namespace)); err != nil {
		return nil, fmt.Errorf("list MaaSModelRefs in namespace %s: %w", model.Namespace, err)
	}

	var conflicts []string
	for i := range siblings.Items {
		other := &siblings.Items[i]
		if other.Name == model.Name {
			continue
		}
		if other.Status.ResolvedModelAlias == model.Status.ResolvedModelAlias {
			conflicts = append(conflicts, other.Name)
		}
	}
	sort.Strings(conflicts)
	return conflicts, nil
}

// checkModelIdentityConflict detects sibling MaaSModelRefs that resolve to the
// same model alias, updates the ModelIdentityUnique condition, and emits a
// Warning/Normal event on transition (detected/resolved) so the conflict is
// visible via `kubectl describe`/`oc get events` without spamming an event on
// every reconcile.
func (r *MaaSModelRefReconciler) checkModelIdentityConflict(ctx context.Context, log logr.Logger, model *maasv1alpha1.MaaSModelRef) {
	if model.Status.ResolvedModelAlias == "" {
		apimeta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
			Type:               ConditionModelIdentityUnique,
			Status:             metav1.ConditionUnknown,
			Reason:             "AliasNotResolved",
			Message:            "Model alias has not been resolved yet; conflict status is unknown",
			ObservedGeneration: model.GetGeneration(),
		})
		return
	}

	prev := apimeta.FindStatusCondition(model.Status.Conditions, ConditionModelIdentityUnique)

	conflicts, err := r.findModelAliasConflicts(ctx, model)
	if err != nil {
		log.Error(err, "failed to check for model identity conflicts")
		apimeta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
			Type:               ConditionModelIdentityUnique,
			Status:             metav1.ConditionUnknown,
			Reason:             "ConflictCheckFailed",
			Message:            err.Error(),
			ObservedGeneration: model.GetGeneration(),
		})
		return
	}
	setModelIdentityCondition(model, conflicts)

	curr := apimeta.FindStatusCondition(model.Status.Conditions, ConditionModelIdentityUnique)
	if curr == nil || r.Recorder == nil {
		return
	}

	shouldEmitConflictEvent := curr.Status == metav1.ConditionFalse &&
		(prev == nil || prev.Status != curr.Status || prev.Message != curr.Message)
	if shouldEmitConflictEvent {
		r.Recorder.Eventf(model, "Warning", "ModelNameConflict",
			"Model identity %q is shared with %d other MaaSModelRef%s in this namespace: %s",
			model.Status.ResolvedModelAlias, len(conflicts), pluralS(len(conflicts)), strings.Join(conflicts, ", "))
	}

	shouldEmitResolvedEvent := curr.Status == metav1.ConditionTrue &&
		prev != nil && prev.Status == metav1.ConditionFalse
	if shouldEmitResolvedEvent {
		r.Recorder.Event(model, "Normal", "ModelNameConflictResolved",
			"Model identity is no longer shared with any other MaaSModelRef in this namespace")
	}
}

// setModelIdentityCondition updates the ModelIdentityUnique condition on a
// MaaSModelRef based on the sibling MaaSModelRef names that resolve to the
// same model alias. This is purely informational — it does not affect Phase —
// so an existing, previously-working deployment does not regress just because
// two models happen to share a name.
func setModelIdentityCondition(model *maasv1alpha1.MaaSModelRef, conflicts []string) {
	if len(conflicts) == 0 {
		apimeta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
			Type:               ConditionModelIdentityUnique,
			Status:             metav1.ConditionTrue,
			Reason:             "UniqueIdentity",
			Message:            "No other MaaSModelRef in this namespace resolves to the same model identity",
			ObservedGeneration: model.GetGeneration(),
		})
		return
	}

	msg := fmt.Sprintf(
		"Model identity %q is shared with %d other MaaSModelRef%s in namespace %s: %s. "+
			"Body-based routing selects a backend by model identity alone and cannot "+
			"disambiguate MaaSModelRefs that resolve to the same identity — requests may be "+
			"routed to the wrong backend or evaluated against the wrong subscription. "+
			"Give each model a unique spec.model.name to resolve this.",
		model.Status.ResolvedModelAlias, len(conflicts), pluralS(len(conflicts)), model.Namespace, strings.Join(conflicts, ", "),
	)
	if len(msg) > 1024 {
		msg = msg[:1021] + "..."
	}

	apimeta.SetStatusCondition(&model.Status.Conditions, metav1.Condition{
		Type:               ConditionModelIdentityUnique,
		Status:             metav1.ConditionFalse,
		Reason:             "ModelNameConflict",
		Message:            msg,
		ObservedGeneration: model.GetGeneration(),
	})
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
