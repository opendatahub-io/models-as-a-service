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

	aigatewayv1alpha1 "github.com/opendatahub-io/ai-gateway-controller/api/aigateway/v1alpha1"
	aiguardrail "github.com/opendatahub-io/ai-gateway-controller/pkg/guardrail"
	maasguardrail "github.com/opendatahub-io/models-as-a-service/maas-common/pkg/guardrail"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	maasv1alpha1 "github.com/opendatahub-io/models-as-a-service/maas-controller/api/maas/v1alpha1"
	"github.com/opendatahub-io/models-as-a-service/maas-controller/pkg/platform/tenantreconcile"
)

type guardrailHeader struct {
	ID   string
	Name string
}

// aiGatewayResolutionChangedPredicate filters AIGuardrail updates to the fields
// AttachmentResolver uses when accepting a policy. Create and delete events pass
// through via predicate.Funcs.
type aiGatewayResolutionChangedPredicate struct {
	predicate.Funcs
}

func (aiGatewayResolutionChangedPredicate) Update(e event.UpdateEvent) bool {
	oldPolicy, oldOK := e.ObjectOld.(*aigatewayv1alpha1.AIGuardrail)
	newPolicy, newOK := e.ObjectNew.(*aigatewayv1alpha1.AIGuardrail)
	if !oldOK || !newOK {
		return true
	}
	if oldPolicy.Generation != newPolicy.Generation {
		return true
	}
	return guardrailResolutionStateFor(oldPolicy) != guardrailResolutionStateFor(newPolicy)
}

type guardrailResolutionState struct {
	bindingRevision    string
	acceptedFound      bool
	acceptedStatus     metav1.ConditionStatus
	acceptedGeneration int64
}

func guardrailResolutionStateFor(policy *aigatewayv1alpha1.AIGuardrail) guardrailResolutionState {
	state := guardrailResolutionState{bindingRevision: policy.Status.BindingRevision}
	for _, condition := range policy.Status.Conditions {
		if condition.Type != aiguardrail.ConditionAccepted {
			continue
		}
		state.acceptedFound = true
		state.acceptedStatus = condition.Status
		state.acceptedGeneration = condition.ObservedGeneration
		break
	}
	return state
}

// guardrailHeaders returns every check that may be selected for a tenant.
// It uses the same composition path as subscription selection so a rendered
// AuthPolicy cannot advertise checks that maas-api would reject.
func (r *MaaSAuthPolicyReconciler) guardrailHeaders(ctx context.Context, tenantNamespace string) ([]guardrailHeader, error) {
	tenantConfig := &maasv1alpha1.MaasTenantConfig{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: tenantNamespace, Name: maasv1alpha1.MaasTenantConfigInstanceName}, tenantConfig); err != nil {
		if apierrors.IsNotFound(err) {
			managed, err := r.isAITenantManagedNamespace(ctx, tenantNamespace)
			if err != nil {
				return nil, err
			}
			if managed {
				return nil, fmt.Errorf("MaasTenantConfig %s/%s is missing from AITenant-managed namespace", tenantNamespace, maasv1alpha1.MaasTenantConfigInstanceName)
			}
			return nil, nil
		}
		return nil, fmt.Errorf("get MaaSTenantConfig %s/%s: %w", tenantNamespace, maasv1alpha1.MaasTenantConfigInstanceName, err)
	}

	if !tenantreconcile.TenantUsesAITenantPlatformContext(tenantConfig) {
		return nil, nil
	}

	aitenant, err := r.owningAITenant(ctx, tenantConfig)
	if err != nil {
		return nil, err
	}

	var subscriptions maasv1alpha1.MaaSSubscriptionList
	if err := r.List(ctx, &subscriptions, client.InNamespace(tenantNamespace)); err != nil {
		return nil, fmt.Errorf("list MaaSSubscriptions in %s: %w", tenantNamespace, err)
	}

	resolver := aiguardrail.NewAttachmentResolver(&clientPolicyReader{client: r.Client})
	headers := map[string]guardrailHeader{}
	for i := range subscriptions.Items {
		subscription := &subscriptions.Items[i]
		for _, entry := range subscription.Spec.ModelRefs {
			model := &maasv1alpha1.MaaSModelRef{}
			if err := r.Get(ctx, client.ObjectKey{Namespace: entry.Namespace, Name: entry.Name}, model); err != nil {
				if apierrors.IsNotFound(err) {
					continue
				}
				return nil, fmt.Errorf("get MaaSModelRef %s/%s: %w", entry.Namespace, entry.Name, err)
			}
			if model.Status.ResolvedTenantRef != aitenant.Name {
				continue
			}

			_, plan, err := maasguardrail.ComposeSubscriptionModelPlan(
				ctx,
				resolver,
				tenantNamespace,
				&maasguardrail.TenantSelection{ResourceRef: resourceRef(aitenant), Guardrails: guardrailAttachments(aitenant.Spec.Guardrails)},
				&maasguardrail.TenantConfigSelection{ResourceRef: resourceRef(tenantConfig), Guardrails: guardrailAttachments(tenantConfig.Spec.Guardrails)},
				&maasguardrail.SubscriptionSelection{
					ResourceRef: resourceRef(subscription),
					Guardrails:  guardrailAttachments(subscription.Spec.Guardrails),
					ModelRefs:   subscriptionModelEntries(subscription.Spec.ModelRefs),
				},
				&maasguardrail.ModelSelection{ResourceRef: resourceRef(model), Guardrails: guardrailAttachments(model.Spec.Guardrails)},
				model.Namespace,
				model.Name,
			)
			if err != nil {
				return nil, fmt.Errorf("compose guardrails for subscription %s/%s and model %s/%s: %w", subscription.Namespace, subscription.Name, model.Namespace, model.Name, err)
			}

			for _, selected := range plan.Checks {
				id := aiguardrail.CheckID(selected.Policy.Namespace, selected.Policy.Name, selected.Check.Name)
				headers[id] = guardrailHeader{
					ID:   id,
					Name: aiguardrail.CheckHeaderName(selected.Policy.Namespace, selected.Policy.Name, selected.Check.Name),
				}
			}
		}
	}

	result := make([]guardrailHeader, 0, len(headers))
	for _, header := range headers {
		result = append(result, header)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result, nil
}

func (r *MaaSAuthPolicyReconciler) isDefaultTenantNamespace(namespace string) bool {
	defaultNamespace := r.TenantNamespace
	if defaultNamespace == "" {
		defaultNamespace = tenantreconcile.DefaultAITenantName
	}
	return namespace == defaultNamespace
}

func (r *MaaSAuthPolicyReconciler) isAITenantManagedNamespace(ctx context.Context, namespace string) (bool, error) {
	tenantNamespace := &corev1.Namespace{}
	if err := r.Get(ctx, client.ObjectKey{Name: namespace}, tenantNamespace); err != nil {
		return false, fmt.Errorf("get namespace %s while resolving missing MaasTenantConfig: %w", namespace, err)
	}
	return tenantNamespace.Labels[tenantreconcile.LabelManagedByAITenant] == "true", nil
}

func (r *MaaSAuthPolicyReconciler) owningAITenant(ctx context.Context, tenantConfig *maasv1alpha1.MaasTenantConfig) (*maasv1alpha1.AITenant, error) {
	annotations := tenantConfig.GetAnnotations()
	name := annotations[tenantreconcile.AnnotationAITenantName]
	namespace := annotations[tenantreconcile.AnnotationAITenantNamespace]
	switch {
	case name == "" && namespace == "":
		if !r.isDefaultTenantNamespace(tenantConfig.Namespace) {
			return nil, fmt.Errorf("MaasTenantConfig %s/%s is missing owning AITenant annotations", tenantConfig.Namespace, tenantConfig.Name)
		}
		name = tenantreconcile.DefaultAITenantName
		namespace = tenantreconcile.DefaultAITenantNamespace
	case name == "" || namespace == "":
		return nil, fmt.Errorf("MaasTenantConfig %s/%s has incomplete owning AITenant annotations", tenantConfig.Namespace, tenantConfig.Name)
	}

	aitenant := &maasv1alpha1.AITenant{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, aitenant); err != nil {
		return nil, fmt.Errorf("get owning AITenant %s/%s: %w", namespace, name, err)
	}
	if aitenant.Status.TenantNamespace != tenantConfig.Namespace {
		return nil, fmt.Errorf("AITenant %s/%s target namespace %q does not match MaaSTenantConfig namespace %q", aitenant.Namespace, aitenant.Name, aitenant.Status.TenantNamespace, tenantConfig.Namespace)
	}
	return aitenant, nil
}

func resourceRef(obj client.Object) maasguardrail.ResourceRef {
	return maasguardrail.ResourceRef{Namespace: obj.GetNamespace(), Name: obj.GetName(), UID: obj.GetUID()}
}

func guardrailAttachments(attachments []maasv1alpha1.GuardrailAttachment) []aiguardrail.Attachment {
	result := make([]aiguardrail.Attachment, 0, len(attachments))
	for _, attachment := range attachments {
		result = append(result, aiguardrail.Attachment{Name: attachment.Ref.Name, Checks: append([]string(nil), attachment.Checks...)})
	}
	return result
}

func subscriptionModelEntries(entries []maasv1alpha1.ModelSubscriptionRef) []maasguardrail.ModelEntrySelection {
	result := make([]maasguardrail.ModelEntrySelection, 0, len(entries))
	for _, entry := range entries {
		result = append(result, maasguardrail.ModelEntrySelection{
			Namespace:  entry.Namespace,
			Name:       entry.Name,
			Guardrails: guardrailAttachments(entry.Guardrails),
		})
	}
	return result
}

type clientPolicyReader struct {
	client client.Client
}

func (r *clientPolicyReader) GetPolicy(ctx context.Context, namespace, name string) (*aiguardrail.Policy, error) {
	policy := &aigatewayv1alpha1.AIGuardrail{}
	if err := r.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("policy %s/%s: %w", namespace, name, aiguardrail.ErrPolicyNotFound)
		}
		return nil, fmt.Errorf("policy %s/%s: %w: %w", namespace, name, aiguardrail.ErrReadFailure, err)
	}

	checks := make([]aiguardrail.Check, 0, len(policy.Spec.Checks))
	for index, check := range policy.Spec.Checks {
		checks = append(checks, aiguardrail.Check{Name: check.Name, Phases: append([]aigatewayv1alpha1.GuardrailPhase(nil), check.Phases...), SpecIndex: index})
	}
	conditions := make([]aiguardrail.PolicyCondition, 0, len(policy.Status.Conditions))
	for _, condition := range policy.Status.Conditions {
		conditions = append(conditions, aiguardrail.PolicyCondition{Type: condition.Type, Status: condition.Status == "True", ObservedGeneration: condition.ObservedGeneration})
	}
	return &aiguardrail.Policy{
		Namespace:       policy.Namespace,
		Name:            policy.Name,
		UID:             policy.UID,
		Generation:      policy.Generation,
		BindingRevision: policy.Status.BindingRevision,
		Conditions:      conditions,
		Checks:          checks,
	}, nil
}
